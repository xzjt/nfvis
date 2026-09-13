package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/system"
)

// M5-6：备份 → 恢复出厂 → 恢复 的 API 闭环。
func TestBackupRestoreZeroizeEndpoints(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// 预置一点配置（经 CLI 会话，exit 会释放 candidate 锁，避免影响后续 zeroize/restore）
	for _, line := range []string{
		"configure",
		"set interfaces ens3f0 description to-tor",
		"commit",
		"exit",
	} {
		if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/cli/execute", token,
			map[string]any{"line": line}, nil); status != http.StatusOK {
			t.Fatalf("预置 %q: %d %s", line, status, data)
		}
	}

	// 1) 生成备份
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/backup", token, nil,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusAccepted {
		t.Fatalf("生成备份: %d %s", status, data)
	}
	var f struct {
		File string `json:"file"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &f); err != nil || f.File == "" || f.Kind != "config-backup" {
		t.Fatalf("备份元数据: %s (%v)", data, err)
	}

	// 2) 列表 + 下载
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/backup", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), f.File) {
		t.Fatalf("列表: %d %s", status, data)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+APIPrefix+"/system/backup/"+f.File, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(buf.Bytes(), []byte("nfvis-config-backup")) {
		t.Fatalf("下载归档: %d %s", resp.StatusCode, buf.String())
	}
	archive := buf.Bytes()

	// 3) 恢复出厂（无 confirm → 400）
	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system:zeroize", token,
		map[string]any{"confirm": false}, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusBadRequest {
		t.Fatalf("zeroize 无 confirm 应 400，实际 %d", status)
	}
	// 带 confirm → 202，配置清空
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system:zeroize", token,
		map[string]any{"confirm": true}, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusAccepted {
		t.Fatalf("zeroize: %d %s", status, data)
	}
	if status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/interfaces", token, nil, nil); status != http.StatusOK ||
		strings.Contains(string(data), "ens3f0") {
		t.Fatalf("恢复出厂后接口应清空: %d %s", status, data)
	}

	// 4) 恢复
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("file", "backup.json")
	_, _ = part.Write(archive)
	_ = mw.Close()
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+APIPrefix+"/system/restore", &body)
	req2.Header.Set("Content-Type", mw.FormDataContentType())
	req2.Header.Set("Authorization", "Bearer "+token)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	out := new(bytes.Buffer)
	_, _ = out.ReadFrom(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK || !strings.Contains(out.String(), "revision") {
		t.Fatalf("恢复: %d %s", resp2.StatusCode, out.String())
	}
	if status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/interfaces", token, nil, nil); status != http.StatusOK ||
		!strings.Contains(string(data), "ens3f0") {
		t.Fatalf("恢复后接口应回来: %d %s", status, data)
	}
}

// M5-6：CLI 侧 backup / restore / zeroize（双重确认）。
func TestCLISystemBackupRestoreZeroize(t *testing.T) {
	x, eng := newCLIKit(t)
	mgr := system.NewManager(system.Config{Dir: t.TempDir()}, eng, nil, "test")
	x.setSystemOps(mgr)

	run(t, x, "admin", "super-user", "ssh",
		"configure",
		"set interfaces ens1f0 description uplink",
		"commit",
		"exit")

	out := run(t, x, "admin", "super-user", "ssh", "request system configuration backup")
	if !strings.Contains(out, "备份已生成") {
		t.Fatalf("备份输出: %s", out)
	}
	files := mgr.List()
	if len(files) != 1 {
		t.Fatalf("应有 1 个归档: %+v", files)
	}
	path, err := mgr.Path(files[0].File)
	if err != nil {
		t.Fatal(err)
	}

	// 双重确认：首次问询、二次问询、两次 --yes 后执行
	r1 := x.Execute("admin", "super-user", "ssh", "request system zeroize")
	if !strings.HasSuffix(strings.TrimSpace(r1.Output), "[yes,no]") {
		t.Fatalf("首次应问询: %q", r1.Output)
	}
	r2 := x.Execute("admin", "super-user", "ssh", "request system zeroize --yes")
	if !strings.HasSuffix(strings.TrimSpace(r2.Output), "[yes,no]") {
		t.Fatalf("二次仍应问询: %q", r2.Output)
	}
	r3 := x.Execute("admin", "super-user", "ssh", "request system zeroize --yes --yes")
	if !strings.Contains(r3.Output, "已恢复出厂") {
		t.Fatalf("两次确认后应执行: %q", r3.Output)
	}
	// 恢复出厂后配置为空 → 恢复归档
	run(t, x, "admin", "super-user", "ssh", "request system configuration restore "+path)
	cfg, err := eng.Committed()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, i := range cfg.Interfaces {
		if i.Name == "ens1f0" && i.Description == "uplink" {
			found = true
		}
	}
	if !found {
		t.Fatalf("恢复后配置应含 ens1f0: %+v", cfg.Interfaces)
	}
}

// ---------- M5-8：TLS 证书与日志保留配置 ----------

type testTLS struct{ m *system.TLSManager }

func (t *testTLS) Info() (system.TlsInfo, bool)                { return t.m.Info() }
func (t *testTLS) Install(c, k string) (system.TlsInfo, error) { return t.m.Install(c, k) }
func (t *testTLS) Regenerate(h string, ips []string) (system.TlsInfo, error) {
	return t.m.Regenerate(h, ips)
}
func (t *testTLS) RegenerateSSHHostKeys(ctx context.Context) error {
	return nil
}

func TestTLSEndpoints(t *testing.T) {
	mgr := system.NewTLSManager(t.TempDir(), nil)
	ts := newTestServerOpts(t, Options{TLS: &testTLS{m: mgr}})
	token := loginAdmin(t, ts)

	// 未配置 → configured:false
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/tls", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), `"configured":false`) {
		t.Fatalf("未配置证书: %d %s", status, data)
	}
	// 重签 → 指纹返回
	status, _, data = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/tls:regenerate", token, nil,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK || !strings.Contains(string(data), "fingerprint") {
		t.Fatalf("重签: %d %s", status, data)
	}
	first, _ := mgr.Info()
	// 再签 → 指纹变化
	cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/tls:regenerate", token, nil,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	second, _ := mgr.Info()
	if first.Fingerprint == second.Fingerprint {
		t.Fatal("重签后指纹应变化")
	}
	// 安装外部证书（用另一对生成物）
	other := system.NewTLSManager(t.TempDir(), nil)
	if _, err := other.Regenerate("ext", nil); err != nil {
		t.Fatal(err)
	}
	certPEM, _ := os.ReadFile(other.CertPath())
	keyPEM, _ := os.ReadFile(other.KeyPath())
	status, _, data = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/system/tls", token,
		map[string]any{"certificate": string(certPEM), "key": string(keyPEM)},
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("安装证书: %d %s", status, data)
	}
	// 不匹配的私钥 → 400
	if status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/system/tls", token,
		map[string]any{"certificate": string(certPEM), "key": string(keyPEM[:len(keyPEM)/2])},
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusBadRequest {
		t.Fatalf("截断私钥应 400，实际 %d", status)
	}
}

// CLI：request system api tls regenerate / ssh host-key regenerate；syslog 与 tls 语句落模型。
func TestCLITLSAndSyslog(t *testing.T) {
	mgr := system.NewTLSManager(t.TempDir(), nil)
	x, eng := newCLIKit(t)
	x.setTLS(&testTLS{m: mgr})

	out := run(t, x, "admin", "super-user", "ssh", "request system api tls regenerate")
	if !strings.Contains(out, "自签证书已重签") {
		t.Fatalf("tls regenerate: %s", out)
	}
	out = run(t, x, "admin", "super-user", "ssh", "request system ssh host-key regenerate")
	if !strings.Contains(out, "SSH host key 已重新生成") {
		t.Fatalf("ssh host-key: %s", out)
	}

	run(t, x, "admin", "super-user", "ssh",
		"configure",
		"set system syslog local retention-days 30",
		"set system syslog local max-size-mb 512",
		"set system api tls self-signed regenerate",
		"commit",
		"exit")
	cfg, err := eng.Committed()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.System == nil || cfg.System.Syslog == nil || cfg.System.Syslog.RetentionDays != 30 ||
		cfg.System.Syslog.MaxSizeMB != 512 {
		t.Fatalf("syslog 保留策略未落模型: %+v", cfg.System)
	}
	if cfg.System.API == nil || !cfg.System.API.TLSSelfSigned {
		t.Fatalf("tls self-signed 未落模型: %+v", cfg.System.API)
	}
}
