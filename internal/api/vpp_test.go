package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// ---------- M3-2：/vpp/status 与 /vpp/restart（FR-SYS-007/009） ----------

type fakeVppController struct {
	restarted  bool
	restartErr error
	status     VppStatus
}

func (f *fakeVppController) Status(*model.VppConfig) VppStatus { return f.status }
func (f *fakeVppController) Restart(context.Context, *model.VppConfig) error {
	f.restarted = true
	return f.restartErr
}

func TestVppStatusEndpoint(t *testing.T) {
	fake := &fakeVppController{status: VppStatus{
		Version: "26.06-release", Connected: true, PendingRestart: true}}
	ts := newTestServerOpts(t, Options{VPP: fake})
	token := loginAdmin(t, ts)

	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/vpp/status", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /vpp/status: %d %s", status, data)
	}
	var got VppStatus
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if got.Version != "26.06-release" || !got.Connected || !got.PendingRestart {
		t.Fatalf("状态不符: %+v", got)
	}
}

func TestVppRestartEndpoint(t *testing.T) {
	fake := &fakeVppController{}
	ts := newTestServerOpts(t, Options{VPP: fake})
	token := loginAdmin(t, ts)

	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/vpp/restart", token, nil, nil)
	if status != http.StatusAccepted {
		t.Fatalf("POST /vpp/restart: %d %s", status, data)
	}
	if !fake.restarted {
		t.Fatalf("应调用控制器 Restart")
	}
}

// read-only 用户不可重启（super-user 限定）。
func TestVppRestartForbiddenForViewer(t *testing.T) {
	fake := &fakeVppController{}
	ts := newTestServerOpts(t, Options{VPP: fake})
	token := loginViewer(t, ts)

	status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/vpp/restart", token, nil, nil)
	if status != http.StatusForbidden {
		t.Fatalf("read-only 应 403: %d", status)
	}
	if fake.restarted {
		t.Fatalf("403 不应触发 Restart")
	}
}

// 未装配编排器时返回 503。
func TestVppEndpointsUnavailable(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	status, _, _ := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/vpp/status", token, nil, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("未装配应 503: %d", status)
	}
}
