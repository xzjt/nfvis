package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	ksys "github.com/xzjt/nfvis/internal/system"
)

// 决策 #146：`POST /system/kernel:apply|:rollback` —— 契约里早就声明、服务端**从未注册**的幽灵端点
// （控制台「内核基线」页的写入按钮点了得到 404）。本用例钉住三件事：
//
//	① class = super-user（operator / read-only 一律 403）——命令树 §3 与《命令全表》都写 S；
//	② 与 CLI **同源**：走同一个 applyKernelBaseline/rollbackKernelBaseline + 同一个落地器
//	   （断言落地器真的被调用、且落地的期望基线与 `GET /system/kernel` 的 desired 一致）；
//	③ 结果形状：action / backup / pending_reboot（契约 KernelBaselineAction）。
//
// 变异验证（手工做过）：把 server.go 里注册的 class 改回 ClassReadOnly → ① 立刻报 ✗。
type fakeKernelApplier struct {
	applied   []ksys.KernelDesired
	rollbacks int
}

func (f *fakeKernelApplier) Apply(d ksys.KernelDesired) (string, error) {
	f.applied = append(f.applied, d)
	return "/etc/default/grub.d/99-nfvis.cfg.bak-test", nil
}

func (f *fakeKernelApplier) Rollback() (string, error) {
	f.rollbacks++
	return "已回退内核基线（恢复上一版本片段）", nil
}

func TestKernelBaselineEndpoints(t *testing.T) {
	fake := &fakeKernelApplier{}
	ts := newTestServerOpts(t, Options{Kernel: fake})
	admin := loginAdmin(t, ts)
	_, viewer := login(t, ts, "viewer", "s3cret-Passw0rd!")

	// 建一个 operator 账号（用于 class 断言）
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/login-users", admin,
		map[string]any{"name": "opk", "class": "operator", "password": "Op@12345678"},
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("建 operator: %d %s", status, data)
	}
	_, op := login(t, ts, "opk", "Op@12345678")

	applyURL := ts.URL + APIPrefix + "/system/kernel:apply"
	rollbackURL := ts.URL + APIPrefix + "/system/kernel:rollback"

	// ① class：operator 与 read-only 一律 403
	for _, c := range []struct{ name, tok string }{{"operator", op.Token}, {"read-only", viewer.Token}} {
		if st, _, _ := cfgRequest(t, http.MethodPost, applyURL, c.tok, nil, nil); st != http.StatusForbidden {
			t.Errorf("%s 调 apply 应 403（声明为 super-user），实际 %d", c.name, st)
		}
		if st, _, _ := cfgRequest(t, http.MethodPost, rollbackURL, c.tok, nil, nil); st != http.StatusForbidden {
			t.Errorf("%s 调 rollback 应 403，实际 %d", c.name, st)
		}
	}
	if len(fake.applied) != 0 || fake.rollbacks != 0 {
		t.Fatalf("被拒的请求不该碰到落地器：applied=%d rollbacks=%d", len(fake.applied), fake.rollbacks)
	}

	// ② super-user apply → 200，且**真的调了落地器**
	status, _, data = cfgRequest(t, http.MethodPost, applyURL, admin, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("super-user apply 应 200: %d %s", status, data)
	}
	var out struct {
		Action        string   `json:"action"`
		Backup        string   `json:"backup"`
		PendingReboot []string `json:"pending_reboot"`
		Message       string   `json:"message"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("响应不是 KernelBaselineAction: %s (%v)", data, err)
	}
	if out.Action != "apply" || out.Backup == "" {
		t.Errorf("apply 结果形状不对：action=%q backup=%q", out.Action, out.Backup)
	}
	if out.PendingReboot == nil {
		t.Errorf("pending_reboot 必须是数组（空数组表示已一致），实际是 null：%s", data)
	}
	if len(fake.applied) != 1 {
		t.Fatalf("落地器应被调用 1 次，实际 %d", len(fake.applied))
	}
	// 同源判据：落地的期望基线 == `GET /system/kernel` 的 desired（同一份派生函数）
	view, err := kernelBaselineView(committedConfigViaREST(t, ts, admin))
	if err != nil {
		t.Fatalf("kernelBaselineView: %v", err)
	}
	wantJSON, _ := json.Marshal(view["desired"])
	gotJSON, _ := json.Marshal(fake.applied[0])
	var wantDesired, gotDesired map[string]any
	if err := json.Unmarshal(wantJSON, &wantDesired); err != nil {
		t.Fatalf("解析 desired: %v", err)
	}
	if err := json.Unmarshal(gotJSON, &gotDesired); err != nil {
		t.Fatalf("解析落地基线: %v", err)
	}
	if !reflect.DeepEqual(wantDesired, gotDesired) {
		t.Errorf("REST 落地的期望基线与 GET /system/kernel 的 desired 不一致（两侧必须同源）：\n want=%s\n got =%s",
			wantJSON, gotJSON)
	}

	// ③ rollback → 200 + message，落地器被调用一次
	status, _, data = cfgRequest(t, http.MethodPost, rollbackURL, admin, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("super-user rollback 应 200: %d %s", status, data)
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Action != "rollback" || out.Message == "" {
		t.Errorf("rollback 结果形状不对：%s", data)
	}
	if fake.rollbacks != 1 {
		t.Errorf("Rollback 应被调用 1 次，实际 %d", fake.rollbacks)
	}

	// ④ 未装配落地器 → 503（如实说"未接入"，不是 404/500）
	ts2 := newTestServer(t)
	admin2 := loginAdmin(t, ts2)
	if st, _, body := cfgRequest(t, http.MethodPost, ts2.URL+APIPrefix+"/system/kernel:apply", admin2, nil, nil); st != http.StatusServiceUnavailable {
		t.Errorf("未装配落地器应 503，实际 %d %s", st, body)
	}
}

// committedConfigViaREST 取 committed 配置（走 REST，不直接摸引擎）。
func committedConfigViaREST(t *testing.T, ts *httptest.Server, tok string) model.Config {
	t.Helper()
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration", tok, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /configuration: %d %s", status, data)
	}
	var doc struct {
		Configuration model.Config `json:"configuration"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("解析 /configuration: %v", err)
	}
	return doc.Configuration
}
