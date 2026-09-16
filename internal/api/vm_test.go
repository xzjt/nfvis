package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/orchestrator"
)

// fakeVM VMRuntime 测试替身。
type fakeVM struct {
	states  map[string]string
	actions []string
}

func newFakeVM() *fakeVM { return &fakeVM{states: map[string]string{}} }

func (f *fakeVM) StartVM(_ context.Context, name string) error {
	f.actions = append(f.actions, "start:"+name)
	f.states[name] = orchestrator.VMStateRunning
	return nil
}
func (f *fakeVM) StopVM(_ context.Context, name string) error {
	f.actions = append(f.actions, "stop:"+name)
	f.states[name] = orchestrator.VMStateShutoff
	return nil
}
func (f *fakeVM) RestartVM(_ context.Context, name string) error {
	f.actions = append(f.actions, "restart:"+name)
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
