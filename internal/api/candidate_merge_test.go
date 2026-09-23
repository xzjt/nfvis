package api

// 缺口④（决策 #127）：`load merge` 的 REST 等价物 —— `PUT /configuration/candidate`
// 带 `X-NFVIS-Merge: true` 时按字段合并（缺省仍是 override 整体替换）。
// 判据：merge 只覆盖文档里出现的字段（未出现的保持不动），且回显**合并后**的 candidate；
// override 保持原语义（未出现的字段被清掉）。

import (
	"encoding/json"
	"net/http"
	"testing"
)

// 先写一份基线 candidate（hostname + idle-timeout + 一条 dns），再用 merge 只改 hostname。
func TestCandidatePutMergeKeepsUnmentionedFields(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	base := map[string]any{
		"system": map[string]any{
			"hostname":             "base-1",
			"idle_timeout_minutes": 15,
			"dns_servers":          []string{"192.0.2.53"},
		},
	}
	status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token, base, nil)
	if status != http.StatusOK {
		t.Fatalf("写基线 candidate: %d %s", status, data)
	}

	// merge：只给 hostname
	patch := map[string]any{"system": map[string]any{"hostname": "merged-1"}}
	status, _, data = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token, patch,
		map[string]string{"X-NFVIS-Merge": "true"})
	if status != http.StatusOK {
		t.Fatalf("merge 写 candidate: %d %s", status, data)
	}
	var got struct {
		Candidate map[string]any `json:"candidate"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("解析响应: %v (%s)", err, data)
	}
	sys, _ := got.Candidate["system"].(map[string]any)
	if sys == nil {
		t.Fatalf("回显应含 system: %s", data)
	}
	if sys["hostname"] != "merged-1" {
		t.Fatalf("hostname 应被覆盖为 merged-1，得到 %v", sys["hostname"])
	}
	// 未提及的字段必须保留（这正是 merge 与 override 的区别）
	if _, ok := sys["idle_timeout_minutes"]; !ok {
		t.Fatalf("merge 不得清掉未提及的 idle_timeout_minutes: %s", data)
	}
	if _, ok := sys["dns_servers"]; !ok {
		t.Fatalf("merge 不得清掉未提及的 dns_servers: %s", data)
	}

	// 对照：同一次会话改回 override，未提及字段应被清掉
	status, _, data = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token, patch, nil)
	if status != http.StatusOK {
		t.Fatalf("override 写 candidate: %d %s", status, data)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("解析响应: %v (%s)", err, data)
	}
	sys, _ = got.Candidate["system"].(map[string]any)
	if sys == nil {
		t.Fatalf("override 后应仍有 system（patch 里有）: %s", data)
	}
	if _, ok := sys["idle_timeout_minutes"]; ok {
		t.Fatalf("override 应整体替换（idle_timeout_minutes 不该在）: %s", data)
	}
}

// merge 也支持 Auto-Commit（一次请求合并+提交）。
func TestCandidatePutMergeWithAutoCommit(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	base := map[string]any{"system": map[string]any{"hostname": "base-1", "idle_timeout_minutes": 15}}
	if status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token, base, nil); status != http.StatusOK {
		t.Fatalf("写基线: %d %s", status, data)
	}
	patch := map[string]any{"system": map[string]any{"hostname": "merged-2"}}
	status, hdr, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token, patch,
		map[string]string{"X-NFVIS-Merge": "true", "X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("merge+auto-commit: %d %s", status, data)
	}
	if hdr.Get("X-NFVIS-Committed") != "true" {
		t.Fatalf("应已提交（X-NFVIS-Committed: true），得到 %q", hdr.Get("X-NFVIS-Committed"))
	}
	// committed 应同时含新 hostname 与保留下来的 idle_timeout_minutes
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("读 committed: %d %s", status, data)
	}
	var doc struct {
		Configuration map[string]any `json:"configuration"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("解析: %v (%s)", err, data)
	}
	sys, _ := doc.Configuration["system"].(map[string]any)
	if sys["hostname"] != "merged-2" {
		t.Fatalf("committed hostname 应为 merged-2: %s", data)
	}
	if _, ok := sys["idle_timeout_minutes"]; !ok {
		t.Fatalf("committed 应保留 merge 前就有的 idle_timeout_minutes: %s", data)
	}
}
