package api

// round51 评审 L15：`GET /configuration/candidate` 与 `GET /configuration/diff` 此前只要求
// 「存在某个编辑会话」（Engine 的 requireEditingLocked 只看 candidate != nil），于是**非持有者**
// 也能读到他人候选与差异——与 CLI `show configuration`（对非持有者回退 committed）口径不一致。
// 本文件钉住两个方向：持有者响应逐字不变；非持有者回退 committed（带 source 标记）且拿不到他人草稿。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestFixR51CandidateReadIsHoldLockAware(t *testing.T) {
	ts := newTestServer(t)
	holder, other := loginAdminTwice(t, ts) // 同一用户、两个不同 token = 两个不同会话

	// 持有者写候选（另一会话持有锁）
	if status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate",
		holder, sampleCandidate(), nil); status != http.StatusOK {
		t.Fatalf("PUT candidate: %d %s", status, body)
	}

	// ① 持有者：响应逐字不变（candidate = 自己的草稿、dirty=true、**无** source 键）
	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/candidate", holder, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("持有者 GET candidate: %d %s", status, body)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("解析 candidate 响应: %v (%s)", err, body)
	}
	if _, ok := raw["source"]; ok {
		t.Fatalf("持有者响应不得含 source 键（行为须逐字不变）: %s", body)
	}
	var view struct {
		Candidate struct {
			System struct {
				Hostname string `json:"hostname"`
			} `json:"system"`
		} `json:"candidate"`
		Dirty bool `json:"dirty"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("解析 candidate 内容: %v (%s)", err, body)
	}
	if !view.Dirty || view.Candidate.System.Hostname != "api-node" {
		t.Fatalf("持有者应读到自己的候选（dirty=true、hostname=api-node）: %s", body)
	}
	status, _, body = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/diff", holder, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(body), "api-node") {
		t.Fatalf("持有者 diff 应含自己的变更: %d %s", status, body)
	}

	// ② 非持有者（同用户、另一 token）：看不到他人草稿，回退 committed 并显式标出来源
	status, _, body = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/candidate", other, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("非持有者 GET candidate: %d %s", status, body)
	}
	var nonHolder map[string]any
	if err := json.Unmarshal(body, &nonHolder); err != nil {
		t.Fatalf("解析非持有者响应: %v (%s)", err, body)
	}
	if nonHolder["source"] != "committed" {
		t.Fatalf("非持有者应标 source=committed: %s", body)
	}
	if d, _ := nonHolder["dirty"].(bool); d {
		t.Fatalf("非持有者回退的是生效配置，dirty 应 false: %s", body)
	}
	candJSON, _ := json.Marshal(nonHolder["candidate"])
	if strings.Contains(string(candJSON), "api-node") {
		t.Fatalf("非持有者不得读到他人候选（hostname 泄漏）: %s", body)
	}

	// 非持有者 diff 为空（没有「自己的候选」）
	status, _, body = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/diff", other, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("非持有者 GET diff: %d %s", status, body)
	}
	if strings.TrimSpace(string(body)) != "" {
		t.Fatalf("非持有者 diff 应为空（不得返回他人候选的差异）: %q", body)
	}

	// ③ 丢弃候选（释放锁）后无编辑会话：两个会话都回到 409（既有语义不变）
	if status, _, _ := cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/configuration/candidate", holder, nil, nil); status != http.StatusNoContent {
		t.Fatalf("discard 应 204: %d", status)
	}
	if status, _, _ := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/candidate", other, nil, nil); status != http.StatusConflict {
		t.Fatalf("无编辑会话时仍应 409: %d", status)
	}
	if status, _, _ := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/diff", other, nil, nil); status != http.StatusConflict {
		t.Fatalf("无编辑会话时 diff 仍应 409: %d", status)
	}
}

// TestFixR51CandidateReadSameTokenViaCLIIsStillHolder 边界（与上面的判据互为反面）：
// 同一 token 经 CLI 桥取的锁，其 REST 读回仍是**自己的**候选——会话身份是稳定 ID（一次登录），
// 接入源只是通道；判据若按 `user@api` 身份键就会把「自己的草稿」误判成别人的（clean_lock_test.go
// 的既有契约正是这一条）。
func TestFixR51CandidateReadSameTokenViaCLIIsStillHolder(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	execVia(t, ts, token, "127.0.0.1:40001", "ssh", "configure")
	execVia(t, ts, token, "127.0.0.1:40001", "ssh", "set system hostname same-token-dirty")

	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/candidate", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("同一 token 的 REST 读 candidate: %d %s", status, body)
	}
	if strings.Contains(string(body), `"source"`) {
		t.Fatalf("同一 token 是持有者，响应不得带 source 键: %s", body)
	}
	var view struct {
		Candidate struct {
			System struct {
				Hostname string `json:"hostname"`
			} `json:"system"`
		} `json:"candidate"`
		Dirty bool `json:"dirty"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("解析: %v (%s)", err, body)
	}
	if view.Candidate.System.Hostname != "same-token-dirty" || !view.Dirty {
		t.Fatalf("同一 token 的 REST 读应仍读到自己的候选: %s", body)
	}
	// 同一 token 的 diff 也应是自己的变更
	status, _, body = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/diff", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(body), "same-token-dirty") {
		t.Fatalf("同一 token 的 diff 应含自己的变更: %d %s", status, body)
	}
}

// ---------- round51 评审 L2：`DELETE /images/{name}` 的响应形状 ----------

// TestFixR51DeleteImageShapeMatchesContract 删除镜像的响应形状必须与**契约声明**逐字段一致。
//
// 由来：契约此前把 delete 的 200 声明为 `CommitResult`（committed/revision/confirmed_until/
// warnings），而实现回的是 `{"status":"deleted","name":…}`——删除镜像不产生配置提交，
// CommitResult 那几个字段本就不该有。本用例以**已发布的契约**为 oracle（declaredProps 读内嵌
// 规范）：契约与实现任一漂移都会红（把 delete 改回 CommitResult 也会红）。
func TestFixR51DeleteImageShapeMatchesContract(t *testing.T) {
	ts := newTestServerOpts(t, Options{Images: &fakeImagesRuntime{}})
	token := loginAdmin(t, ts)
	spec := loadEmbeddedSpec(t)

	props := declaredPropsStatus(t, spec, "/images/{name}", "DELETE", "200")
	if len(props) == 0 {
		t.Fatal("契约里取不到 DELETE /images/{name} 的 200 响应字段")
	}
	status, _, body := cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/images/alpine.qcow2", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("DELETE /images: %d %s", status, body)
	}
	got := responseObject(t, body)
	want := make(map[string]bool, len(props))
	for _, f := range props {
		want[f] = true
	}
	assertPropsExact(t, "DELETE /images/{name}", got, want)
	// 值也要逐字对（形状对、取值错同样是漂移）
	if got["status"] != "deleted" || got["name"] != "alpine.qcow2" {
		t.Fatalf("删除结果取值不符: %v", got)
	}
	// CommitResult 的字段不得出现（那正是被修掉的误声明）
	for _, f := range []string{"committed", "revision", "confirmed_until", "warnings"} {
		if _, ok := got[f]; ok {
			t.Fatalf("删除响应不得含 CommitResult 字段 %s: %s", f, body)
		}
	}
}
