package network

// 决策 #348：nfvisd 启动时确保 VPP 运行（Manager.EnsureRunning）的单测。
//
// 用注入的假发起执行器与假探针，不碰真 systemctl、不打真底座。锁住硬口径：
// ① VPP 未运行 → 发起拉起一次；② VPP 已在运行 → 零动作（不滥拉）；
// ③ 发起失败 → 如实返回错误（供上层告警，绝不假绿），且每次启动只尝试一次；
// ④ **启动序列绝不被阻塞**——探针卡住或发起动作迟迟不返回，EnsureRunning 仍在上限内返回
//    （这是真机 dev39 回归的守护：绝不能再在启动路径上同步等待 VPP）；
// ⑤ 发起参数确为 `--no-block`（提交 job 即返回，不等 job 完成）。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeStarter 记录发起调用（不碰 systemctl）。
type fakeStarter struct {
	calls int
	err   error
}

func (f *fakeStarter) Start(context.Context) error {
	f.calls++
	return f.err
}

// hangingStarter 模拟「发起动作迟迟不返回」：阻塞直到 release 或 ctx 取消。
type hangingStarter struct{ release chan struct{} }

func (s *hangingStarter) Start(ctx context.Context) error {
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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

// newEnsureManager 构造带短上限的管理器（探针与发起实现由各用例注入），
// 使「卡住」用例在毫秒级内即可判出，无需真等默认的秒级上限。
func newEnsureManager(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(testConfig(), &fakeDialer{})
	m.ensureProbeTimeout = 20 * time.Millisecond
	m.ensureStartTimeout = 20 * time.Millisecond
	return m
}

var errVPPDown = errors.New("VPP API 套接字不可用：no such file")

func TestEnsureRunningStartsVPPWhenNotRunning(t *testing.T) {
	m := newEnsureManager(t)
	st := &fakeStarter{}
	m.SetEnsureStarter(st)
	m.SetEnsureProbe(probeSeq(errVPPDown)) // 拉起前探测未运行

	if err := m.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("发起拉起后应返回 nil: %v", err)
	}
	if st.calls != 1 {
		t.Fatalf("VPP 未运行时应发起拉起一次，实际 %d 次", st.calls)
	}
}

func TestEnsureRunningNoopWhenAlreadyRunning(t *testing.T) {
	m := newEnsureManager(t)
	st := &fakeStarter{}
	m.SetEnsureStarter(st)
	m.SetEnsureProbe(probeSeq(nil)) // 探测即成功：VPP 已在运行

	if err := m.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("VPP 已在运行应返回 nil: %v", err)
	}
	if st.calls != 0 {
		t.Fatalf("VPP 已在运行不该发起拉起，实际 %d 次", st.calls)
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
		t.Fatalf("已有可用连接不该发起拉起，实际 %d 次", st.calls)
	}
}

func TestEnsureRunningFailsWhenStartFails(t *testing.T) {
	m := newEnsureManager(t)
	st := &fakeStarter{err: errors.New("systemctl start --no-block vpp: exit status 1")}
	m.SetEnsureStarter(st)
	m.SetEnsureProbe(probeSeq(errVPPDown)) // 始终未运行

	err := m.EnsureRunning(context.Background())
	if err == nil {
		t.Fatal("发起拉起失败必须返回错误（供告警，不得假绿）")
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

// 回归（真机 dev39）：探针卡住（govpp 连接路径不接收 context）时，EnsureRunning 必须
// 在硬超时内返回——启动序列绝不能被「探一下 VPP 是否在运行」拖住。
func TestEnsureRunningDoesNotBlockOnHangingProbe(t *testing.T) {
	m := newEnsureManager(t)
	st := &fakeStarter{}
	m.SetEnsureStarter(st)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	m.SetEnsureProbe(func(context.Context) error { <-release; return errVPPDown })

	start := time.Now()
	err := m.EnsureRunning(context.Background())
	if err != nil {
		t.Fatalf("探针卡住时 EnsureRunning 不该报错（应视为未运行后发起拉起）: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("探针卡住时启动序列仍须快速完成，实际耗时 %v", d)
	}
	if st.calls != 1 {
		t.Fatalf("探针超时视为未运行，应发起拉起一次，实际 %d 次", st.calls)
	}
}

// 回归（真机 dev39）：发起动作迟迟不返回时，EnsureRunning 必须有界放弃等待、立即返回，
// 绝不把启动序列卡在 systemctl 上（真实实现用 --no-block，本用例防未来改回阻塞实现）。
func TestEnsureRunningDoesNotBlockOnHangingStarter(t *testing.T) {
	m := newEnsureManager(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	m.SetEnsureStarter(&hangingStarter{release: release})
	m.SetEnsureProbe(probeSeq(errVPPDown)) // VPP 未运行

	start := time.Now()
	err := m.EnsureRunning(context.Background())
	if err != nil {
		t.Fatalf("发起动作卡住不该让 EnsureRunning 报错（有界放弃等待即可）: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("发起动作卡住时启动序列仍须快速完成，实际耗时 %v", d)
	}
}

// 发起参数必须含 --no-block：systemctl 提交启动 job 后立即返回，不等 job 完成。
func TestSystemctlStartArgsUsesNoBlock(t *testing.T) {
	args := systemctlStartArgs("vpp")
	want := map[string]bool{"start": true, "--no-block": true, "vpp": true}
	if len(args) != len(want) {
		t.Fatalf("systemctlStartArgs 参数数量不符：%v", args)
	}
	for _, a := range args {
		if !want[a] {
			t.Fatalf("意外参数 %q：%v", a, args)
		}
		delete(want, a)
	}
	if len(want) != 0 {
		t.Fatalf("缺少参数：%v（args=%v）", want, args)
	}
}
