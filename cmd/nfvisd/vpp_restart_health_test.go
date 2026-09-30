package main

// R84-4：`request vpp restart` 的起后健康校验（vppController.Restart/waitHealthy/precheckHugepages）。
//
// 缺陷面：`systemctl restart vpp` 返回 0 只说明动作被接受；VPP 起不来时（实测把 2M 大页池
// 运行期置 0 后 VPP 直接 SEGV）旧实现仍返回 nil，CLI 报「已按 committed 配置重启 VPP」——
// 操作者看到成功、数据面其实全挂。本文件锁住「没起来必须报错」「起来了才报成功」。

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// ---------- waitHealthy：有界等待 + 探针语义 ----------

func TestWaitHealthyTimesOutWhenProbeKeepsFailing(t *testing.T) {
	c := &vppController{
		healthTimeout:  60 * time.Millisecond,
		healthInterval: 5 * time.Millisecond,
		probe:          func(context.Context) error { return errors.New("connection refused") },
	}
	start := time.Now()
	err := c.waitHealthy(context.Background(), 0)
	if err == nil {
		t.Fatal("探针一直失败必须报错（否则又是「界面报成功、数据面全挂」）")
	}
	for _, want := range []string{"未起来", "binary API 未连上", "connection refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误文案应说明 VPP 未起来/未连上并带上原因，缺 %q: %v", want, err)
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("等待应有界，实际耗时 %v", d)
	}
}

func TestWaitHealthySucceedsWhenProbeRecovers(t *testing.T) {
	calls := 0
	c := &vppController{
		healthTimeout:  time.Second,
		healthInterval: 5 * time.Millisecond,
		probe: func(context.Context) error {
			calls++
			if calls < 3 {
				return errors.New("尚未就绪")
			}
			return nil
		},
	}
	if err := c.waitHealthy(context.Background(), 0); err != nil {
		t.Fatalf("探针第 3 次成功应判定健康: %v", err)
	}
	if calls != 3 {
		t.Fatalf("应在成功当次立即返回，实际探测 %d 次", calls)
	}
}

func TestWaitHealthyImmediateSuccess(t *testing.T) {
	c := &vppController{probe: func(context.Context) error { return nil }}
	start := time.Now()
	if err := c.waitHealthy(context.Background(), 0); err != nil {
		t.Fatalf("探针立即成功不该报错: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("成功路径不该等待，实际耗时 %v", d)
	}
}

// 探针自身卡住（govpp 连接路径不可取消）时，等待仍必须在上限内结束。
func TestWaitHealthyBoundsHungProbe(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	c := &vppController{
		healthTimeout:  50 * time.Millisecond,
		healthInterval: 5 * time.Millisecond,
		probe:          func(context.Context) error { <-release; return errors.New("仍然连不上") },
	}
	start := time.Now()
	err := c.waitHealthy(context.Background(), 0)
	if err == nil {
		t.Fatal("探针卡住时必须按上限报错，不能一直等")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("等待应有界，实际耗时 %v", d)
	}
}

// ---------- precheckHugepages：重启前按配置需求预检大页池 ----------

func TestPrecheckHugepagesRejectsEmptyPool(t *testing.T) {
	c := &vppController{poolPages: func(string) (int, bool) { return 0, true }}
	vpp := &model.VppConfig{Memory: &model.VppMemory{HugepagePreference: "2M"}}
	err := c.precheckHugepages(vpp)
	if err == nil {
		t.Fatal("配置偏好 2M 而 2M 池为 0 时应拒绝重启（VPP 必然起不来）")
	}
	for _, want := range []string{"未重启", "2M", "hugepages-2048kB"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("拒绝文案应指明池与文件路径，缺 %q: %v", want, err)
		}
	}
}

func TestPrecheckHugepagesPassesWhenPoolHasPages(t *testing.T) {
	c := &vppController{poolPages: func(string) (int, bool) { return 768, true }}
	vpp := &model.VppConfig{Memory: &model.VppMemory{HugepagePreference: "2M"}}
	if err := c.precheckHugepages(vpp); err != nil {
		t.Fatalf("池非空不该拒绝: %v", err)
	}
}

func TestPrecheckHugepagesSkipsWhenUnknown(t *testing.T) {
	cases := []struct {
		name string
		c    *vppController
		vpp  *model.VppConfig
	}{
		{"未声明偏好", &vppController{poolPages: func(string) (int, bool) { return 0, true }}, &model.VppConfig{}},
		{"无 vpp 配置", &vppController{poolPages: func(string) (int, bool) { return 0, true }}, nil},
		{"池读不到（非 sysfs 环境）", &vppController{poolPages: func(string) (int, bool) { return 0, false }},
			&model.VppConfig{Memory: &model.VppMemory{HugepagePreference: "2M"}}},
	}
	for _, tc := range cases {
		if err := tc.c.precheckHugepages(tc.vpp); err != nil {
			t.Errorf("%s：不该拒绝，实际 %v", tc.name, err)
		}
	}
}

func TestReadHugepagePoolUnknownSize(t *testing.T) {
	if path := hugepageSysfsPath("4M"); path != "" {
		t.Fatalf("未识别的页大小不该给出路径: %s", path)
	}
	if _, ok := readHugepagePool("4M"); ok {
		t.Fatal("未识别的页大小应报「读不到」，而不是当成池为 0")
	}
}

// ---------- Restart：装配层的顺序与结果（预检 → 落地重启 → 起后健康校验） ----------

// fakeRestarter 记录重启调用（不碰 systemctl）。
type fakeRestarter struct {
	calls int
	err   error
	hook  func() // 每次重启动作时执行（测试用它模拟「重启导致管理器重连」）
}

func (f *fakeRestarter) Restart(context.Context) error {
	f.calls++
	if f.hook != nil {
		f.hook()
	}
	return f.err
}

// newTestVppController 构造真实配置引擎 + 假落地器（写文件与重启都注入，不碰底座）。
func newTestVppController(t *testing.T, cfg model.Config, c *vppController) *fakeRestarter {
	t.Helper()
	store, err := config.OpenStore(filepath.Join(t.TempDir(), "nfvis.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("序列化配置: %v", err)
	}
	if _, err := store.AppendRevision(data, time.Now(), "test", "test"); err != nil {
		t.Fatalf("写入 committed 配置: %v", err)
	}
	engine, err := config.NewEngine(store, orchestrator.NewNoopApplier(), config.Options{})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(engine.Close)
	restarter := &fakeRestarter{}
	c.engine = engine
	c.applier = &network.Applier{
		Path:           filepath.Join(t.TempDir(), "startup.conf"),
		Write:          func(string, []byte) error { return nil },
		Restarter:      restarter,
		RestartOnApply: true,
	}
	return restarter
}

func TestRestartFailsWhenVPPDoesNotComeUp(t *testing.T) {
	c := &vppController{
		healthTimeout:  40 * time.Millisecond,
		healthInterval: 5 * time.Millisecond,
		probe:          func(context.Context) error { return errors.New("VPP API socket file does not exist") },
	}
	restarter := newTestVppController(t, model.Config{}, c)

	err := c.Restart(context.Background(), nil)
	if err == nil {
		t.Fatal("重启动作成功但 VPP 没起来时必须报错（R84-4 的假成功面）")
	}
	if !strings.Contains(err.Error(), "未起来") {
		t.Fatalf("错误应说明 VPP 未起来: %v", err)
	}
	if restarter.calls != 1 {
		t.Fatalf("重启动作应已执行一次（错误来自起后校验），实际 %d 次", restarter.calls)
	}
}

func TestRestartSucceedsWhenVPPComesUp(t *testing.T) {
	c := &vppController{probe: func(context.Context) error { return nil }}
	restarter := newTestVppController(t, model.Config{}, c)

	if err := c.Restart(context.Background(), nil); err != nil {
		t.Fatalf("VPP 起来了就该返回成功: %v", err)
	}
	if restarter.calls != 1 {
		t.Fatalf("应执行一次重启，实际 %d 次", restarter.calls)
	}
}

func TestRestartRefusesBeforeRestartWhenHugepagePoolEmpty(t *testing.T) {
	c := &vppController{
		poolPages: func(string) (int, bool) { return 0, true },
		probe:     func(context.Context) error { return nil },
	}
	cfg := model.Config{Vpp: &model.VppConfig{Memory: &model.VppMemory{HugepagePreference: "2M"}}}
	restarter := newTestVppController(t, cfg, c)

	err := c.Restart(context.Background(), nil)
	if err == nil {
		t.Fatal("大页池为 0 时应拒绝重启")
	}
	if !strings.Contains(err.Error(), "2M") {
		t.Fatalf("拒绝文案应指明页大小: %v", err)
	}
	if restarter.calls != 0 {
		t.Fatalf("预检拒绝时不该真的重启数据面，实际 %d 次", restarter.calls)
	}
}

// ---------- 决策 #315：重启返回即代表「查询可用」（等管理器换成新连接） ----------

// 裸探针已通但连接管理器会话尚未重建：waitHealthy 不能就此返回成功，
// 要等它重建（否则操作者紧接着的查询会撞旧会话的 broken pipe）。
func TestWaitHealthyWaitsForFreshSessionRebuild(t *testing.T) {
	checks := 0
	c := &vppController{
		healthTimeout:  time.Second,
		healthInterval: 2 * time.Millisecond,
		probe:          func(context.Context) error { return nil },
		connReady: func(uint64) bool {
			checks++
			return checks >= 3 // 前两次仍报「旧会话」
		},
	}
	if err := c.waitHealthy(context.Background(), 0); err != nil {
		t.Fatalf("会话重建后应判定健康: %v", err)
	}
	if checks < 3 {
		t.Fatalf("不能在会话重建前就返回成功，实际就绪检查 %d 次", checks)
	}
}

// 探针通（VPP 进程活着）但会话始终没重建：如实报「数据面连接在重启窗口内不可用」+ 指引，
// 不得返回成功、也不得显示为空结果。
func TestWaitHealthyReportsStaleSessionHonestly(t *testing.T) {
	c := &vppController{
		healthTimeout:  40 * time.Millisecond,
		healthInterval: 5 * time.Millisecond,
		probe:          func(context.Context) error { return nil },
		connReady:      func(uint64) bool { return false },
	}
	err := c.waitHealthy(context.Background(), 0)
	if err == nil {
		t.Fatal("VPP 已起来但会话未重建时必须报错（否则返回成功即撞 broken pipe）")
	}
	for _, want := range []string{"数据面连接未在", "此刻查询会报", "request vpp restart", "show vpp"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误文案应含 %q，实得：%v", want, err)
		}
	}
}

// Restart 必须把「重启前的连接世代」抓在重启动作之前，并在返回前等到世代前进：
// 若管理器会话始终没换新，即便 VPP 进程活着也要报错（决策 #315 的核心口径）。
func TestRestartFailsWhenManagerSessionNeverRebuilds(t *testing.T) {
	c := &vppController{
		healthTimeout:  40 * time.Millisecond,
		healthInterval: 5 * time.Millisecond,
		probe:          func(context.Context) error { return nil },
		connGen:        func() uint64 { return 7 },
		connReady:      func(since uint64) bool { return since != 7 }, // 世代永不前进
	}
	restarter := newTestVppController(t, model.Config{}, c)
	err := c.Restart(context.Background(), nil)
	if err == nil {
		t.Fatal("会话未重建时必须报错，不能报成功")
	}
	if !strings.Contains(err.Error(), "数据面连接未在") {
		t.Fatalf("错误应说明数据面连接未恢复: %v", err)
	}
	if restarter.calls != 1 {
		t.Fatalf("重启动作应已执行一次，实际 %d 次", restarter.calls)
	}
}

// 正例：重启动作触发管理器重连（世代前进），Restart 等到新会话才返回成功——
// 「返回后立即查询」因此不会撞旧会话。
func TestRestartSucceedsAfterManagerSessionRebuilds(t *testing.T) {
	gen := uint64(7)
	c := &vppController{
		healthTimeout:  time.Second,
		healthInterval: 2 * time.Millisecond,
		probe:          func(context.Context) error { return nil },
		connGen:        func() uint64 { return gen },
		connReady:      func(since uint64) bool { return gen > since },
	}
	restarter := newTestVppController(t, model.Config{}, c)
	restarter.hook = func() { gen = 8 } // 模拟重启导致管理器重连

	if err := c.Restart(context.Background(), nil); err != nil {
		t.Fatalf("管理器重连后 Restart 应成功: %v", err)
	}
	if restarter.calls != 1 {
		t.Fatalf("应执行一次重启，实际 %d 次", restarter.calls)
	}
	if !c.sessionReady(7) {
		t.Fatal("返回后会话应为新连接（sessionReady(重启前世代) 应 true）")
	}
}
