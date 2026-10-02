package network

// 决策 #348：nfvisd 启动时确保 VPP 运行（Manager.EnsureRunning）的单测。
//
// 用注入的假拉起执行器与假就绪探针，不碰真 systemctl、不打真底座。锁住三条硬口径：
// ① VPP 未运行 → 由 nfvisd 拉起一次；② VPP 已在运行 → 零动作（不滥拉）；
// ③ 拉起失败 / 拉起后超时未就绪 → 如实返回错误（供上层告警，绝不假绿），且每次启动只尝试一次。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeStarter 记录拉起调用（不碰 systemctl）。
type fakeStarter struct {
	calls int
	err   error
}

func (f *fakeStarter) Start(context.Context) error {
	f.calls++
	return f.err
}

// probeSeq 按调用次序返回预置的探测结果（用尽后固定返回最后一个）。
func probeSeq(results ...error) func(context.Context) error {
	i := 0
	return func(context.Context) error {
		if i >= len(results) {
			return results[len(results)-1]
		}
		r := results[i]
		i++
		return r
	}
}

// newEnsureManager 构造带短等待上限的管理器（探针与拉起实现由各用例注入）。
func newEnsureManager(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(testConfig(), &fakeDialer{})
	m.ensureTimeout = 30 * time.Millisecond
	m.ensureInterval = 2 * time.Millisecond
	return m
}

var errVPPDown = errors.New("VPP API 套接字不可用：no such file")

func TestEnsureRunningStartsVPPWhenNotRunning(t *testing.T) {
	m := newEnsureManager(t)
	st := &fakeStarter{}
	m.SetEnsureStarter(st)
	// 首次探测（拉起前）未运行，拉起后探测成功。
	m.SetEnsureProbe(probeSeq(errVPPDown, nil))

	if err := m.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("拉起成功后应返回 nil: %v", err)
	}
	if st.calls != 1 {
		t.Fatalf("VPP 未运行时应拉起一次，实际 %d 次", st.calls)
	}
}

func TestEnsureRunningNoopWhenAlreadyRunning(t *testing.T) {
	m := newEnsureManager(t)
	st := &fakeStarter{}
	m.SetEnsureStarter(st)
	m.SetEnsureProbe(probeSeq(nil)) // 拉起前探测即成功：VPP 已在运行

	if err := m.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("VPP 已在运行应返回 nil: %v", err)
	}
	if st.calls != 0 {
		t.Fatalf("VPP 已在运行不该拉起，实际 %d 次", st.calls)
	}
}

func TestEnsureRunningNoopWhenManagerConnected(t *testing.T) {
	m := newEnsureManager(t)
	st := &fakeStarter{}
	m.SetEnsureStarter(st)
	// 管理器已有可用连接：直接视为在运行，探针都不该被用来判定。
	m.mu.Lock()
	m.state = StateConnected
	m.mu.Unlock()
	m.SetEnsureProbe(probeSeq(errVPPDown))

	if err := m.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("已有连接应返回 nil: %v", err)
	}
	if st.calls != 0 {
		t.Fatalf("已有可用连接不该拉起，实际 %d 次", st.calls)
	}
}

func TestEnsureRunningFailsWhenStartFails(t *testing.T) {
	m := newEnsureManager(t)
	st := &fakeStarter{err: errors.New("systemctl start vpp: exit status 1")}
	m.SetEnsureStarter(st)
	m.SetEnsureProbe(probeSeq(errVPPDown)) // 始终未运行

	err := m.EnsureRunning(context.Background())
	if err == nil {
		t.Fatal("拉起失败必须返回错误（供告警，不得假绿）")
	}
	for _, want := range []string{"systemctl", "journalctl -u vpp"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误应带手查路径，缺 %q：%v", want, err)
		}
	}
	if st.calls != 1 {
		t.Fatalf("每次启动只尝试一次，实际 %d 次", st.calls)
	}
}

func TestEnsureRunningFailsWhenNotReadyInTime(t *testing.T) {
	m := newEnsureManager(t)
	st := &fakeStarter{}
	m.SetEnsureStarter(st)
	m.SetEnsureProbe(probeSeq(errVPPDown, errVPPDown, errVPPDown, errVPPDown,
		errVPPDown, errVPPDown, errVPPDown, errVPPDown))

	start := time.Now()
	err := m.EnsureRunning(context.Background())
	if err == nil {
		t.Fatal("拉起后超时未就绪必须返回错误")
	}
	if st.calls != 1 {
		t.Fatalf("拉起应只一次，实际 %d 次", st.calls)
	}
	if !strings.Contains(err.Error(), "未在") {
		t.Fatalf("应说明未在窗口内就绪：%v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("有界等待超时不应太久，实际 %v", d)
	}
}
