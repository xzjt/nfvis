package cliclient

// L5（round51 评审）：升级/回退的「重连复核」会把**上一次**留下的成功结论当成本次结果——
// 客户端只解 `结果/版本/状态`，既不比对服务端渲染的「时间」，也不与命令行能推出的目标版本
// 对照。于是对一次**根本没跑成**的动作，操作者看到「结论: 成功」+ 退出码 0（谎报成功）。
//
// 修法：把记录**归因到本次尝试**（时间不早于发起时刻；能推出目标版本时版本还必须一致），
// 归因不上就继续在既有有界窗口内等，等到头如实报「未定」并带上最后一次复核错误。
// 本文件用假服务端复现「命令连接被断开 + 结论文件仍在」的现场。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// shrinkRecheck 缩短复核窗口与轮询间隔（产品缺省 240s/2s，测试用毫秒级）。
func shrinkRecheck(t *testing.T) {
	t.Helper()
	oldWait, oldIv := upgradeRestartWait, recheckInterval
	upgradeRestartWait, recheckInterval = 300*time.Millisecond, 15*time.Millisecond
	t.Cleanup(func() { upgradeRestartWait, recheckInterval = oldWait, oldIv })
}

// upgradeDropServer 假 nfvisd：升级行**断开连接**（复现 postinst 重启现场），
// 其余 /cli/execute 回一份 `show system software result` 的渲染文本（record 函数给出），
// /login 照常发 token（重连复核要先重登）。
func upgradeDropServer(t *testing.T, upgradeLine string, record func() string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/login":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"tok2","user":{"class":"super-user"}}`))
			return
		case "/api/v1/cli/execute":
			var req struct {
				Line string `json:"line"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Line == upgradeLine {
				// 断开连接：客户端看到 EOF/connection reset，进而走重连复核（与真机同形）。
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Errorf("测试服务端不支持 Hijack")
					return
				}
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.Close()
				}
				return
			}
			w.Header().Set("Content-Type", "application/json")
			payload := fmt.Sprintf(`{"output":%q,"mode":"oper","prompt":"nfvis> "}`, record())
			_, _ = w.Write([]byte(payload))
			return
		}
		http.NotFound(w, r)
	}))
}

// TestUpgradeRecheckRefusesStaleSuccessRecord 核心回归：留在结论文件里的**旧成功记录**
// 不得当成本次结果——报「未定」且绝不出现「结论: 成功」。
func TestUpgradeRecheckRefusesStaleSuccessRecord(t *testing.T) {
	shrinkRecheck(t)
	const line = "request system software add /tmp/nfvis_3.0.33_amd64.deb"
	stale := fmt.Sprintf("结果: 成功\n动作: 升级\n时间: %s\n版本: 3.0.32\n状态: install ok installed\n",
		time.Now().Add(-time.Hour).UTC().Format(time.RFC3339))
	srv := upgradeDropServer(t, line, func() string { return stale })
	defer srv.Close()

	c, err := NewWithTLS(srv.URL, TLSOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Login("admin", "pw"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	res, err := c.Execute(line, "ssh")
	if err == nil {
		t.Fatalf("旧成功记录不得当成本次结果（应报未定），实得输出: %q", res.Output)
	}
	if strings.Contains(res.Output, "结论: 成功") || strings.Contains(err.Error(), "结论: 成功") {
		t.Fatalf("不得把旧记录报成本次成功: %v", err)
	}
	for _, want := range []string{"未定", "不属于本次尝试", "最后一次复核错误"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错应含 %q（归因不上要如实说清），实得: %v", want, err)
		}
	}
}

// TestUpgradeRecheckAcceptsThisAttemptsRecord 正控：本次尝试的记录（时间在发起之后、
// 版本与命令行推出的目标一致）必须照旧报成功，文案逐字不变。
func TestUpgradeRecheckAcceptsThisAttemptsRecord(t *testing.T) {
	shrinkRecheck(t)
	const line = "request system software add /tmp/nfvis_3.0.33_amd64.deb"
	fresh := fmt.Sprintf("结果: 成功\n动作: 升级\n时间: %s\n版本: 3.0.33\n状态: install ok installed\n",
		time.Now().Add(5*time.Second).UTC().Format(time.RFC3339))
	srv := upgradeDropServer(t, line, func() string { return fresh })
	defer srv.Close()

	c, err := NewWithTLS(srv.URL, TLSOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Login("admin", "pw"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	res, err := c.Execute(line, "ssh")
	if err != nil {
		t.Fatalf("本次尝试的成功记录应被接受: %v", err)
	}
	want := "升级动作已受理：nfvisd 因此重启，本次连接已断开（预期行为，不是失败）。\n" +
		"已自动重连并读取服务端记录的实际结果：\n  版本: 3.0.33\n  状态: install ok installed\n结论: 成功。\n"
	if res.Output != want {
		t.Fatalf("成功文案应与既有口径逐字一致：\n got %q\nwant %q", res.Output, want)
	}
}

// TestUpgradeRecheckRefusesVersionMismatch 版本不一致的**新**记录（时间够新）也不算本次结果：
// 版本维度是第二道归因（时间维度的补充）。
func TestUpgradeRecheckRefusesVersionMismatch(t *testing.T) {
	shrinkRecheck(t)
	const line = "request system software rollback to 3.0.31"
	mismatch := fmt.Sprintf("结果: 成功\n动作: 回退\n时间: %s\n版本: 3.0.30\n状态: install ok installed\n",
		time.Now().Add(5*time.Second).UTC().Format(time.RFC3339))
	srv := upgradeDropServer(t, line, func() string { return mismatch })
	defer srv.Close()

	c, err := NewWithTLS(srv.URL, TLSOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Login("admin", "pw"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	_, err = c.Execute(line, "ssh")
	if err == nil {
		t.Fatal("版本与本次目标不符的记录不得报成功")
	}
	if !strings.Contains(err.Error(), "3.0.30") || !strings.Contains(err.Error(), "3.0.31") {
		t.Fatalf("报错应点出记录版本与目标版本: %v", err)
	}
}

// TestTargetVersionOf 目标版本推导：只认命令行里能确定的东西（本地 .deb 路径的版本段、
// `rollback to <v>`），推不出就返回空串（交给时间归因）。
func TestTargetVersionOf(t *testing.T) {
	cases := []struct{ line, want string }{
		{"request system software add /tmp/nfvis_3.0.33_amd64.deb", "3.0.33"},
		{"request system software add nfvis_3.0.33~dev2_amd64.deb --yes", "3.0.33~dev2"},
		{"request system software add /var/tmp/x/broken.deb", ""},
		{"request system software add https://example.com/nfvis_3.0.33_amd64.deb", ""},
		{"request system software rollback to 3.0.31", "3.0.31"},
		{"request system software rollback", ""},
		{"show version", ""},
	}
	for _, tc := range cases {
		if got := targetVersionOf(tc.line); got != tc.want {
			t.Errorf("targetVersionOf(%q) = %q，期望 %q", tc.line, got, tc.want)
		}
	}
}

// TestUnattributable 归因判据（纯函数）：旧时间、版本不一致都要判「归因不上」，
// 无时间字段时退回版本判据。
func TestUnattributable(t *testing.T) {
	issued := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		recTime time.Time
		ver     string
		target  string
		want    bool // true = 归因不上
	}{
		{"本次（时间新、版本符）", issued.Add(30 * time.Second), "3.0.33", "3.0.33", false},
		{"旧记录（时间早）", issued.Add(-time.Hour), "3.0.33", "3.0.33", true},
		{"版本不符", issued.Add(30 * time.Second), "3.0.32", "3.0.33", true},
		{"无版本→只判时间", issued.Add(30 * time.Second), "", "", false},
		{"无版本且时间早", issued.Add(-time.Minute), "", "", true},
		{"无时间字段但版本符", time.Time{}, "3.0.33", "3.0.33", false},
		{"无时间字段且版本不符", time.Time{}, "3.0.32", "3.0.33", true},
		{"无时间字段且推不出目标", time.Time{}, "3.0.33", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			why := unattributable(tc.recTime, tc.ver, issued, tc.target)
			if (why != "") != tc.want {
				t.Fatalf("unattributable(%v,%q,%v,%q) = %q，期望归因不上=%v",
					tc.recTime, tc.ver, issued, tc.target, why, tc.want)
			}
		})
	}
}
