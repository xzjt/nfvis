package api

// 决策 #369（收口 R142-11）：FR-CFG-012 接入源事实化——`console` 自述只在连接源自本机回环时
// 被采信；远程连接声称 console 一律按 ssh（网络会话）处理。本文件两组用例：
//  ① 纯函数矩阵（cliSessionSource / loopbackOrigin）；
//  ② api 层 A/B：远程声称 console 改管理口 ⇒ 被守卫（要求 commit confirmed）；
//     本机回环 console ⇒ 放行；本机 ssh（不声称）⇒ 受守卫（FR 原文语义不变）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCLISessionSourceFactBased(t *testing.T) {
	cases := []struct {
		name     string
		remote   string
		claimed  string
		expected string
	}{
		{"回环+console → console（采信）", "127.0.0.1:40001", "console", "console"},
		{"IPv6 回环+console → console", "[::1]:40001", "console", "console"},
		{"回环+ssh → ssh", "127.0.0.1:40001", "ssh", "ssh"},
		{"远程+console → ssh（伪造不采信）", "192.168.1.50:40001", "console", "ssh"},
		{"远程+空 → ssh（默认）", "192.168.1.50:40001", "", "ssh"},
		{"回环+空 → ssh（默认，不自动当 console）", "127.0.0.1:40001", "", "ssh"},
		{"远程 IPv6+console → ssh", "[2001:db8::1]:40001", "console", "ssh"},
		{"RemoteAddr 无端口（保守按非回环）", "192.168.1.50", "console", "ssh"},
		{"RemoteAddr 空（保守按非回环）", "", "console", "ssh"},
		{"console 大小写/空白（原样非 console）", "127.0.0.1:40001", " console ", "console"},
		// round51 评审 M1：自述值不再原样透传——内部维护来源（服务端在恢复/出厂路径自设，
		// 客户端不可声明）与任何未在白名单里的值一律归入网络会话 ssh。
		{"远程+system-restore → ssh（伪造豁免不采信）", "192.168.1.50:40001", "system-restore", "ssh"},
		{"远程+system-zeroize → ssh", "192.168.1.50:40001", "system-zeroize", "ssh"},
		{"远程+system-format-data → ssh", "192.168.1.50:40001", "system-format-data", "ssh"},
		{"回环+system-restore → ssh（内部来源永不采信）", "127.0.0.1:40001", "system-restore", "ssh"},
		{"远程+api/任意未知值 → ssh（白名单外）", "192.168.1.50:40001", "api", "ssh"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/v1/cli/execute", nil)
			r.RemoteAddr = tc.remote
			if got := cliSessionSource(r, tc.claimed); got != tc.expected {
				t.Fatalf("cliSessionSource(remote=%q, claimed=%q) = %q，期望 %q",
					tc.remote, tc.claimed, got, tc.expected)
			}
		})
	}
}

// execVia 以受控 RemoteAddr 驱动一次 /cli/execute（Bearer 已认证），返回 CLI 输出文本。
func execVia(t *testing.T, ts *httptest.Server, token, remoteAddr, source, line string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"line": line, "source": source})
	req := httptest.NewRequest(http.MethodPost, APIPrefix+"/cli/execute", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("execute 应 200，得 %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应: %v (%s)", err, rec.Body.String())
	}
	return out.Output
}

// 管理口变更三步（逐条执行，与 nfvis-cli 前端行为一致）。
func mgmtChangeVia(t *testing.T, ts *httptest.Server, token, remoteAddr, source string) string {
	t.Helper()
	execVia(t, ts, token, remoteAddr, source, "configure")
	execVia(t, ts, token, remoteAddr, source, "set system management interface ens160")
	return execVia(t, ts, token, remoteAddr, source, "commit")
}

func TestCLIExecuteRemoteConsoleClaimGuarded(t *testing.T) {
	ts := newTestServerOpts(t, Options{})
	token := loginAdmin(t, ts)

	// ① 远程连接声称 console：物理上不可能是本地串口会话 ⇒ 按 ssh 处理 ⇒ 自锁守卫生效
	out := mgmtChangeVia(t, ts, token, "192.168.1.50:40001", "console")
	if !strings.Contains(out, "commit confirmed") {
		t.Fatalf("远程伪造 console 应被自锁守卫拒绝（要求 commit confirmed），实得: %s", out)
	}

	// ② 本机回环 + 不声称 console（SSH 语义）：同样受守卫（FR 原文不变）
	ts2 := newTestServerOpts(t, Options{})
	token2 := loginAdmin(t, ts2)
	out2 := mgmtChangeVia(t, ts2, token2, "127.0.0.1:40001", "")
	if !strings.Contains(out2, "commit confirmed") {
		t.Fatalf("本机 ssh 会话应受自锁守卫，实得: %s", out2)
	}

	// ③ 本机回环 + 声称 console：采信 ⇒ 放行（救援路径保留）
	ts3 := newTestServerOpts(t, Options{})
	token3 := loginAdmin(t, ts3)
	out3 := mgmtChangeVia(t, ts3, token3, "127.0.0.1:40001", "console")
	if strings.Contains(out3, "commit confirmed") {
		t.Fatalf("本机 console 会话应放行管理口变更，实得: %s", out3)
	}
	if !strings.Contains(out3, "成功") {
		t.Fatalf("本机 console 提交应成功，实得: %s", out3)
	}
}

// TestCLIExecuteMaintenanceSourceClaimGuarded（round51 评审 M1）：远程连接声称**内部维护来源**
// （恢复出厂/恢复配置/重置数据分区）不得借 `confirmGuardExempt` 的豁免绕过管理口自锁守卫——
// 这三类来源只在服务端恢复/出厂路径上设置，CLI 桥对任何非回环 console 的自述一律按 ssh。
func TestCLIExecuteMaintenanceSourceClaimGuarded(t *testing.T) {
	for _, src := range []string{"system-restore", "system-zeroize", "system-format-data"} {
		t.Run(src, func(t *testing.T) {
			ts := newTestServerOpts(t, Options{})
			token := loginAdmin(t, ts)
			out := mgmtChangeVia(t, ts, token, "192.168.1.50:40001", src)
			if !strings.Contains(out, "commit confirmed") {
				t.Fatalf("远程声称 %s 应仍受管理口自锁守卫（要求 commit confirmed），实得: %s", src, out)
			}
			if strings.Contains(out, "成功") {
				t.Fatalf("远程声称 %s 不得放行管理口变更，实得: %s", src, out)
			}
		})
	}
}

// TestCLIExecuteMaintenanceSourceClaimKeepsHighRiskAudit（round51 评审 M1 第二面）：伪造内部
// 维护来源**不得**抹掉高危变更的意图审计——`highRiskConfigIntent` 对内部来源返回空串（那三类
// 动作由操作级助手记两行），若采信客户端自述，远程用户改本地用户时审计里就只有结果行、看不出
// 「要做什么」。归一为 ssh 后，意图行照落。
func TestCLIExecuteMaintenanceSourceClaimKeepsHighRiskAudit(t *testing.T) {
	ts := newTestServerOpts(t, Options{})
	token := loginAdmin(t, ts)

	execVia(t, ts, token, "192.168.1.50:40001", "system-restore", "configure")
	execVia(t, ts, token, "192.168.1.50:40001", "system-restore",
		"set system login user r51-ops password R51-Passw0rd class operator")
	out := execVia(t, ts, token, "192.168.1.50:40001", "system-restore", "commit")

	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/audit-logs?limit=200", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET audit-logs: %d %s", status, data)
	}
	var entries []struct {
		Detail string `json:"detail"`
		Result string `json:"result"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("解析审计: %v (%s)", err, data)
	}
	found := false
	for _, e := range entries {
		if e.Result == "intent" && strings.Contains(e.Detail, "创建本地用户 r51-ops") {
			found = true
		}
	}
	if !found {
		t.Fatalf("高危意图行被抹掉（远程伪造内部维护来源应仍落意图审计）：commit=%q entries=%+v", out, entries)
	}
}
