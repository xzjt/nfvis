package api

// FR-NET-001（决策 #72）：网卡 DPDK 驱动接管的 API/CLI 面。
// 底座（sysfs）由 network.DPDKBinder 注入假实现，这里只验证契约与确认语义。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/schema"
)

type fakeDPDK struct {
	lastIface  string
	lastBound  bool
	lastDriver string
	calls      int
	err        error
}

func (f *fakeDPDK) SetDPDKBound(_ context.Context, ifname string, bound bool, driver string) (string, string, error) {
	f.calls++
	f.lastIface, f.lastBound, f.lastDriver = ifname, bound, driver
	if f.err != nil {
		return "", "", f.err
	}
	if bound {
		d := driver
		if d == "" {
			d = "vfio-pci"
		}
		return "0000:13:00.0", d, nil
	}
	return "0000:13:00.0", "vmxnet3", nil
}

// 未确认 → 400 CONFIRM_REQUIRED（绑定会中断该网卡流量）
func TestPutDPDKRequiresConfirm(t *testing.T) {
	f := &fakeDPDK{}
	ts := newTestServerOpts(t, Options{DPDK: f})
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/interfaces/ens224/dpdk", token,
		map[string]any{"bound": true}, nil)
	if status != http.StatusBadRequest || !strings.Contains(string(data), "CONFIRM_REQUIRED") {
		t.Fatalf("缺 confirm 应 400: %d %s", status, data)
	}
	if f.calls != 0 {
		t.Fatal("未确认不应调用底座")
	}
}

func TestPutDPDKBindAndUnbind(t *testing.T) {
	f := &fakeDPDK{}
	ts := newTestServerOpts(t, Options{DPDK: f})
	token := loginAdmin(t, ts)

	// 绑定（缺省驱动 vfio-pci）
	status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/interfaces/ens224/dpdk?confirm=true", token,
		map[string]any{"bound": true}, nil)
	if status != http.StatusOK {
		t.Fatalf("绑定: %d %s", status, data)
	}
	var got struct{ Interface, PCI, Driver string }
	_ = json.Unmarshal(data, &got)
	if f.lastIface != "ens224" || !f.lastBound || got.Driver != "vfio-pci" || got.PCI == "" {
		t.Fatalf("绑定未生效: %s %+v", data, f)
	}

	// 解绑
	status, _, data = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/interfaces/ens224/dpdk?confirm=true", token,
		map[string]any{"bound": false}, nil)
	if status != http.StatusOK || f.lastBound {
		t.Fatalf("解绑: %d %s", status, data)
	}
	if !strings.Contains(string(data), "vmxnet3") {
		t.Fatalf("解绑后应报告当前驱动: %s", data)
	}

	// bound 必填
	if status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/interfaces/ens224/dpdk?confirm=true", token,
		map[string]any{}, nil); status != http.StatusBadRequest {
		t.Fatalf("缺 bound 应 400: %d", status)
	}

	// 底座报错 → 400（不静默）
	f.err = errors.New("目标驱动 vfio-pci 不可用（模块未加载？）")
	if status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/interfaces/ens224/dpdk?confirm=true", token,
		map[string]any{"bound": true}, nil); status != http.StatusBadRequest || !strings.Contains(string(data), "不可用") {
		t.Fatalf("底座错误应 400 且带原因: %d %s", status, data)
	}
}

// 未装配 → 503
func TestPutDPDKUnavailable(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	if status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/interfaces/ens224/dpdk?confirm=true", token,
		map[string]any{"bound": true}, nil); status != http.StatusServiceUnavailable {
		t.Fatalf("未装配应 503: %d", status)
	}
}

// CLI 语法：bind-dpdk / unbind-dpdk 必须在命令树中可解析，且不得落到通用 fallback。
func TestCLIDPDKCommandCoverage(t *testing.T) {
	for _, stmt := range [][]string{
		{"request", "interfaces", "ens224", "bind-dpdk"},
		{"request", "interfaces", "ens224", "bind-dpdk", "uio-driver", "vfio-pci"},
		{"request", "interfaces", "ens224", "unbind-dpdk"},
	} {
		if _, _, err := schema.Match(schema.OperRoot(), stmt); err != nil {
			t.Fatalf("命令树应可解析 %v: %v", stmt, err)
		}
	}
	// 运行期（未装配）应给明确提示而非通用 fallback
	x, _ := newCLIKit(t)
	out := x.Execute("admin", "super-user", "ssh", "request interfaces ens224 bind-dpdk").Output
	if !strings.Contains(out, "DPDK 接管不可用") {
		t.Fatalf("未装配应明确提示: %q", out)
	}
}

// 确认语义：不带 --yes 只问不做；带 --yes 才执行（脚本/管道不得静默执行破坏性动作）。
func TestCLIDPDKBindNeedsConfirm(t *testing.T) {
	x, _ := newCLIKit(t)
	f := &fakeDPDK{}
	x.setDPDK(f)

	out := x.Execute("admin", "super-user", "ssh", "request interfaces ens224 bind-dpdk").Output
	if !strings.Contains(out, "yes,no") {
		t.Fatalf("应先问询: %q", out)
	}
	if f.calls != 0 {
		t.Fatal("未确认不应调用底座")
	}
	out = x.Execute("admin", "super-user", "ssh", "request interfaces ens224 bind-dpdk --yes").Output
	if f.calls != 1 || !f.lastBound {
		t.Fatalf("带 --yes 应执行: %q", out)
	}
	if !strings.Contains(out, "vfio-pci") {
		t.Fatalf("应报告绑定结果: %q", out)
	}

	// 解绑同样需确认
	out = x.Execute("admin", "super-user", "ssh", "request interfaces ens224 unbind-dpdk").Output
	if !strings.Contains(out, "yes,no") || f.calls != 1 {
		t.Fatalf("解绑应先问询: %q", out)
	}
	out = x.Execute("admin", "super-user", "ssh", "request interfaces ens224 unbind-dpdk --yes").Output
	if f.calls != 2 || f.lastBound {
		t.Fatalf("解绑应执行: %q", out)
	}
	if !strings.Contains(out, "vmxnet3") {
		t.Fatalf("解绑后应报告内核驱动: %q", out)
	}
}
