package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// fakeVM VMRuntime 测试替身。
type fakeVM struct {
	states  map[string]string
	actions []string
	// startProbe 非 nil 时 StartVMChecked 返回它（决策 #311 的失败诊断测试）；
	// nil 表示正常启动（OK、无诊断字段）。
	startProbe *orchestrator.VMStartProbe
	startErr   error
	// restartErr 非 nil 时 RestartVM 返回它（决策 #314 的 restart off→start 分支测试）。
	restartErr error
}

func newFakeVM() *fakeVM { return &fakeVM{states: map[string]string{}} }

// RefreshSeed 记录调用（决策 #114：启动/重启前重建 seed）。
func (f *fakeVM) RefreshSeed(_ context.Context, vm model.VMFunction) error {
	f.actions = append(f.actions, "refresh-seed:"+vm.Name)
	return nil
}

func (f *fakeVM) StartVM(_ context.Context, name string) error {
	f.actions = append(f.actions, "start:"+name)
	f.states[name] = orchestrator.VMStateRunning
	return nil
}

// StartVMChecked 决策 #311：默认正常启动（OK）；测试可注入 startProbe 模拟非预期态。
func (f *fakeVM) StartVMChecked(_ context.Context, name string) (orchestrator.VMStartProbe, error) {
	f.actions = append(f.actions, "start-checked:"+name)
	if f.startErr != nil {
		return orchestrator.VMStartProbe{}, f.startErr
	}
	if f.startProbe != nil {
		f.states[name] = f.startProbe.State
		return *f.startProbe, nil
	}
	f.states[name] = orchestrator.VMStateRunning
	return orchestrator.VMStartProbe{OK: true, State: orchestrator.VMStateRunning}, nil
}
func (f *fakeVM) StopVM(_ context.Context, name string) error {
	f.actions = append(f.actions, "stop:"+name)
	f.states[name] = orchestrator.VMStateShutoff
	return nil
}
func (f *fakeVM) RestartVM(_ context.Context, name string) error {
	f.actions = append(f.actions, "restart:"+name)
	if f.restartErr != nil {
		return f.restartErr
	}
	f.states[name] = orchestrator.VMStateRunning
	return nil
}
func (f *fakeVM) VMState(_ context.Context, name string) (string, error) {
	if s, ok := f.states[name]; ok {
		return s, nil
	}
	return orchestrator.VMStateShutoff, nil
}

// seedVMPool 建好资源池，使 VM 校验（大口页池/隔离核）通过。
func seedVMPool(t *testing.T, ts *httptest.Server, token string) {
	t.Helper()
	pools := map[string]any{
		"hugepages": []map[string]any{{"page_size": "1G", "count": 8}},
		"cpu":       map[string]any{"isolated_cores": []int{4, 5, 6, 7}},
	}
	status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/resource-pools", token, pools,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("PUT resource-pools: %d %s", status, data)
	}
}

func vmBody(name string) map[string]any {
	return map[string]any{
		"name":   name,
		"image":  "img.qcow2",
		"vcpu":   map[string]any{"count": 1},
		"memory": map[string]any{"size_mb": 1024, "hugepage_size": "1G"},
	}
}

func TestVMLifecycleEndpoints(t *testing.T) {
	fake := newFakeVM()
	ts := newTestServerOpts(t, Options{VM: fake})
	token := loginAdmin(t, ts)
	seedVMPool(t, ts, token)

	// 创建（直提）
	status, hdr, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		vmBody("fw-vm"), map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusCreated || hdr.Get("X-NFVIS-Committed") != "true" {
		t.Fatalf("创建 VM: %d %s", status, data)
	}
	// 重复创建 → 409
	status, _, _ = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		vmBody("fw-vm"), map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusConflict {
		t.Fatalf("重复创建应 409: %d", status)
	}

	// 列表含 state
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-machine-functions", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), `"fw-vm"`) || !strings.Contains(string(data), `"state":"shutoff"`) {
		t.Fatalf("列表应含运行态: %d %s", status, data)
	}

	// 详情
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "img.qcow2") {
		t.Fatalf("详情: %d %s", status, data)
	}
	status, _, _ = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-machine-functions/ghost", token, nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("不存在的 VM 应 404: %d", status)
	}

	// 关机态修改：允许
	status, _, data = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm", token,
		vmBody("fw-vm"), map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("关机态修改应成功: %d %s", status, data)
	}

	// 生命周期动作（FR-CMP-011）
	for _, act := range []string{"start", "restart", "stop"} {
		status, _, data = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm:"+act, token, nil, nil)
		if status != http.StatusAccepted {
			t.Fatalf("%s 应 202: %d %s", act, status, data)
		}
	}
	if len(fake.actions) != 3 {
		t.Fatalf("应执行 3 次动作: %v", fake.actions)
	}

	// 运行中修改 → 409（FR-CMP-012）
	fake.states["fw-vm"] = orchestrator.VMStateRunning
	status, _, data = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm", token,
		vmBody("fw-vm"), map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusConflict || !strings.Contains(string(data), "修改需先关机") {
		t.Fatalf("运行中修改应 409: %d %s", status, data)
	}

	// 生命周期动作入审计（FR-OPS-031）
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/audit-logs?limit=50", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("audit-logs: %d", status)
	}
	for _, want := range []string{"vm.start", "vm.restart", "vm.stop"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("审计应含 %s: %s", want, data)
		}
	}

	// 删除需二次确认（FR-CMP-013）
	status, _, data = cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm", token, nil, nil)
	if status != http.StatusBadRequest || !strings.Contains(string(data), "CONFIRM_REQUIRED") {
		t.Fatalf("无 confirm 应 400: %d %s", status, data)
	}
	status, _, data = cfgRequest(t, http.MethodDelete,
		ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm?confirm=true", token, nil,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("确认删除应 200: %d %s", status, data)
	}
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-machine-functions", token, nil, nil)
	if status != http.StatusOK || strings.Contains(string(data), "fw-vm") {
		t.Fatalf("删除后列表应为空: %s", data)
	}
}

// TestVMActionStartSurfacesDiagnosis 决策 #311：启动停在非预期态时，REST 用既有 Error 形状
// （500 VM_START_FAILED + detail[]）如实透出域状态、日志摘录与恢复建议。
func TestVMActionStartSurfacesDiagnosis(t *testing.T) {
	fake := newFakeVM()
	fake.startProbe = &orchestrator.VMStartProbe{
		OK:      false,
		State:   orchestrator.VMStatePaused,
		Reason:  "paused (starting up)",
		Detail:  "qemu: vhost-user: connect failed",
		LogPath: "/var/log/libvirt/qemu/fw-vm.log",
		Hints:   []string{"确认数据面 VPP 正在运行", "request vpp restart 后重新 start"},
	}
	ts := newTestServerOpts(t, Options{VM: fake})
	token := loginAdmin(t, ts)
	seedVMPool(t, ts, token)
	cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		vmBody("fw-vm"), map[string]string{"X-NFVIS-Auto-Commit": "true"})

	status, _, data := cfgRequest(t, http.MethodPost,
		ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm:start", token, nil, nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("停在非预期态应 500: %d %s", status, data)
	}
	for _, want := range []string{"VM_START_FAILED", "paused", "starting up",
		"vhost-user", "/var/log/libvirt/qemu/fw-vm.log", "request vpp restart"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("响应应含 %q：%s", want, data)
		}
	}
	// 失败必须入审计（failure）。
	_, _, alog := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/audit-logs?limit=50", token, nil, nil)
	if !strings.Contains(string(alog), "vm.start") {
		t.Errorf("启动失败应入审计: %s", alog)
	}
}

// TestVMActionStartNormalShapeUnchanged 正常启动：仍 202 且响应体与既有逐字一致
// （决策 #311：正常路径不因探测而改变输出）。
func TestVMActionStartNormalShapeUnchanged(t *testing.T) {
	fake := newFakeVM()
	ts := newTestServerOpts(t, Options{VM: fake})
	token := loginAdmin(t, ts)
	seedVMPool(t, ts, token)
	cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		vmBody("fw-vm"), map[string]string{"X-NFVIS-Auto-Commit": "true"})

	status, _, data := cfgRequest(t, http.MethodPost,
		ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm:start", token, nil, nil)
	if status != http.StatusAccepted {
		t.Fatalf("正常启动应 202: %d %s", status, data)
	}
	if !strings.Contains(string(data), `"status":"starting"`) || !strings.Contains(string(data), `"name":"fw-vm"`) {
		t.Fatalf("正常启动响应体应保持既有形状: %s", data)
	}
	if strings.Contains(string(data), "VM_START_FAILED") || strings.Contains(string(data), "vm_state") {
		t.Fatalf("正常启动不应带诊断字段: %s", data)
	}
}

// TestVMActionStartDataPlaneUnavailable503 决策 #314：VPP 不可用（启动前置判定命中）时，
// REST 用既有 Error 形状返回 503 UNAVAILABLE（不新增 code/响应字段），message 含「未启动虚拟机」
// 与恢复指引；不得误用「已创建域但没起来」的 500 VM_START_FAILED。
func TestVMActionStartDataPlaneUnavailable503(t *testing.T) {
	fake := newFakeVM()
	fake.startErr = fmt.Errorf("%w：未启动虚拟机 fw-vm（VPP 未连接）；请先恢复数据面后重试——request vpp restart（自查：show vpp）",
		orchestrator.ErrDataPlaneUnavailable)
	ts := newTestServerOpts(t, Options{VM: fake})
	token := loginAdmin(t, ts)
	seedVMPool(t, ts, token)
	cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		vmBody("fw-vm"), map[string]string{"X-NFVIS-Auto-Commit": "true"})

	status, _, data := cfgRequest(t, http.MethodPost,
		ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm:start", token, nil, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("数据面不可用应 503: %d %s", status, data)
	}
	for _, want := range []string{"UNAVAILABLE", "数据面（VPP）当前不可用", "未启动虚拟机", "request vpp restart"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("响应应含 %q：%s", want, data)
		}
	}
	if strings.Contains(string(data), "VM_START_FAILED") {
		t.Errorf("数据面前置判定不是 VM_START_FAILED: %s", data)
	}
	if !strings.Contains(string(alogOf(t, ts, token)), "vm.start") {
		t.Error("启动失败应入审计")
	}
}

// TestVMActionRestartDataPlaneUnavailable503 决策 #314：restart 的 off→start 分支命中数据面
// 不可用，同样 503 UNAVAILABLE（既有 Error 形状）。
func TestVMActionRestartDataPlaneUnavailable503(t *testing.T) {
	fake := newFakeVM()
	fake.restartErr = fmt.Errorf("%w：未启动虚拟机 fw-vm（VPP 未运行）", orchestrator.ErrDataPlaneUnavailable)
	ts := newTestServerOpts(t, Options{VM: fake})
	token := loginAdmin(t, ts)
	seedVMPool(t, ts, token)
	cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		vmBody("fw-vm"), map[string]string{"X-NFVIS-Auto-Commit": "true"})

	status, _, data := cfgRequest(t, http.MethodPost,
		ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm:restart", token, nil, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("数据面不可用应 503: %d %s", status, data)
	}
	if !strings.Contains(string(data), "UNAVAILABLE") {
		t.Errorf("响应应含 code UNAVAILABLE：%s", data)
	}
}

// alogOf 取审计日志原始响应体（断言动作已入库）。
func alogOf(t *testing.T, ts *httptest.Server, token string) []byte {
	t.Helper()
	_, _, alog := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/audit-logs?limit=50", token, nil, nil)
	return alog
}

func TestVMActionErrors(t *testing.T) {
	fake := newFakeVM()
	ts := newTestServerOpts(t, Options{VM: fake})
	token := loginAdmin(t, ts)
	seedVMPool(t, ts, token)
	cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		vmBody("fw-vm"), map[string]string{"X-NFVIS-Auto-Commit": "true"})

	// 未知动作
	status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm:fly", token, nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("未知动作应 404: %d", status)
	}
	// 不在 committed 配置中的目标
	status, _, _ = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions/ghost:start", token, nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("不存在目标应 404: %d", status)
	}
}

// 未装配计算编排：列表可用（省略 state），生命周期动作 503。
func TestVMWithoutRuntime(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-machine-functions", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("未装配时列表仍应 200: %d %s", status, data)
	}
	var list []map[string]any
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("列表应可解析: %s", data)
	}
	status, _, _ = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm:start", token, nil, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("未装配时动作应 503: %d", status)
	}
}
