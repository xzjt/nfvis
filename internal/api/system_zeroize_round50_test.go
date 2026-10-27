package api

// 决策 #517（round50 只读评审，接 #500 口径）：zeroize 部分失败在 REST/CLI/审计三个出口如实上报。
//
//   - REST `POST /system:zeroize`：有残留 ⇒ **500**，message 逐条列出残留（与 format-data 同形），
//     不再回 202 `zeroized`；
//   - CLI `request system zeroize`：有残留 ⇒ `%%` 报失败，**不得**打印「已恢复出厂」；
//   - 审计：runHighRisk 因而记 failure（此前记 success）。

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/system"
)

// zeroizeResidueStub 造一个「账号已复位但镜像删不掉」的部分失败桩。
func zeroizeResidueStub() stubSysOps {
	const residue = "镜像 stuck.qcow2：删除失败: 设备忙"
	return stubSysOps{
		failZeroize: errors.New("恢复出厂未清干净：1 项残留——" + residue),
		zeroizeRes: system.ZeroizeResult{
			Revision: 7, RemovedImages: 1, Residuals: []string{residue},
		},
	}
}

// TestZeroizeRESTSurfacesResidue500：有残留 ⇒ 500 + message 列出残留 + detail 逐条（与 format-data 同形）。
func TestZeroizeRESTSurfacesResidue500(t *testing.T) {
	ts := newTestServerOpts(t, Options{SysOps: zeroizeResidueStub()})
	token := loginAdmin(t, ts)

	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system:zeroize", token,
		map[string]any{"confirm": true}, nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("有残留应 500（不是 %d）: %s", status, data)
	}
	if !strings.Contains(string(data), "stuck.qcow2") || !strings.Contains(string(data), "残留") {
		t.Fatalf("500 message 应逐条列出残留: %s", data)
	}
	// detail[] 里也应逐条（客户端无需解析文本）。
	if !strings.Contains(string(data), `"detail"`) {
		t.Fatalf("500 应带 detail 残留清单: %s", data)
	}

	// 成功路径仍 202（回归）。
	ts2 := newTestServerOpts(t, Options{SysOps: stubSysOps{}})
	token2 := loginAdmin(t, ts2)
	status, _, data = cfgRequest(t, http.MethodPost, ts2.URL+APIPrefix+"/system:zeroize", token2,
		map[string]any{"confirm": true}, nil)
	if status != http.StatusAccepted || !strings.Contains(string(data), "zeroized") {
		t.Fatalf("成功应 202 zeroized: %d %s", status, data)
	}
}

// TestCLIZeroizePartialFailureFails：CLI 有残留 ⇒ `%%` 报失败，绝不打印「已恢复出厂」。
func TestCLIZeroizePartialFailureFails(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setSystemOps(zeroizeResidueStub())

	// 双重确认：两次问询后第三次（两个 --yes）执行。
	_ = x.Execute("admin", "super-user", "ssh", "request system zeroize")
	_ = x.Execute("admin", "super-user", "ssh", "request system zeroize --yes")
	res := x.Execute("admin", "super-user", "ssh", "request system zeroize --yes --yes")
	if !strings.Contains(res.Output, "%%") {
		t.Fatalf("有残留应以 %% 报失败: %q", res.Output)
	}
	if strings.Contains(res.Output, "已恢复出厂") {
		t.Fatalf("有残留不得打印「已恢复出厂」: %q", res.Output)
	}
	if !strings.Contains(res.Output, "stuck.qcow2") {
		t.Fatalf("失败文案应含残留对象: %q", res.Output)
	}
	// 部分成功的统计也要给出（本会话新增的失败分支），便于对照「清掉了哪些」。
	if !strings.Contains(res.Output, "本次已删除镜像 1 个") {
		t.Fatalf("失败文案应带已清空的统计: %q", res.Output)
	}
}

// TestZeroizeAuditFailure：有残留时 runHighRisk 记 failure（此前记 success）。
func TestZeroizeAuditFailure(t *testing.T) {
	ts := newTestServerOpts(t, Options{SysOps: zeroizeResidueStub()})
	token := loginAdmin(t, ts)
	before := auditRows(t, ts, token)

	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system:zeroize", token,
		map[string]any{"confirm": true}, nil); status != http.StatusInternalServerError {
		t.Fatalf("有残留应 500，实得 %d", status)
	}
	after := auditRows(t, ts, token)
	rows := newRows(t, before, after)
	assertPair(t, rows, "system.zeroize", "恢复出厂", "failure")
}

// 编译期：桩满足 CLI/REST 共用的 SystemOpsRuntime 接口。
var _ SystemOpsRuntime = stubSysOps{}
