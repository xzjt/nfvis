package api

// R37-2 收口（决策 #118）：/system/version 与 show version 的组件版本 plumbing。
// 探测器与 VPP 连接管理器都经 Options 注入——形状守护因此能在 CI 上核到全部键，
// 不依赖真机底座（真机可用性另由 internal/system 的单测与真机复验覆盖）。

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// fakeVersions 组件版本探测的测试替身。
type fakeVersions struct{}

func (fakeVersions) Components(context.Context) map[string]string {
	return map[string]string{"ubuntu": "26.04", "libvirt": "12.0.0", "qemu": "10.2.1", "docker": "29.1.3"}
}

// 探测到的键必须真的发出去；DPDK 无来源，不给；任何键都不得以空串形式出现。
func TestVersionEndpointReportsProbedComponents(t *testing.T) {
	ts := newTestServerOpts(t, Options{
		Versions: fakeVersions{},
		VPP:      &fakeVppController{status: VppStatus{Version: "26.06-release", Connected: true}},
	})
	token := loginAdmin(t, ts)
	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/version", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /system/version: %d %s", status, body)
	}
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("解析: %v", err)
	}
	want := map[string]string{
		"nfvis": VersionStr, "ubuntu": "26.04", "vpp": "26.06-release",
		"libvirt": "12.0.0", "qemu": "10.2.1", "docker": "29.1.3",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("/system/version = %v，期望 %v", got, want)
	}
}

// 未注入探测器（旧装配/单测）时只回 NFViS：不 panic、不编造空串键。
func TestVersionEndpointWithoutProbe(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/version", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /system/version: %d %s", status, body)
	}
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(got) != 1 || got["nfvis"] != VersionStr {
		t.Fatalf("无探测器时应只回 nfvis，得到 %v", got)
	}
}

// show version 的七组件汇总：探测到的印版本，探测不到的明说，不显示空值。
func TestShowVersionSummary(t *testing.T) {
	x := newCLIExecutor(nil, nil)
	x.setVersions(fakeVersions{})
	x.setVppCtl(&fakeVppController{status: VppStatus{Version: "26.06-release"}})
	out := x.versionSummary()
	for _, want := range []string{"NFViS", VersionStr, "Ubuntu", "26.04", "VPP", "26.06-release",
		"libvirt", "12.0.0", "QEMU", "10.2.1", "Docker", "29.1.3"} {
		if !strings.Contains(out, want) {
			t.Errorf("show version 输出应含 %q：\n%s", want, out)
		}
	}
	// DPDK 无来源 → 明说未探测到（不是空值，也不是假版本）。
	if !strings.Contains(out, "DPDK") || !strings.Contains(out, "（未探测到）") {
		t.Errorf("DPDK 应显示未探测到：\n%s", out)
	}
	if strings.Contains(out, "FR-") || strings.Contains(out, "决策 #") {
		t.Errorf("操作者可见文本不得含内部引用：\n%s", out)
	}
}

// 未注入探测器时 show version 仍可用（只印 NFViS，其余未探测到），不 panic。
func TestShowVersionSummaryWithoutProbe(t *testing.T) {
	x := newCLIExecutor(nil, nil)
	out := x.versionSummary()
	if !strings.Contains(out, "NFViS") || !strings.Contains(out, VersionStr) {
		t.Fatalf("应至少印出 NFViS 版本：\n%s", out)
	}
}
