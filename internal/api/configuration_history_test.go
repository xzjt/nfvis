package api

// 配置提交历史端点（`GET /configuration/history`，决策 #142）。
//
// 由来：CLI 早有 `rollback [n]` 与 `show configuration compare rollback <n>`，但**没有
// 「列出历史快照」的读物**——控制台的配置历史页要的是「rev → 时间/用户/注释」的列表，
// 操作者据此先看差异再回滚。本组用例核四件事：
//   ① 正常返回（最新在前、current 标记当前 committed、提交者与提交说明如实回填）；
//   ② 只回元数据——**历史快照里的配置正文不外发**（快照含口令哈希，列表不需要它）；
//   ③ 权限与 `GET /configuration` 同口径（未认证 401、read-only 可读）；
//   ④ 键集恰为契约声明的五个字段（多回字段即漂移）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// historyItem 契约 ConfigRevision 的解出形状。
type historyItem struct {
	Rev         int    `json:"rev"`
	CommittedAt string `json:"committed_at"`
	User        string `json:"user"`
	Comment     string `json:"comment"`
	Current     bool   `json:"current"`
}

func getHistory(t *testing.T, ts *httptest.Server, token string) (int, []byte) {
	t.Helper()
	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/history", token, nil, nil)
	return status, body
}

// commitWithMessage 走两步事务流提交一份带说明的配置（自动提交的直提通道
// 不携带提交说明，故这里用 candidate → commit）。
func commitWithMessage(t *testing.T, ts *httptest.Server, token, hostname, message string) int {
	t.Helper()
	status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		map[string]any{"system": map[string]any{"hostname": hostname}}, nil)
	if status != http.StatusOK {
		t.Fatalf("写 candidate: %d %s", status, body)
	}
	status, _, body = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/configuration/commit", token,
		map[string]any{"message": message}, nil)
	if status != http.StatusOK {
		t.Fatalf("commit: %d %s", status, body)
	}
	var res struct {
		Revision int `json:"revision"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("解析 commit 响应: %v %s", err, body)
	}
	return res.Revision
}

func TestConfigurationHistoryEndpoint(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	rev1 := commitWithMessage(t, ts, token, "hist-node-1", "第一版：改名")
	rev2 := commitWithMessage(t, ts, token, "hist-node-2", "第二版：再改名")
	if rev2 <= rev1 {
		t.Fatalf("两次提交应产生递增修订号，实得 %d → %d", rev1, rev2)
	}

	status, body := getHistory(t, ts, token)
	if status != http.StatusOK {
		t.Fatalf("GET /configuration/history: %d %s", status, body)
	}
	var items []historyItem
	if err := json.Unmarshal(body, &items); err != nil {
		t.Fatalf("响应不是数组: %v %s", err, body)
	}
	if len(items) < 3 { // 初始化 + 预置 viewer + 本轮两次
		t.Fatalf("历史条目过少（%d 条）：%s", len(items), body)
	}
	for i := 1; i < len(items); i++ { // 最新在前
		if items[i].Rev >= items[i-1].Rev {
			t.Fatalf("历史应按 rev 降序（最新在前）：第 %d 条 rev=%d，前一条 rev=%d", i, items[i].Rev, items[i-1].Rev)
		}
	}
	if items[0].Rev != rev2 {
		t.Errorf("首条应为最新修订 rev=%d，实得 %d", rev2, items[0].Rev)
	}
	if !items[0].Current {
		t.Errorf("最新一份必须标记 current=true：%+v", items[0])
	}
	if items[1].Current {
		t.Errorf("非最新一份不该标记 current：%+v", items[1])
	}
	if n := countCurrent(items); n != 1 {
		t.Errorf("current=true 的条目应恰有 1 条，实得 %d", n)
	}
	// 提交者与说明如实回填——这是本端点的**存在理由**（谁、什么时候、为什么提交的）
	if items[0].User != "admin" {
		t.Errorf("最新一份的提交者应为 admin，实得 %q", items[0].User)
	}
	if items[0].Comment != "第二版：再改名" {
		t.Errorf("最新一份的提交说明应为 commit 时给的 message，实得 %q", items[0].Comment)
	}
	if items[1].Comment != "第一版：改名" {
		t.Errorf("次新一份的提交说明实得 %q", items[1].Comment)
	}
	if items[0].CommittedAt == "" || !strings.Contains(items[0].CommittedAt, "T") {
		t.Errorf("提交时间应为 RFC3339 时间戳，实得 %q", items[0].CommittedAt)
	}

	// 与独立事实源对照：GET /configuration 报的 revision 必须等于历史首条
	status, _, cbody := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /configuration: %d %s", status, cbody)
	}
	var cur struct {
		Revision int `json:"revision"`
	}
	if err := json.Unmarshal(cbody, &cur); err != nil {
		t.Fatalf("解析 /configuration: %v %s", err, cbody)
	}
	if cur.Revision != items[0].Rev {
		t.Errorf("历史首条 rev=%d 与 /configuration 的 revision=%d 不一致（取数不是同一份事实）",
			items[0].Rev, cur.Revision)
	}

	// 键集恰为契约声明的五个字段：多回的字段（尤其配置正文）不该出现
	want := []string{"comment", "committed_at", "current", "rev", "user"}
	var raw []map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("解析原始条目: %v", err)
	}
	for i, it := range raw {
		got := make([]string, 0, len(it))
		for k := range it {
			got = append(got, k)
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("第 %d 条的字段集与契约不符：实得 %v，契约 %v", i, got, want)
		}
	}
}

// TestConfigurationHistoryCarriesNoConfigBody 历史列表**只回元数据**：
// 快照正文里有口令哈希（测试服务器预置的 viewer 用户），列表一律不外发（FR-SEC-007）。
func TestConfigurationHistoryCarriesNoConfigBody(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	status, body := getHistory(t, ts, token)
	if status != http.StatusOK {
		t.Fatalf("GET /configuration/history: %d %s", status, body)
	}
	// 前提自校准：committed 配置里确实有 viewer 这个本地用户——它的**口令哈希只存在于
	// 快照正文**（配置视图读取时会被 redactConfigView 摘掉），故这条断言证明
	// 「历史里那些快照的正文含敏感字段」，本用例才不是空转。
	status, _, cbody := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /configuration: %d %s", status, cbody)
	}
	if !strings.Contains(string(cbody), "viewer") {
		t.Fatalf("前提不成立：committed 配置里没有预置的 viewer 用户（测试装配变了？），本用例无从判别")
	}

	for _, leak := range []string{"password_hash", "pbkdf2$", "config_json", "configuration", "hostname"} {
		if strings.Contains(string(body), leak) {
			t.Errorf("历史响应里出现了 %q——列表只该回元数据（rev/时间/用户/注释/current）", leak)
		}
	}
}

// TestConfigurationHistoryPermission 权限与 `GET /configuration` 同口径：
// ClassReadOnly 可读（历史不含敏感字段），未认证 401。
func TestConfigurationHistoryPermission(t *testing.T) {
	ts := newTestServer(t)

	if status, body := getHistory(t, ts, ""); status != http.StatusUnauthorized {
		t.Errorf("未认证访问应 401，实得 %d %s", status, body)
	}
	viewer := loginViewer(t, ts)
	if status, body := getHistory(t, ts, viewer); status != http.StatusOK {
		t.Errorf("read-only class 应可读（与 GET /configuration 同口径），实得 %d %s", status, body)
	}
}

func countCurrent(items []historyItem) int {
	n := 0
	for _, it := range items {
		if it.Current {
			n++
		}
	}
	return n
}
