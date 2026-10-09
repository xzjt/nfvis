package api

// R2-25：REST 对「能力在当前数据面不受支持」的响应码。
//
// 此前这类错误一律落到 500/502（内核数据面下的 VPP 重启、NAT 会话表、LLDP 邻居、
// 接口清零统计、抓包）——监控会把「这条路在内核数据面下不存在」记成服务异常。
// 现在按提供者的哨兵（netkernel.ErrUnsupported / ErrStatsClearUnsupported，经 %w 包装）
// 映射 **501 Not Implemented**（message 保留原始文案，含数据面与替代路径）；
// VPP 数据面下的真实执行故障仍是 500/502，装配缺失仍是 503——三者分工明确。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/netkernel"
)

// errLldpRuntime LLDP 运行态抛错替身（fakeLldpRuntime 只会成功）。
type errLldpRuntime struct{ err error }

func (f errLldpRuntime) Neighbors(context.Context) ([]LldpNeighborRow, error) {
	return nil, f.err
}

func TestRESTUnsupportedCapabilityIs501(t *testing.T) {
	// 内核数据面提供者的能力错误：经 %w 包装后仍要被识别为「不受支持」
	unsupported := fmt.Errorf("%w：%s", netkernel.ErrUnsupported, "NAT 会话表")

	// ① GET /nat/sessions
	ts := newTestServerOpts(t, Options{NAT: &fakeNatSessions{err: unsupported}})
	token := loginAdmin(t, ts)
	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/nat/sessions", token, nil, nil)
	if status != http.StatusNotImplemented {
		t.Fatalf("能力不受支持应 501，得到 %d %s", status, body)
	}
	if !strings.Contains(string(body), "NOT_IMPLEMENTED") || !strings.Contains(string(body), "NAT 会话表") {
		t.Fatalf("501 应带错误码且保留原始文案：%s", body)
	}

	// ② GET /protocols/lldp/neighbors
	ts2 := newTestServerOpts(t, Options{LLDP: errLldpRuntime{err: fmt.Errorf("%w：%s", netkernel.ErrUnsupported, "LLDP 邻居")}})
	token2 := loginAdmin(t, ts2)
	status, _, body = cfgRequest(t, http.MethodGet, ts2.URL+APIPrefix+"/protocols/lldp/neighbors", token2, nil, nil)
	if status != http.StatusNotImplemented {
		t.Fatalf("LLDP 不受支持应 501，得到 %d %s", status, body)
	}

	// ③ POST /interfaces:clear-statistics（内核 diag 的独立哨兵）
	ts3 := diagServer(t, &fakeDiag{clrErr: netkernel.ErrStatsClearUnsupported})
	token3 := loginAdmin(t, ts3)
	status, _, body = cfgRequest(t, http.MethodPost, ts3.URL+APIPrefix+"/interfaces:clear-statistics", token3,
		map[string]any{"name": "ens192"}, nil)
	if status != http.StatusNotImplemented {
		t.Fatalf("清零统计不受支持应 501，得到 %d %s", status, body)
	}

	// ④ POST /vpp/restart（内核数据面 VppController 的错误在 api 之外，按装配事实判）
	ts4 := newTestServerOpts(t, Options{VPP: &fakeVppController{
		status:     VppStatus{Mode: model.DataPlaneKernel},
		restartErr: errors.New("当前数据面为 Linux 内核网络，该能力不可用"),
	}})
	token4 := loginAdmin(t, ts4)
	status, _, body = cfgRequest(t, http.MethodPost, ts4.URL+APIPrefix+"/vpp/restart", token4, nil, nil)
	if status != http.StatusNotImplemented {
		t.Fatalf("内核数据面重启应 501，得到 %d %s", status, body)
	}
	if !strings.Contains(string(body), "Linux 内核网络") {
		t.Fatalf("501 文案应点名数据面：%s", body)
	}
}

// 对照组：同一批端点在 VPP 数据面下的真实故障仍是 500/502，装配缺失仍是 503（不误判）。
func TestRESTVppFaultsStay500AndUnwired503(t *testing.T) {
	// NAT 会话表执行故障（非「不受支持」）→ 500
	ts := newTestServerOpts(t, Options{NAT: &fakeNatSessions{err: errors.New("VPP 会话读取失败")}})
	token := loginAdmin(t, ts)
	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/nat/sessions", token, nil, nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("VPP 侧执行故障应 500，得到 %d %s", status, body)
	}

	// 清零统计执行失败（非不受支持）→ 502
	ts2 := diagServer(t, &fakeDiag{clrErr: errors.New("vppctl 执行失败")})
	token2 := loginAdmin(t, ts2)
	status, _, body = cfgRequest(t, http.MethodPost, ts2.URL+APIPrefix+"/interfaces:clear-statistics", token2,
		map[string]any{"name": "ens192"}, nil)
	if status != http.StatusBadGateway {
		t.Fatalf("清零统计执行失败应 502，得到 %d %s", status, body)
	}

	// VPP 数据面下的重启执行故障（无装配事实自报 Mode）→ 500
	ts3 := newTestServerOpts(t, Options{VPP: &fakeVppController{
		status:     VppStatus{Version: "26.06", Connected: true},
		restartErr: errors.New("VPP 未起来"),
	}})
	token3 := loginAdmin(t, ts3)
	status, _, body = cfgRequest(t, http.MethodPost, ts3.URL+APIPrefix+"/vpp/restart", token3, nil, nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("VPP 重启执行故障应 500，得到 %d %s", status, body)
	}

	// 抓包模块未装配（VPP 数据面）→ 503（保持既有语义）
	ts4 := newTestServerOpts(t, Options{VPP: fakeVppCtl{st: VppStatus{Mode: model.DataPlaneVPP}}})
	token4 := loginAdmin(t, ts4)
	status, _, body = cfgRequest(t, http.MethodGet, ts4.URL+APIPrefix+"/vpp/capture", token4, nil, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("VPP 数据面未装配抓包应 503，得到 %d %s", status, body)
	}
}

// 抓包：两种数据面**都有实现**（VPP 走 pcap trace、内核走 tcpdump），故未装配 Provider
// 只剩「模块未接入」这一种成因——503，且不再按数据面分叉（没有「该数据面没有该能力」的指向）。
// 新名 `/capture` 与兼容别名 `/vpp/capture` 走同一 handler，两者行为逐字相同。
func TestCaptureWiringGapIs503Not501(t *testing.T) {
	for _, mode := range []string{model.DataPlaneVPP, model.DataPlaneKernel} {
		ts := newTestServerOpts(t, Options{VPP: fakeVppCtl{st: VppStatus{Mode: mode}}})
		token := loginAdmin(t, ts)
		for _, ep := range []struct{ method, path string }{
			{http.MethodGet, "/capture"},
			{http.MethodPost, "/capture"},
			{http.MethodDelete, "/capture"},
			{http.MethodGet, "/capture/x.pcap"},
			{http.MethodGet, "/vpp/capture"},        // 兼容别名
			{http.MethodPost, "/vpp/capture"},       // 兼容别名
			{http.MethodDelete, "/vpp/capture"},     // 兼容别名
			{http.MethodGet, "/vpp/capture/x.pcap"}, // 兼容别名
		} {
			var payload any
			if ep.method == http.MethodPost {
				payload = map[string]any{"interface": "ens192"}
			}
			status, _, body := cfgRequest(t, ep.method, ts.URL+APIPrefix+ep.path, token, payload, nil)
			if status != http.StatusServiceUnavailable {
				t.Fatalf("%s %s（数据面 %s）未装配抓包应 503，得到 %d %s", ep.method, ep.path, mode, status, body)
			}
			if !strings.Contains(string(body), "抓包模块未接入") {
				t.Fatalf("%s %s 应如实报「抓包模块未接入」：%s", ep.method, ep.path, body)
			}
			if strings.Contains(string(body), "尚未实现") {
				t.Fatalf("%s %s 不得再报「尚未实现」（两数据面都有实现）：%s", ep.method, ep.path, body)
			}
		}
	}
}
