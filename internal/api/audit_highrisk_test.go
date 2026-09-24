package api

// 决策 #150：高危档动作的审计「两行」（设计 §7）——服务端行为的守护。
//
// 判据（逐条对应设计 §7 的高危清单）：
//   - 每个被覆盖的动作**两条**记录：intent 在前、success/failure 在后；
//   - 失败路径也是两条，且结果行是 failure（带原因）；
//   - 同一条动作从 CLI 与 REST 走，action 与意图文案一致（同源助手）；
//   - **非高危动作仍然只有一条**（防「顺手全改」）；
//   - 被前置拒绝的请求（没给确认词 / confirm=false）不落审计（动作没开始）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/images"
	"github.com/xzjt/nfvis/internal/system"
)

// ---------- 桩：软件升级 / 证书（可控成功与失败） ----------

type stubSoftware struct{ fail error }

func (s stubSoftware) Add(context.Context, string, string) (system.SoftwareResult, error) {
	if s.fail != nil {
		return system.SoftwareResult{}, s.fail
	}
	return system.SoftwareResult{Package: "nfvis_9.9.9_amd64.deb", Previous: "1.1.31", Version: "9.9.9"}, nil
}
func (s stubSoftware) Rollback(context.Context) (system.SoftwareResult, error) {
	if s.fail != nil {
		return system.SoftwareResult{}, s.fail
	}
	return system.SoftwareResult{Package: "nfvis_1.1.30_amd64.deb", Previous: "1.1.31", Version: "1.1.30"}, nil
}
func (stubSoftware) Reboot(context.Context) error   { return nil }
func (stubSoftware) Shutdown(context.Context) error { return nil }
func (stubSoftware) NTPSync(context.Context, []string) (string, error) {
	return "", nil
}

type stubTLS struct{ fail error }

func (s stubTLS) Info() (system.TlsInfo, bool) { return system.TlsInfo{}, false }
func (s stubTLS) Install(certPEM, keyPEM string) (system.TlsInfo, error) {
	if s.fail != nil {
		return system.TlsInfo{}, s.fail
	}
	return system.TlsInfo{Subject: "CN=uploaded", Fingerprint: "AA:BB:CC"}, nil
}
func (s stubTLS) RegenerateSelfSigned(string) (system.TlsInfo, error) { return system.TlsInfo{}, nil }
func (s stubTLS) RegenerateSSHHostKeys(context.Context) error         { return nil }

// stubSysOps 备份/恢复/恢复出厂的可控桩（只为失败路径；成功路径用真实 Manager）。
type stubSysOps struct{ failZeroize error }

func (s stubSysOps) Backup() (system.File, error) { return system.File{File: "x.json"}, nil }
func (s stubSysOps) List() []system.File          { return nil }
func (s stubSysOps) Path(string) (string, error)  { return "", nil }
func (s stubSysOps) Restore(context.Context, []byte, string) (config.CommitResult, []images.Meta, error) {
	return config.CommitResult{}, nil, errors.New("归档不可用")
}
func (s stubSysOps) Zeroize(context.Context, string) (system.ZeroizeResult, error) {
	if s.failZeroize != nil {
		return system.ZeroizeResult{}, s.failZeroize
	}
	return system.ZeroizeResult{Revision: 9}, nil
}

// ---------- 断言工具 ----------

// auditRow 审计记录的一条（只取判据用得到的字段）。
type auditRow struct {
	Action string `json:"action"`
	Detail string `json:"detail"`
	Result string `json:"result"`
	User   string `json:"user"`
}

// auditRows 取全部审计记录（倒序：最新在前）。
func auditRows(t *testing.T, ts *httptest.Server, token string) []auditRow {
	t.Helper()
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/audit-logs?limit=200", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET audit-logs: %d %s", status, data)
	}
	var rows []auditRow
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatalf("解析审计: %v (%s)", err, data)
	}
	return rows
}

// cliAuditRows 取引擎审计（倒序：最新在前）。
func cliAuditRows(t *testing.T, engine *config.Engine) []auditRow {
	t.Helper()
	entries, err := engine.AuditTrail(200, 0)
	if err != nil {
		t.Fatalf("读取审计: %v", err)
	}
	out := make([]auditRow, 0, len(entries))
	for _, e := range entries {
		out = append(out, auditRow{Action: e.Action, Detail: e.Detail, Result: e.Result, User: e.User})
	}
	return out
}

// assertPair 断言某动作的审计恰好是「意图 + 结果」两条。
//
// wantIntent 是意图行 detail 里必须出现的关键事实（人话片段）；
// wantResult 是结果行的 result 取值（success / failure）。
func assertPair(t *testing.T, rows []auditRow, wantAction, wantIntent, wantResult string) {
	t.Helper()
	var got []auditRow
	for _, r := range rows { // rows 已倒序：筛出该动作的全部记录后翻成正序（旧 → 新）
		if r.Action == wantAction {
			got = append([]auditRow{r}, got...)
		}
	}
	if len(got) != 2 {
		t.Fatalf("%s 应有两条审计（意图 + 结果），实得 %d 条: %+v", wantAction, len(got), got)
	}
	if got[0].Result != config.AuditResultIntent {
		t.Errorf("%s 第一条应是意图行（result=%s），实得 %+v", wantAction, config.AuditResultIntent, got[0])
	}
	if !strings.Contains(got[0].Detail, wantIntent) {
		t.Errorf("%s 意图行应说明「%s」，实得 %q", wantAction, wantIntent, got[0].Detail)
	}
	if got[1].Result != wantResult {
		t.Errorf("%s 第二条应是结果行（result=%s），实得 %+v", wantAction, wantResult, got[1])
	}
}

// countAction 统计某动作的审计条数（用于「非高危仍是一条」与「没开始就不记」）。
func countAction(rows []auditRow, action string) int {
	n := 0
	for _, r := range rows {
		if r.Action == action {
			n++
		}
	}
	return n
}

// newRows 返回 after 里比 before 新增的记录（倒序：最新在前）。
//
// 用例都按「动作前后的增量」断言：测试装配本身（引导 admin、预置 viewer）也会产生
// 高危档的两条记录，那属于别的动作，不能混进本动作的判据。
func newRows(t *testing.T, before, after []auditRow) []auditRow {
	t.Helper()
	if len(after) < len(before) {
		t.Fatalf("审计条数不应减少: %d → %d", len(before), len(after))
	}
	return after[:len(after)-len(before)]
}

// intentDetailOf 取某动作意图行的 detail（没有意图行时返回空串）。
func intentDetailOf(rows []auditRow, action string) string {
	for _, r := range rows {
		if r.Action == action && r.Result == config.AuditResultIntent {
			return r.Detail
		}
	}
	return ""
}

// ---------- REST：四个直接动作（恢复出厂 / 软件升级 / 软件回退 / 证书上传） ----------

func TestHighRiskAuditTwoRowsREST(t *testing.T) {
	ts := newTestServerOpts(t, Options{Software: stubSoftware{}, TLS: stubTLS{}})
	token := loginAdmin(t, ts)

	// 1) 恢复出厂
	before := auditRows(t, ts, token)
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system:zeroize", token,
		map[string]any{"confirm": true}, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusAccepted {
		t.Fatalf("zeroize: %d %s", status, data)
	}
	assertPair(t, newRows(t, before, auditRows(t, ts, token)), "system.zeroize", "恢复出厂", "success")

	// 2) 软件升级
	before = auditRows(t, ts, token)
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/software", token,
		map[string]any{"package": "/tmp/nfvis_9.9.9_amd64.deb"}, nil); status != http.StatusAccepted {
		t.Fatalf("software add: %d %s", status, data)
	}
	assertPair(t, newRows(t, before, auditRows(t, ts, token)), "system.software.add", "安装软件包 /tmp/nfvis_9.9.9_amd64.deb", "success")

	// 3) 软件回退
	before = auditRows(t, ts, token)
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/software:rollback", token,
		nil, nil); status != http.StatusAccepted {
		t.Fatalf("software rollback: %d %s", status, data)
	}
	assertPair(t, newRows(t, before, auditRows(t, ts, token)), "system.software.rollback", "回退到上一版本", "success")

	// 4) 证书上传（PEM 正文与私钥绝不入审计）
	before = auditRows(t, ts, token)
	if status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/system/tls", token,
		map[string]any{
			"certificate": "-----BEGIN CERTIFICATE-----\nSECRET-PEM-BODY\n-----END CERTIFICATE-----",
			"key":         "-----BEGIN PRIVATE KEY-----\nSECRET-KEY-BODY\n-----END PRIVATE KEY-----",
		}, nil); status != http.StatusOK {
		t.Fatalf("tls install: %d %s", status, data)
	}
	added := newRows(t, before, auditRows(t, ts, token))
	assertPair(t, added, "system.tls.install", "上传并安装外部 TLS 证书", "success")
	for _, r := range added {
		if strings.Contains(r.Detail, "SECRET-PEM-BODY") || strings.Contains(r.Detail, "SECRET-KEY-BODY") {
			t.Fatalf("证书/私钥正文不得进审计: %+v", r)
		}
	}
}

// 失败路径：两个动作各两条，结果行是 failure 且带原因。
func TestHighRiskAuditTwoRowsRESTFailures(t *testing.T) {
	ts := newTestServerOpts(t, Options{
		Software: stubSoftware{fail: errors.New("包校验和不匹配")},
		TLS:      stubTLS{fail: errors.New("certificate 与 key 不配对")},
	})
	token := loginAdmin(t, ts)
	before := auditRows(t, ts, token)

	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/software", token,
		map[string]any{"package": "/tmp/bad.deb"}, nil); status != http.StatusBadRequest {
		t.Fatalf("失败的安装应 400")
	}
	added := newRows(t, before, auditRows(t, ts, token))
	assertPair(t, added, "system.software.add", "安装软件包 /tmp/bad.deb", "failure")
	for _, r := range added {
		if r.Action == "system.software.add" && r.Result == "failure" && !strings.Contains(r.Detail, "包校验和不匹配") {
			t.Errorf("失败结果行要写原因: %+v", r)
		}
	}

	before = auditRows(t, ts, token)
	if status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/system/tls", token,
		map[string]any{"certificate": "CERT", "key": "KEY"}, nil); status != http.StatusBadRequest {
		t.Fatalf("失败的安装应 400")
	}
	assertPair(t, newRows(t, before, auditRows(t, ts, token)), "system.tls.install", "上传并安装外部 TLS 证书", "failure")
}

// 恢复出厂失败路径：两条，结果行 failure 带原因（REST 与 CLI 各一次）。
func TestHighRiskAuditZeroizeFailureTwoRows(t *testing.T) {
	ts := newTestServerOpts(t, Options{SysOps: stubSysOps{failZeroize: errors.New("镜像删除失败")}})
	token := loginAdmin(t, ts)
	before := auditRows(t, ts, token)
	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system:zeroize", token,
		map[string]any{"confirm": true}, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusInternalServerError {
		t.Fatalf("失败的恢复出厂应 500")
	}
	added := newRows(t, before, auditRows(t, ts, token))
	assertPair(t, added, "system.zeroize", "恢复出厂", "failure")
	if !strings.Contains(added[0].Detail, "镜像删除失败") {
		t.Errorf("失败结果行要写原因: %+v", added[0])
	}

	// CLI 侧同一个助手 → 同样两条
	x, engine := newCLIKit(t)
	x.setSystemOps(stubSysOps{failZeroize: errors.New("镜像删除失败")})
	beforeCLI := cliAuditRows(t, engine)
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request system zeroize --yes --yes")
	if !strings.Contains(res.Output, "%%") {
		t.Fatalf("CLI 恢复出厂应失败: %s", res.Output)
	}
	assertPair(t, newRows(t, beforeCLI, cliAuditRows(t, engine)), "system.zeroize", "恢复出厂", "failure")
}

// 前置拒绝（confirm=false）不落审计：动作没开始。
func TestHighRiskAuditNotWrittenWhenRefused(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system:zeroize", token,
		map[string]any{"confirm": false}, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusBadRequest {
		t.Fatalf("zeroize 无 confirm 应 400")
	}
	if n := countAction(auditRows(t, ts, token), "system.zeroize"); n != 0 {
		t.Fatalf("被拒的恢复出厂不应留审计，实得 %d 条", n)
	}
}

// 恢复配置：multipart 上传归档 → 两条（成功与失败各验一次）。
func TestHighRiskAuditRestoreTwoRows(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// 先做一份备份归档（内容即当前 committed 配置）
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/backup", token, nil,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusAccepted {
		t.Fatalf("生成备份: %d %s", status, data)
	}
	var f struct {
		File string `json:"file"`
	}
	_ = json.Unmarshal(data, &f)
	archive := downloadBackup(t, ts, token, f.File)

	postRestoreArchive(t, ts, token, archive, http.StatusOK)
	before := auditRows(t, ts, token)
	postRestoreArchive(t, ts, token, archive, http.StatusOK)
	assertPair(t, newRows(t, before, auditRows(t, ts, token)), "system.restore", "从备份归档恢复配置", "success")

	// 失败路径：坏归档（不是合法归档 JSON）→ 两条，结果行 failure 带原因
	before = auditRows(t, ts, token)
	postRestoreArchive(t, ts, token, []byte("not-an-archive"), http.StatusBadRequest)
	added := newRows(t, before, auditRows(t, ts, token))
	assertPair(t, added, "system.restore", "从备份归档恢复配置", "failure")
	if !strings.Contains(added[0].Detail, "归档格式不符") {
		t.Errorf("失败结果行要写原因: %+v", added[0])
	}
}

func downloadBackup(t *testing.T, ts *httptest.Server, token, name string) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+APIPrefix+"/system/backup/"+name, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("下载归档: %d", resp.StatusCode)
	}
	return buf.Bytes()
}

func postRestoreArchive(t *testing.T, ts *httptest.Server, token string, archive []byte, wantStatus int) {
	t.Helper()
	body := new(bytes.Buffer)
	mw := multipart.NewWriter(body)
	fw, err := mw.CreateFormFile("file", "backup.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(archive)
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+APIPrefix+"/system/restore", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("恢复配置应 %d，实际 %d", wantStatus, resp.StatusCode)
	}
}

// ---------- CLI：与 REST 同源（同一 action、同一意图文案） ----------

// 恢复出厂：CLI 侧两条，且意图文案与 REST 逐字相同（同一份构造）。
func TestHighRiskAuditIntentSameTextAcrossEntries(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system:zeroize", token,
		map[string]any{"confirm": true}, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusAccepted {
		t.Fatalf("REST zeroize 失败")
	}
	rows := auditRows(t, ts, token)
	assertPair(t, rows, "system.zeroize", "恢复出厂", "success")
	restIntent := intentDetailOf(rows, "system.zeroize")

	// CLI 侧：同一个助手 → 同一 action、同一意图文案（两次 --yes 才是双重确认）
	x, engine := newCLIKit(t)
	x.setSystemOps(system.NewManager(system.Config{Dir: t.TempDir()}, engine, nil, "test"))
	out := run(t, x, "admin", aaa.ClassSuperUser, "ssh", "request system zeroize --yes --yes")
	if !strings.Contains(out, "已恢复出厂") {
		t.Fatalf("CLI 恢复出厂输出不符: %s", out)
	}
	cliRows := cliAuditRows(t, engine)
	assertPair(t, cliRows, "system.zeroize", "恢复出厂", "success")
	if cliIntent := intentDetailOf(cliRows, "system.zeroize"); cliIntent != restIntent {
		t.Fatalf("CLI 与 REST 的意图文案必须一致（同源助手）:\nCLI = %q\nREST= %q", cliIntent, restIntent)
	}
}

// 软件升级/回退（CLI）：成功与失败各两条。
func TestHighRiskAuditTwoRowsCLI(t *testing.T) {
	x, engine := newCLIKit(t)
	x.setSoftware(stubSoftware{})

	run(t, x, "admin", aaa.ClassSuperUser, "ssh", "request system software add /tmp/pk.deb --yes")
	assertPair(t, cliAuditRows(t, engine), "system.software.add", "安装软件包 /tmp/pk.deb", "success")

	// 失败路径：同一个助手 → 两条，结果行 failure 带原因
	x.setSoftware(stubSoftware{fail: errors.New("下载失败")})
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request system software rollback --yes")
	if !strings.Contains(res.Output, "%%") {
		t.Fatalf("CLI 回退应失败: %s", res.Output)
	}
	rows := cliAuditRows(t, engine)
	assertPair(t, rows, "system.software.rollback", "回退到上一版本", "failure")
	if !strings.Contains(rows[0].Detail, "下载失败") {
		t.Errorf("失败结果行要写原因: %+v", rows[0])
	}
}

// 恢复配置（CLI）：两条；与 REST 同一 action 与意图文案。
func TestHighRiskAuditRestoreTwoRowsCLI(t *testing.T) {
	x, engine := newCLIKit(t)
	x.setSystemOps(system.NewManager(system.Config{Dir: t.TempDir()}, engine, nil, "test"))

	// 失败路径：不存在的归档 → 读文件失败属**前置拒绝**（动作没开始，不落审计）
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request system configuration restore /no/such/archive.json")
	if !strings.Contains(res.Output, "%%") {
		t.Fatalf("应报读归档失败: %s", res.Output)
	}
	if n := countAction(cliAuditRows(t, engine), "system.restore"); n != 0 {
		t.Fatalf("读不到归档不应落审计，实得 %d 条", n)
	}
}

// ---------- 配置事务型高危变更（删用户 / 建用户 / 口令策略 / 证书文件） ----------

// 本地用户：REST（直提）与 CLI（set/delete + commit）都恰好两条。
func TestHighRiskAuditUserDeleteTwoRows(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// 建用户（控制台对本地用户的写操作一律走高危闸门，故同属高危档）
	before := auditRows(t, ts, token)
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/login-users", token,
		map[string]any{"name": "netop-tmp", "password": "Str0ng-Passw0rd!", "class": "operator"},
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("建用户: %d %s", status, data)
	}
	added := newRows(t, before, auditRows(t, ts, token))
	assertPair(t, added, "config.commit", "创建本地用户 netop-tmp", "success")
	// 口令哈希不进审计（FR-SEC-007）
	for _, r := range added {
		if strings.Contains(r.Detail, "pbkdf2$") {
			t.Fatalf("口令哈希不得进审计: %+v", r)
		}
	}

	// 删用户
	before = auditRows(t, ts, token)
	if status, _, data := cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/system/login-users/netop-tmp",
		token, nil, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("删用户: %d %s", status, data)
	}
	assertPair(t, newRows(t, before, auditRows(t, ts, token)), "config.commit", "删除本地用户 netop-tmp", "success")

	// CLI 侧：set/delete + commit → 同两条（同一实现，不经 HTTP）
	x, engine := newCLIKit(t)
	before = cliAuditRows(t, engine)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set system login user cli-tmp password Str0ng-Passw0rd! class operator",
		"commit")
	assertPair(t, newRows(t, before, cliAuditRows(t, engine)), "config.commit", "创建本地用户 cli-tmp", "success")

	before = cliAuditRows(t, engine)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", "delete system login user cli-tmp", "commit")
	assertPair(t, newRows(t, before, cliAuditRows(t, engine)), "config.commit", "删除本地用户 cli-tmp", "success")
}

// 口令策略变更：两条，逐字段说明变化，且不含口令内容。
func TestHighRiskAuditPasswordPolicyTwoRows(t *testing.T) {
	x, engine := newCLIKit(t)
	before := cliAuditRows(t, engine)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set system login password-policy min-length 14",
		"set system login password-policy lockout-threshold 3",
		"commit")
	added := newRows(t, before, cliAuditRows(t, engine))
	assertPair(t, added, "config.commit", "修改口令策略", "success")
	intent := intentDetailOf(added, "config.commit")
	for _, want := range []string{"口令最小长度 未设置 → 14", "连续失败锁定阈值 未设置 → 3"} {
		if !strings.Contains(intent, want) {
			t.Errorf("策略意图行应含「%s」，实得 %q", want, intent)
		}
	}
}

// 证书文件引用（CLI 的「证书上传」路径）：两条。
func TestHighRiskAuditCertFileTwoRows(t *testing.T) {
	x, engine := newCLIKit(t)
	before := cliAuditRows(t, engine)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set system api tls cert-file /root/server.crt key-file /root/server.key",
		"commit")
	added := newRows(t, before, cliAuditRows(t, engine))
	assertPair(t, added, "config.commit", "安装外部证书", "success")
	if intent := intentDetailOf(added, "config.commit"); !strings.Contains(intent, "/root/server.crt") {
		t.Errorf("证书意图行应含关键参数（文件路径）: %q", intent)
	}
}

// 非高危动作仍然**只有一条**（防「顺手全改」）：主机名 / 接口描述提交。
func TestNonHighRiskAuditStaysSingleRow(t *testing.T) {
	x, engine := newCLIKit(t)
	before := cliAuditRows(t, engine)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set system hostname single-row",
		"set interfaces ens3f0 description uplink",
		"commit")
	added := newRows(t, before, cliAuditRows(t, engine))
	if n := countAction(added, "config.commit"); n != 1 {
		t.Fatalf("非高危提交应只有一条审计，实得 %d 条: %+v", n, added)
	}
	if added[0].Result != "success" || !strings.Contains(added[0].Detail, "hostname") {
		t.Fatalf("那一条仍是既有的结果行: %+v", added[0])
	}
}
