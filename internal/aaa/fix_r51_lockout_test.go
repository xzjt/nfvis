package aaa

// round51 评审 M12（登录失败表无界增长）与 L13（登录时间侧信道）的判据。
//
// M12：`recordFailure` 对任意用户名建条目，未达阈值时 lockedUntil 恒零值、清扫只删
// 「锁已过期」的条目 ⇒ 每个陌生用户名永驻内存，未认证攻击者可逐次换名撑爆 map。
// L13：「用户不存在」「用户无口令」两条分支不跑 PBKDF2，响应体相同而耗时不同 ⇒ 存在性预言机。

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
)

// TestFixR51LockoutTableReclaimsIdleSubThreshold 大量陌生用户名（均未达阈值）应被清扫回收，
// 但在空闲时限内不得提前回收（否则正常失败序列攒不到阈值）。
func TestFixR51LockoutTableReclaimsIdleSubThreshold(t *testing.T) {
	now, clockPtr := fakeClock()
	s := NewService(fakeSource{testConfig()}, now)

	// 数量取 30 而非上千：每个陌生用户名的登录现在都真的花掉一次 PBKDF2（L13 的时间等价），
	// 上千次会让本用例耗时数分钟；30 个不同用户名已足够证明「按空闲回收」的批量效果。
	const n = 30
	for i := 0; i < n; i++ {
		if _, err := s.Login(fmt.Sprintf("ghost-%d", i), "x"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("未知用户应 ErrInvalidCredentials: %v", err)
		}
	}
	if got := len(s.lockouts); got != n {
		t.Fatalf("前置：应记录 %d 个陌生用户名的失败计数，实得 %d", n, got)
	}

	// 空闲时限内：不回收（失败序列要能攒到阈值）
	*clockPtr = clockPtr.Add(lockoutIdleTTL - time.Minute)
	s.sweepLocked()
	if got := len(s.lockouts); got != n {
		t.Fatalf("空闲时限内不得回收失败计数（否则攒不到阈值），实得 %d", got)
	}

	// 超过空闲时限：全部回收
	*clockPtr = clockPtr.Add(2 * time.Minute)
	s.sweepLocked()
	if got := len(s.lockouts); got != 0 {
		t.Fatalf("闲置超过 %v 的陌生用户名条目应被回收，实得残留 %d", lockoutIdleTTL, got)
	}
}

// TestFixR51LockoutLockedEntrySweptAtExpiry 锁定条目恰在锁到期时被回收：锁定期内不得被
// 「空闲回收」清掉（那等于提前解锁），到期后回收且可再次登录。
func TestFixR51LockoutLockedEntrySweptAtExpiry(t *testing.T) {
	now, clockPtr := fakeClock()
	cfg := testConfig()
	cfg.System.Login.PasswordPolicy = &model.PasswordPolicy{LockoutThreshold: 3, LockoutMinutes: 10}
	s := NewService(fakeSource{cfg}, now)

	for i := 0; i < 3; i++ {
		if _, err := s.Login("admin", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("第 %d 次失败: %v", i+1, err)
		}
	}
	if st := s.lockouts["admin"]; st == nil || st.lockedUntil.IsZero() {
		t.Fatalf("达阈值应置锁定: %+v", st)
	}

	// 锁定未到期（5 分钟 < 10 分钟）：清扫不得动它
	*clockPtr = clockPtr.Add(5 * time.Minute)
	s.sweepLocked()
	if _, ok := s.lockouts["admin"]; !ok {
		t.Fatalf("锁定未到期的条目不得被回收（等于提前解锁）")
	}
	if _, err := s.Login("admin", "s3cret-Passw0rd!"); !errors.Is(err, ErrLocked) {
		t.Fatalf("锁定期内正确口令也应 ErrLocked: %v", err)
	}

	// 越过锁定窗口：回收，随后可登录
	*clockPtr = clockPtr.Add(6 * time.Minute) // 共 11 分钟
	s.sweepLocked()
	if _, ok := s.lockouts["admin"]; ok {
		t.Fatalf("锁定到期应回收条目")
	}
	if _, err := s.Login("admin", "s3cret-Passw0rd!"); err != nil {
		t.Fatalf("锁定到期后应可登录: %v", err)
	}
}

// TestFixR51FailureSeriesStillLocksWithinIdleTTL 间隔小于空闲时限的失败序列仍应达阈值锁定
// （回收不得削弱锁定语义）。
func TestFixR51FailureSeriesStillLocksWithinIdleTTL(t *testing.T) {
	now, clockPtr := fakeClock()
	cfg := testConfig()
	cfg.System.Login.PasswordPolicy = &model.PasswordPolicy{LockoutThreshold: 5, LockoutMinutes: 10}
	s := NewService(fakeSource{cfg}, now)

	for i := 0; i < 5; i++ {
		*clockPtr = clockPtr.Add(5 * time.Minute) // 每次间隔 5 分钟（< 30 分钟空闲时限）
		if _, err := s.Login("admin", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("第 %d 次失败应 ErrInvalidCredentials: %v", i+1, err)
		}
	}
	if _, err := s.Login("admin", "s3cret-Passw0rd!"); !errors.Is(err, ErrLocked) {
		t.Fatalf("间隔小于空闲时限的失败序列应仍达阈值锁定: %v", err)
	}
}

// TestFixR51DummyHashIsWellFormedAndCostsSame 哑哈希必须是**合法**的 pbkdf2 串且迭代数与真实
// 哈希相同——否则 VerifyPassword 会提前返回（不做 KDF），时间等价就落空了。
func TestFixR51DummyHashIsWellFormedAndCostsSame(t *testing.T) {
	parts := strings.Split(dummyPasswordHash, "$")
	if len(parts) != 5 || parts[0] != "pbkdf2" || parts[1] != "sha256" {
		t.Fatalf("哑哈希格式不合法（VerifyPassword 会提前返回而不做 KDF）: %q", dummyPasswordHash)
	}
	if parts[2] != strconv.Itoa(pbkdf2Iterations) {
		t.Fatalf("哑哈希迭代数须与真实哈希相同（%d），实得 %s", pbkdf2Iterations, parts[2])
	}
	if salt, err := base64.RawStdEncoding.DecodeString(parts[3]); err != nil || len(salt) == 0 {
		t.Fatalf("哑哈希盐不可解码: %v (%q)", err, parts[3])
	}
	if want, err := base64.RawStdEncoding.DecodeString(parts[4]); err != nil || len(want) != pbkdf2KeyLen {
		t.Fatalf("哑哈希目标须可解码且长度 %d: %v len=%d", pbkdf2KeyLen, err, len(want))
	}
	if VerifyPassword(dummyPasswordHash, "任何口令") {
		t.Fatalf("哑哈希不得校验通过任何口令")
	}
}

// TestFixR51LoginSpendsVerifyOnFastPaths 「用户不存在」与「用户无口令」两条快路径都必须做一次
// 等价哈希比较（用固定哑哈希）；真实用户仍走自己的哈希。
func TestFixR51LoginSpendsVerifyOnFastPaths(t *testing.T) {
	now, _ := fakeClock()
	cfg := testConfig()
	cfg.System.Login.Users = append(cfg.System.Login.Users,
		model.LoginUserConfig{Name: "nopass", Class: ClassOperator}) // 有账号、无口令
	s := NewService(fakeSource{cfg}, now)

	orig := verifyPasswordFn
	defer func() { verifyPasswordFn = orig }()
	var calls []string
	verifyPasswordFn = func(hash, _ string) bool {
		calls = append(calls, hash)
		return false
	}

	// ① 用户不存在：一次哑哈希比较
	calls = nil
	if _, err := s.Login("ghost", "x"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("未知用户应 ErrInvalidCredentials: %v", err)
	}
	if len(calls) != 1 || calls[0] != dummyPasswordHash {
		t.Fatalf("未知用户应恰好做一次哑哈希比较，实得 %v", calls)
	}

	// ② 用户无口令：错误值保持原样，但同样花掉一次哑哈希比较
	calls = nil
	_, err := s.Login("nopass", "x")
	if err == nil || !strings.Contains(err.Error(), "未设置口令") {
		t.Fatalf("无口令账号应返回「未设置口令」错误（值不变）: %v", err)
	}
	if len(calls) != 1 || calls[0] != dummyPasswordHash {
		t.Fatalf("无口令账号也应恰好做一次哑哈希比较，实得 %v", calls)
	}

	// ③ 真实用户 + 错口令：走自己的哈希（不是哑哈希），证明判据能区分两条路径
	calls = nil
	if _, err := s.Login("admin", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("真实用户错口令应 ErrInvalidCredentials: %v", err)
	}
	if len(calls) != 1 || calls[0] == dummyPasswordHash {
		t.Fatalf("真实用户应校验自己的哈希，实得 %v", calls)
	}
}
