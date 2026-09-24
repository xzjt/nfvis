package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/system"
)

type testDiagOps struct {
	tech  *system.TechSupport
	cores *system.CoreDumps
}

func (d *testDiagOps) GenerateTechSupport() (system.File, error) { return d.tech.Generate() }
func (d *testDiagOps) ListTechSupport() []system.File            { return d.tech.List() }
func (d *testDiagOps) TechSupportPath(n string) (string, error)  { return d.tech.Path(n) }
func (d *testDiagOps) ListCoreDumps() []system.CoreDump          { return d.cores.List() }
func (d *testDiagOps) DeleteCoreDumps(f string) (int, error)     { return d.cores.Delete(f) }
func (d *testDiagOps) ExportCoreDumps(ctx context.Context, url string) (int, int, error) {
	return d.cores.ExportManifest(ctx, url)
}

func newTestDiagOps(t *testing.T) (*testDiagOps, string) {
	t.Helper()
	coreDir := t.TempDir()
	cores := system.NewCoreDumps(coreDir, 0)
	tech := system.NewTechSupport(t.TempDir(), system.TechSupportSources{
		Version: func() any { return map[string]string{"nfvis": "test"} },
		Config:  func() (any, error) { return map[string]string{"hostname": "n1"}, nil },
		Audit:   func() (any, error) { return []string{"login"}, nil },
		Status:  func() (any, error) { return map[string]bool{"vpp_connected": true}, nil },
		Logs:    func() ([]byte, error) { return []byte("log tail\n"), nil },
		Cores:   cores.List,
	}, "test")
	return &testDiagOps{tech: tech, cores: cores}, coreDir
}

// M5-4：tech-support 生成/列表/下载 + core-dump 列表/删除。
func TestDiagOpsEndpoints(t *testing.T) {
	d, coreDir := newTestDiagOps(t)
	if err := os.WriteFile(filepath.Join(coreDir, "core.nfvisd.42.1700000000"), []byte("core-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := newTestServerOpts(t, Options{DiagOps: d})
	token := loginAdmin(t, ts)

	// core dump 列表
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/core-dumps", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "core.nfvisd.42.1700000000") ||
		!strings.Contains(string(data), `"process":"nfvisd"`) {
		t.Fatalf("core-dumps 列表: %d %s", status, data)
	}

	// 生成诊断归档
	status, _, data = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/tech-support", token, nil,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusAccepted {
		t.Fatalf("生成归档: %d %s", status, data)
	}
	if !strings.Contains(string(data), "nfvis-tech-support-") {
		t.Fatalf("归档元数据: %s", data)
	}
	// 列表 + 下载（gzip 魔数）
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/tech-support", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "nfvis-tech-support-") {
		t.Fatalf("归档列表: %d %s", status, data)
	}
	files := d.tech.List()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+APIPrefix+"/system/tech-support/"+files[0].File, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.HasPrefix(buf.Bytes(), []byte{0x1f, 0x8b}) {
		t.Fatalf("归档下载应为 gzip: %d %v", resp.StatusCode, buf.Bytes()[:2])
	}
	// 路径穿越拒绝
	req2, _ := http.NewRequest(http.MethodGet, ts.URL+APIPrefix+"/system/tech-support/..%2Fsecret", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	if r2, err := http.DefaultClient.Do(req2); err == nil {
		r2.Body.Close()
		if r2.StatusCode == http.StatusOK {
			t.Fatal("穿越路径不应 200")
		}
	}

	// 删除单个 core dump
	if status, _, _ := cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/system/core-dumps?file=core.nfvisd.42.1700000000", token, nil,
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusNoContent {
		t.Fatalf("删除转储应 204，实际 %d", status)
	}
	if len(d.cores.List()) != 0 {
		t.Fatal("删除后应为空")
	}
}

// 决策 #149：诊断归档下载**保持 read-only 可下载**（现场流程：operator 生成诊断包 →
// 自己下载送支持），代价是归档里的配置必须是**脱敏视图**——本用例同时锁定这两条：
// ① read-only 拿到 200（class 未收紧）；② 归档整包里没有口令哈希（含 config.json）。
// 变异验证：把 config 分节改回不脱敏的 sectionJSON → ② 立刻报「归档泄露口令哈希」。
func TestTechSupportDownloadReadOnlyButRedacted(t *testing.T) {
	const sentinel = "pbkdf2$sha256$600000$APITSALT$APITSHASH"
	coreDir := t.TempDir()
	cores := system.NewCoreDumps(coreDir, 0)
	tech := system.NewTechSupport(t.TempDir(), system.TechSupportSources{
		Version: func() any { return map[string]string{"nfvis": "test"} },
		// 真机形态：config 来源就是 engine.Committed()，必然含本地用户的口令哈希
		Config: func() (any, error) {
			return model.Config{System: &model.SystemConfig{
				Hostname: "ts-node",
				Login: &model.SystemLogin{Users: []model.LoginUserConfig{
					{Name: "admin", Class: "super-user", PasswordHash: sentinel},
				}},
			}}, nil
		},
		Audit:  func() (any, error) { return []string{"config.commit"}, nil },
		Status: func() (any, error) { return map[string]bool{"vpp_connected": true}, nil },
		Logs:   func() ([]byte, error) { return []byte("nfvisd 就绪\n"), nil },
		Cores:  cores.List,
	}, "test")
	ts := newTestServerOpts(t, Options{DiagOps: &testDiagOps{tech: tech, cores: cores}})
	admin := loginAdmin(t, ts)

	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/tech-support", admin, nil,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusAccepted {
		t.Fatalf("生成诊断归档: %d %s", status, data)
	}
	files := tech.List()
	if len(files) != 1 {
		t.Fatalf("应有 1 份归档: %+v", files)
	}
	dl := ts.URL + APIPrefix + "/system/tech-support/" + files[0].File

	// ① read-only（viewer）下载 200 —— 收紧 class 会破掉现场流程，故这里必须可下
	_, viewer := login(t, ts, "viewer", "s3cret-Passw0rd!")
	req, _ := http.NewRequest(http.MethodGet, dl, nil)
	req.Header.Set("Authorization", "Bearer "+viewer.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw := new(bytes.Buffer)
	_, _ = raw.ReadFrom(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read-only 下载诊断归档应 200（现场流程不破），实际 %d %s", resp.StatusCode, raw.String())
	}

	// ② 整包（逐成员）不得含口令哈希；哨兵 + 真哈希形态两条判据互补
	body := gunzipAll(t, raw.Bytes())
	if bytes.Contains(body, []byte(sentinel)) || realHashRe.Match(body) {
		t.Fatalf("诊断归档泄露口令哈希:\n%s", body)
	}
	if bytes.Contains(body, []byte("password_hash")) {
		t.Fatalf("诊断归档回了 password_hash 键:\n%s", body)
	}
	// ③ 脱敏 ≠ 掏空 + 确实打到了归档（避免断言落在空包/错误体上）
	for _, want := range []string{"config.json", "ts-node", "admin", "logs.txt"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("归档应含 %q（否则上面的断言等于没跑）:\n%s", want, body)
		}
	}
}

// gunzipAll 解开 tar.gz 归档的**全部字节**（含成员名与正文），供「整包不含秘密」断言用。
func gunzipAll(t *testing.T, b []byte) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("归档应为 gzip: %v", err)
	}
	defer gz.Close()
	out, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("解压归档: %v", err)
	}
	return out
}

// M5-4：CLI `request system tech-support generate` / `show system tech-support|core-dumps`。
func TestCLIDiagOps(t *testing.T) {
	d, coreDir := newTestDiagOps(t)
	_ = os.WriteFile(filepath.Join(coreDir, "core.vpp_main.7.1700000000"), []byte("x"), 0o644)
	x, _ := newCLIKit(t)
	x.setDiagOps(d)

	out := run(t, x, "admin", "super-user", "ssh", "request system tech-support generate")
	if !strings.Contains(out, "诊断归档已生成") {
		t.Fatalf("生成输出: %s", out)
	}
	out = run(t, x, "admin", "super-user", "ssh", "show system tech-support")
	if !strings.Contains(out, "nfvis-tech-support-") {
		t.Fatalf("tech-support 列表: %s", out)
	}
	out = run(t, x, "admin", "super-user", "ssh", "show system core-dumps")
	if !strings.Contains(out, "vpp_main") {
		t.Fatalf("core-dumps 列表: %s", out)
	}
	out = run(t, x, "admin", "super-user", "ssh", "request system core-dumps delete")
	if !strings.Contains(out, "已删除 1 个") {
		t.Fatalf("删除输出: %s", out)
	}
}

// ---------- M5-5：硬件健康与阈值 ----------

type fakeHardware struct{ hh system.HardwareHealth }

func (f *fakeHardware) Collect(_ context.Context) system.HardwareHealth { return f.hh }
func (f *fakeHardware) Evaluate(hh *system.HardwareHealth, cpuTemp, diskTemp, diskUsed int) []string {
	var v []string
	if diskUsed > 0 && hh.RootUsedPercent >= float64(diskUsed) {
		v = append(v, "root used")
	}
	return v
}

func TestHardwareEndpointsAndThresholds(t *testing.T) {
	fh := &fakeHardware{hh: system.HardwareHealth{
		BMCPresent:      false,
		Sensors:         []system.HealthSensor{{Name: "x86_pkg_temp", Type: "temperature", Value: 47, Unit: "C", Status: "ok"}},
		Disks:           []system.HealthDisk{{Device: "/dev/sda", SmartStatus: "unknown"}},
		RootUsedPercent: 42,
	}}
	ts := newTestServerOpts(t, Options{Hardware: fh})
	token := loginAdmin(t, ts)

	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/hardware", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "x86_pkg_temp") ||
		!strings.Contains(string(data), `"/dev/sda"`) {
		t.Fatalf("GET hardware: %d %s", status, data)
	}

	// 阈值：PUT（auto-commit）→ GET 回读
	if status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/system/health/thresholds", token,
		map[string]any{"cpu_temp_celsius": 85, "disk_temp_celsius": 60, "disk_used_percent": 30},
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("PUT thresholds: %d %s", status, data)
	}
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/health/thresholds", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), `"disk_used_percent":30`) {
		t.Fatalf("GET thresholds: %d %s", status, data)
	}
	// 越限（root 42% ≥ 30%）应出现在 violations
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/hardware", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "root used") {
		t.Fatalf("越限应上报: %d %s", status, data)
	}
	// 范围校验（>100 拒绝）
	if status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/system/health/thresholds", token,
		map[string]any{"disk_used_percent": 200}, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status == http.StatusOK {
		t.Fatal("越界阈值应被校验拒绝")
	}
}

// CLI：show system hardware + set system health thresholds 落入模型。
func TestCLIHardwareAndThresholds(t *testing.T) {
	fh := &fakeHardware{hh: system.HardwareHealth{BMCPresent: false, RootUsedPercent: 10}}
	x, eng := newCLIKit(t)
	x.setHardware(fh)

	run(t, x, "admin", "super-user", "ssh",
		"configure",
		"set system health thresholds cpu-temp-celsius 85",
		"set system health thresholds disk-used-percent 90",
		"commit",
		"exit")
	cfg, err := eng.Committed()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.System == nil || cfg.System.Health == nil || cfg.System.Health.CPUTempCelsius != 85 ||
		cfg.System.Health.DiskUsedPercent != 90 {
		t.Fatalf("阈值未落模型: %+v", cfg.System)
	}
	out := run(t, x, "admin", "super-user", "ssh", "show system hardware")
	if !strings.Contains(out, "bmc-present    false") || !strings.Contains(out, "root-used") {
		t.Fatalf("show system hardware: %s", out)
	}
}
