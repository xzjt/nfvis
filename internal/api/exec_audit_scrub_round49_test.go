package api

// round49（审计脱敏收口）：容器 exec 的**命令原文**进审计，而命令里可能带口令（`mysql -p…`、
// `--password=…`、`Authorization: Bearer …`、内嵌 PEM）。审计有多个出口，故三层都钉：
//
//  1. 写侧（REST container.go / CLI cli_compute_oper.go）：入库前先脱敏——本文件直接读
//     **引擎里的原始审计**（不经任何出口）来证明，避免被读侧脱敏"顺手遮住"。
//  2. 读侧（GET /audit-logs）：覆盖修复前入库的历史明文行。
//
// openapi 承诺「口令、口令哈希与证书/私钥正文一律不入审计」——判据以这条承诺为准。

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/orchestrator/container"
)

// newServerWithEngine 与 newTestServerOpts 同款装配，但把引擎交出来——
// 本文件要读**入库后的原始审计**（出口之外的第三只眼）。
func newServerWithEngine(t *testing.T, opts Options) (*httptest.Server, *config.Engine) {
	t.Helper()
	store, err := config.OpenStore(filepath.Join(t.TempDir(), "nfvis.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	engine, err := config.NewEngine(store, orchestrator.NewNoopApplier(), config.Options{})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(engine.Close)
	authz := aaa.NewService(engine, nil)
	if _, _, err := aaa.EnsureBootstrapAdmin(engine, authz, "s3cret-Passw0rd!"); err != nil {
		t.Fatalf("引导 admin: %v", err)
	}
	opts.Addr = ":0"
	opts.Log = slog.New(slog.DiscardHandler)
	ts := httptest.NewServer(New(engine, authz, opts).Handler())
	t.Cleanup(ts.Close)
	return ts, engine
}

// 写侧：REST 的 exec 审计在**入库前**就掩码了口令（原始审计里也读不到明文）。
func TestExecAuditScrubsSecretRESTWriteSide(t *testing.T) {
	ct := &execFakeCT{fakeCLIContainer: newFakeCLIContainer()}
	ts, engine := newServerWithEngine(t, Options{Containers: ct})
	token := loginAdmin(t, ts)
	seedContainerCommit(t, ts, token, "ct-a")

	ct.execRes = container.ExecResult{ExitCode: 0, HasExitCode: true, Stdout: "ok\n"}
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/container-functions/ct-a:exec", token,
		map[string]any{"command": "mysql -p123456 -h 10.0.0.1"}, nil)
	if status != http.StatusOK {
		t.Fatalf("exec 应 200: %d %s", status, data)
	}
	detail := rawExecAuditDetail(t, engine)
	if strings.Contains(detail, "123456") {
		t.Fatalf("命令里的口令不得进审计（入库前就该掩码）: %s", detail)
	}
	if !strings.Contains(detail, "«已隐藏»") {
		t.Fatalf("应留下脱敏占位（脱敏≠掏空，且要看得出哪段被动过）: %s", detail)
	}
	if !strings.Contains(detail, "mysql -p") {
		t.Fatalf("非秘密部分应保留（审计的可追溯性）: %s", detail)
	}
}

// 写侧：CLI 的 exec 审计同上（同一口径，别只修一面）。
func TestExecAuditScrubsSecretCLIWriteSide(t *testing.T) {
	x, engine := newCLIKit(t)
	seedContainerConfig(t, x)
	ct := newFakeCLIContainer()
	x.setComputeRuntime(nil, nil, nil, ct, nil)

	ct.execResult = container.ExecResult{ExitCode: 0, HasExitCode: true}
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh",
		`request container-functions sbc-ct1 exec "mysqldump --password=hunter2 app"`)
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("正常执行不该报失败: %s", res.Output)
	}
	detail := rawExecAuditDetail(t, engine)
	if strings.Contains(detail, "hunter2") {
		t.Fatalf("命令里的口令不得进审计（入库前就该掩码）: %s", detail)
	}
	if !strings.Contains(detail, "«已隐藏»") {
		t.Fatalf("应留下脱敏占位: %s", detail)
	}
}

// 读侧：修复前入库的**历史明文行**经 GET /audit-logs 也必须掩码（否则改前存下的口令
// 照样被只读账号读走——read-only 就能读本端点）。
func TestAuditLogsEndpointScrubsLegacyPlaintext(t *testing.T) {
	ts, engine := newServerWithEngine(t, Options{})
	token := loginAdmin(t, ts)
	// 模拟「修复前入库」的记录：直接写引擎（不经端点，故写侧脱敏不参与）。
	engine.Audit("admin", "container.exec", "exec 容器 ct-a: mysql -p987654 -h db", "success")
	engine.Audit("admin", "container.exec", `exec 容器 api: curl -H 'Authorization: Bearer legacyToken' http://x`, "success")

	rows := auditRows(t, ts, token)
	joined := ""
	found := 0
	for _, r := range rows {
		if r.Action != "container.exec" {
			continue
		}
		joined += r.Detail + "\n"
		found++
	}
	if found == 0 {
		t.Fatal("应有 container.exec 审计行")
	}
	for _, leak := range []string{"987654", "legacyToken"} {
		if strings.Contains(joined, leak) {
			t.Fatalf("历史明文 %q 不得经 /audit-logs 出口: %s", leak, joined)
		}
	}
	if !strings.Contains(joined, "«已隐藏»") {
		t.Fatalf("应留下脱敏占位: %s", joined)
	}
}

// rawExecAuditDetail 取引擎里最新的 container.exec 审计 detail（**不经出口**，故能证明写侧）。
func rawExecAuditDetail(t *testing.T, engine *config.Engine) string {
	t.Helper()
	entries, err := engine.AuditTrail(50, 0)
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	for _, e := range entries {
		if e.Action == "container.exec" {
			return e.Detail
		}
	}
	t.Fatal("没有 container.exec 审计记录")
	return ""
}
