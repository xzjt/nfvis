package api

// 决策 #339：ACL 逐规则命中读视图（CLI `show acls <name> detail` 与 REST `GET /acls/{name}`）
// ——两面同源（同一 ACLCountersRuntime、同一 aclDetailView），且取数失败如实呈现（不吞）。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/model"
)

// fakeACLCounters 固定返回逐规则命中（键 = 规则下发下标）或错误。
type fakeACLCounters struct {
	hits map[string]map[uint32]uint64
	err  error
}

func (f fakeACLCounters) ACLHitCounters(context.Context) (map[string]map[uint32]uint64, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.hits, nil
}

// seedACL 经引擎预置一条两规则 ACL（CLI 侧）。
func seedACL(t *testing.T, engine *config.Engine) {
	t.Helper()
	sess := config.Session{User: "system", Source: "console"}
	if err := engine.Edit(sess); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	cfg, _ := engine.Committed()
	cfg.Acls = append(cfg.Acls, model.Acl{Name: "acl-web", Rules: []model.AclRule{
		{Seq: 10, Action: "permit", Protocol: "tcp"},
		{Seq: 20, Action: "deny"},
	}})
	if err := engine.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	if _, err := engine.Commit(context.Background(), sess, config.CommitOpts{}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	_ = engine.Release(sess)
}

func TestCLIShowAclDetailHits(t *testing.T) {
	x, engine := newCLIKit(t)
	seedACL(t, engine)
	x.setACLCounters(fakeACLCounters{hits: map[string]map[uint32]uint64{
		"acl-web": {0: 13, 1: 0},
	}})

	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show acls acl-web detail")
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("不应失败: %q", res.Output)
	}
	if strings.Contains(res.Output, "命中计数不可用") {
		t.Fatalf("有计数来源时不应报不可用: %q", res.Output)
	}
	if !strings.Contains(res.Output, "hits 13;") {
		t.Fatalf("详情应含规则 0 的命中 13: %q", res.Output)
	}
	if !strings.Contains(res.Output, "hits 0;") {
		t.Fatalf("规则 1 命中 0 也应呈现（非静默省略）: %q", res.Output)
	}
	if !strings.Contains(res.Output, "总命中：13") {
		t.Fatalf("应有总命中一行（13）: %q", res.Output)
	}
}

// 取数失败：配置详情照常渲染，但**如实**附原因（不把「取不到」当「零命中」）。
func TestCLIShowAclDetailHitsUnavailable(t *testing.T) {
	x, engine := newCLIKit(t)
	seedACL(t, engine)
	x.setACLCounters(fakeACLCounters{err: errors.New("无法连接 stats segment")})

	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show acls acl-web detail")
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("配置详情不应因运行态取数失败而失败: %q", res.Output)
	}
	if !strings.Contains(res.Output, "deny") {
		t.Fatalf("配置详情应照常渲染: %q", res.Output)
	}
	if !strings.Contains(res.Output, "命中计数不可用：无法连接 stats segment") {
		t.Fatalf("应如实附一行原因: %q", res.Output)
	}
	if strings.Contains(res.Output, "hits ") {
		t.Fatalf("取不到时不应给 hits（不编造零命中）: %q", res.Output)
	}
}

// REST 与 CLI 命中一致：同一 ACLCountersRuntime → 同一 aclDetailView。
func TestRESTAclDetailHitsMatchesCLI(t *testing.T) {
	fake := fakeACLCounters{hits: map[string]map[uint32]uint64{
		"acl-web": {0: 13, 1: 0},
	}}
	ts := newTestServerOpts(t, Options{ACLCounters: fake})
	token := loginAdmin(t, ts)

	status, _, body := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/acls", token,
		map[string]any{"name": "acl-web", "rules": []map[string]any{
			{"seq": 10, "action": "permit", "protocol": "tcp"},
			{"seq": 20, "action": "deny"},
		}}, map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusCreated {
		t.Fatalf("创建 ACL: %d %s", status, body)
	}

	status, _, body = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/acls/acl-web", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /acls/acl-web: %d %s", status, body)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("响应非对象: %v", err)
	}
	rules, _ := got["rules"].([]any)
	if len(rules) != 2 {
		t.Fatalf("规则数 = %d，want 2（%s）", len(rules), body)
	}
	for i, want := range []float64{13, 0} {
		rm, _ := rules[i].(map[string]any)
		h, ok := rm["hits"].(float64)
		if !ok || h != want {
			t.Errorf("规则 %d hits = %v，want %v", i, rm["hits"], want)
		}
	}
	if _, has := got["hits_unavailable"]; has {
		t.Errorf("有计数时不应有 hits_unavailable: %s", body)
	}
}

// REST 取数失败：仍回 200 配置详情，hits 省略，另以 hits_unavailable 如实说明原因（不 500、不吞）。
func TestRESTAclDetailHitsUnavailable(t *testing.T) {
	ts := newTestServerOpts(t, Options{ACLCounters: fakeACLCounters{err: errors.New("VPP 未连接")}})
	token := loginAdmin(t, ts)
	status, _, body := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/acls", token,
		map[string]any{"name": "acl-x", "rules": []map[string]any{{"seq": 10, "action": "deny"}}},
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusCreated {
		t.Fatalf("创建 ACL: %d %s", status, body)
	}
	status, _, body = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/acls/acl-x", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("配置详情仍应 200: %d %s", status, body)
	}
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	if got["hits_unavailable"] != "VPP 未连接" {
		t.Errorf("应如实给 hits_unavailable: %s", body)
	}
	rules, _ := got["rules"].([]any)
	if rm, _ := rules[0].(map[string]any); rm != nil {
		if _, has := rm["hits"]; has {
			t.Errorf("取不到时规则不应带 hits: %s", body)
		}
	}
}

// 未接入计数来源（nil）：读视图照常，不带 hits、不报错——与既有可选运行态字段同口径。
func TestACLDetailNoCounterSource(t *testing.T) {
	x, engine := newCLIKit(t)
	seedACL(t, engine)
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show acls acl-web detail")
	if strings.Contains(res.Output, "%%") || strings.Contains(res.Output, "hits ") ||
		strings.Contains(res.Output, "命中计数不可用") {
		t.Fatalf("未接入来源时不应有 hits/原因，且不应失败: %q", res.Output)
	}
}
