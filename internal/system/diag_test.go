package system

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
)

func TestCoreDumpsListDeletePrune(t *testing.T) {
	dir := t.TempDir()
	c := NewCoreDumps(dir, 100) // 100 字节上限便于触发滚动
	write := func(name string, size int, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		_ = os.Chtimes(p, mt, mt)
	}
	write("core.vpp_main.1234.1700000000", 40, 3*time.Hour)
	write("core.qemu-system-x86_64.99.1700000100", 40, 2*time.Hour)
	write("core.nfvisd.7.1700000200", 40, time.Hour)
	write("notes.txt", 10, time.Minute) // 非转储，应忽略

	rows := c.List()
	if len(rows) != 3 {
		t.Fatalf("应识别 3 个转储: %+v", rows)
	}
	if rows[0].Process != "nfvisd" {
		t.Fatalf("最新转储应为 nfvisd: %+v", rows[0])
	}
	if rows[2].Process != "vpp_main" {
		t.Fatalf("进程名推断: %+v", rows[2])
	}

	// 容量 100 < 120 → 删除最旧的 1 个
	if n := c.Prune(); n != 1 {
		t.Fatalf("应清理 1 个: %d", n)
	}
	if len(c.List()) != 2 {
		t.Fatalf("清理后应剩 2 个: %+v", c.List())
	}
	// 删除单个
	if n, err := c.Delete("core.nfvisd.7.1700000200"); err != nil || n != 1 {
		t.Fatalf("删除单个: %d %v", n, err)
	}
	// 路径穿越拒绝
	if _, err := c.Path("../etc/passwd"); err == nil {
		t.Fatal("穿越路径应拒绝")
	}
	// 全部删除
	if n, err := c.Delete(""); err != nil || n != 1 {
		t.Fatalf("全部删除: %d %v", n, err)
	}
	if len(c.List()) != 0 {
		t.Fatal("应清空")
	}
}

func TestTechSupportArchiveSections(t *testing.T) {
	dir := t.TempDir()
	coreDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(coreDir, "core.vpp_main.1.1"), []byte("x"), 0o644)
	cores := NewCoreDumps(coreDir, 0)

	ts := NewTechSupport(dir, TechSupportSources{
		Version: func() any { return map[string]string{"nfvis": "1.0.0", "vpp": "26.06"} },
		Config:  func() (any, error) { return map[string]any{"hostname": "node1"}, nil },
		Audit:   func() (any, error) { return []map[string]string{{"action": "login"}}, nil },
		Status:  func() (any, error) { return map[string]any{"vpp_connected": true}, nil },
		Logs:    func() ([]byte, error) { return []byte("log line 1\nlog line 2\n"), nil },
		Cores:   cores.List,
	}, "1.0.0")

	f, err := ts.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if f.Kind != "tech-support" || f.SizeBytes <= 0 {
		t.Fatalf("元数据: %+v", f)
	}
	path, err := ts.Path(f.File)
	if err != nil {
		t.Fatal(err)
	}
	got := readTarGz(t, path)
	for _, want := range []string{"version.json", "config.json", "audit.json", "status.json", "logs.txt", "core-dumps.json", "README.txt"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("归档缺少分节 %s（实际 %v）", want, keys(got))
		}
	}
	if !strings.Contains(got["logs.txt"], "log line 2") {
		t.Fatalf("logs 分节内容: %q", got["logs.txt"])
	}
	var coreList []CoreDump
	if err := json.Unmarshal([]byte(got["core-dumps.json"]), &coreList); err != nil || len(coreList) != 1 {
		t.Fatalf("core-dumps 分节: %v %v", err, got["core-dumps.json"])
	}
	if !strings.Contains(got["version.json"], "26.06") {
		t.Fatalf("version 分节: %q", got["version.json"])
	}
	if list := ts.List(); len(list) != 1 || list[0].File != f.File {
		t.Fatalf("列表: %+v", list)
	}
}

// 单节来源报错不得中断归档（诊断包必须尽量产出）。
func TestTechSupportSectionErrorTolerated(t *testing.T) {
	ts := NewTechSupport(t.TempDir(), TechSupportSources{
		Config: func() (any, error) { return nil, os.ErrPermission },
	}, "1.0.0")
	f, err := ts.Generate()
	if err != nil {
		t.Fatalf("Generate 不应失败: %v", err)
	}
	path, _ := ts.Path(f.File)
	got := readTarGz(t, path)
	if !strings.Contains(got["config.json"], "section error") {
		t.Fatalf("应写入错误说明: %q", got["config.json"])
	}
}

func readTarGz(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out[hdr.Name] = string(b)
	}
	return out
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestNotFoundMessagesAreContextSpecific 覆盖决策 #76 §4④：
// core dump / 诊断归档 / 备份归档 三者各自的「不存在」报错必须能被区分——
// 此前 coredump.go 与 techsupport.go 复用了 backup.go 的 ErrNotFound，
// 导致 core dump 找不到时报「备份归档不存在」，误导排查（全功能 CLI 测试发现）。
func TestNotFoundMessagesAreContextSpecific(t *testing.T) {
	cores := NewCoreDumps(t.TempDir(), 0)
	if _, err := cores.Path("nope"); err == nil || !strings.Contains(err.Error(), "core dump") {
		t.Fatalf("core dump 不存在应报 core dump 相关文案，实际: %v", err)
	} else if strings.Contains(err.Error(), "备份归档") {
		t.Fatalf("core dump 报错不得说成备份归档: %v", err)
	}

	ts := NewTechSupport(t.TempDir(), TechSupportSources{}, "1.0.0")
	if _, err := ts.Path("nope"); err == nil || !strings.Contains(err.Error(), "诊断归档") {
		t.Fatalf("诊断归档不存在应报诊断相关文案，实际: %v", err)
	} else if strings.Contains(err.Error(), "备份归档") {
		t.Fatalf("诊断归档报错不得说成备份归档: %v", err)
	}

	m := NewManager(Config{Dir: t.TempDir()}, nil, nil, "1.0.0")
	if _, err := m.Path("nope"); err == nil || !strings.Contains(err.Error(), "备份归档") {
		t.Fatalf("备份归档不存在应报备份相关文案，实际: %v", err)
	}
}

// ---------- 决策 #149：诊断归档不含秘密（配置脱敏 + 日志剥凭据 + 0600 落盘） ----------

// tsSentinelHash / tsBootstrapPassword 哨兵值：值由本测试给定，故可精确断言「这一串没有出现」
// （裸 `pbkdf2$` 前缀判据不可靠——契约与规范文本里也有这个前缀，会误报）。
const (
	tsSentinelHash      = "pbkdf2$sha256$600000$TSSALT$TSHASH"
	tsBootstrapPassword = "Boot-OneTime@2026"
)

// newSecretsTechSupport 造一个「配置里带口令哈希、日志里带一次性口令」的诊断包来源——
// 这正是真机上的形态：config 来源是 engine.Committed()，logs 来源是 journalctl（首启引导
// 的一次性口令就打在 stdout、被 journal 收走）。
func newSecretsTechSupport(t *testing.T, dir string) *TechSupport {
	t.Helper()
	logs := "systemd[1]: Started nfvisd.service.\n" +
		"%% 首次启动已创建用户 admin (super-user)。" + bootstrapCredentialMarker + ": " + tsBootstrapPassword + "\n" +
		"nfvisd 就绪\n"
	return NewTechSupport(dir, TechSupportSources{
		Version: func() any { return map[string]string{"nfvis": "1.0.0"} },
		Config: func() (any, error) {
			return model.Config{System: &model.SystemConfig{
				Hostname: "ts-node",
				Login: &model.SystemLogin{Users: []model.LoginUserConfig{
					{Name: "admin", Class: "super-user", PasswordHash: tsSentinelHash},
				}},
			}}, nil
		},
		Audit:  func() (any, error) { return []map[string]string{{"action": "config.commit"}}, nil },
		Status: func() (any, error) { return map[string]bool{"vpp_connected": true}, nil },
		Logs:   func() ([]byte, error) { return []byte(logs), nil },
	}, "1.0.0")
}

// 归档**整包**不得含秘密（逐成员断言，避免只看 config.json 而漏掉别的分节）；
// 同时断言「脱敏 ≠ 掏空」——非敏感内容必须还在，否则"没泄露"只是因为什么都没回。
func TestTechSupportArchiveHasNoSecrets(t *testing.T) {
	ts := newSecretsTechSupport(t, t.TempDir())
	f, err := ts.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	path, err := ts.Path(f.File)
	if err != nil {
		t.Fatal(err)
	}
	got := readTarGz(t, path)

	for name, body := range got {
		for _, leak := range []string{tsSentinelHash, "password_hash", "pbkdf2$", tsBootstrapPassword} {
			if strings.Contains(body, leak) {
				t.Fatalf("分节 %s 含秘密 %q:\n%s", name, leak, body)
			}
		}
	}

	// 配置分节是**脱敏视图**：敏感叶子整个移除，其余照旧
	cfg := got["config.json"]
	if !strings.Contains(cfg, "ts-node") || !strings.Contains(cfg, "admin") {
		t.Fatalf("配置分节应保留非敏感内容（否则「没泄露」只是因为没回）: %s", cfg)
	}
	// 日志分节除凭据那一行外是原文：标记行仍在（引导发生过是诊断事实），值换成占位
	logs := got["logs.txt"]
	if !strings.Contains(logs, "nfvisd 就绪") || !strings.Contains(logs, "Started nfvisd.service") {
		t.Fatalf("日志分节应保留其余行: %q", logs)
	}
	if !strings.Contains(logs, bootstrapCredentialMarker) || !strings.Contains(logs, model.RedactedPlaceholder) {
		t.Fatalf("带凭据的那一行应保留标记、值换占位: %q", logs)
	}
	// README 向读包的人说明口径（支持人员看到「用户没有 password_hash」时不该以为是丢字段）
	if !strings.Contains(got["README.txt"], "脱敏视图") {
		t.Fatalf("README 应写明配置分节是脱敏视图: %q", got["README.txt"])
	}
	// 其余分节照旧
	if !strings.Contains(got["audit.json"], "config.commit") || !strings.Contains(got["status.json"], "vpp_connected") {
		t.Fatalf("audit/status 分节内容应不变: %v", keys(got))
	}
}

// 日志脱敏只动带凭据的那一行：普通行（含与标记无关的口令字样）一字不改。
func TestTechSupportLogsScrubOnlyCredentialLine(t *testing.T) {
	body := []byte("line1: 用户口令策略已更新\n" +
		"line2: " + bootstrapCredentialMarker + ": " + tsBootstrapPassword + "（仅这一次）\n" +
		"line3: 口令已修改\n")
	out := string(scrubBootstrapCredential(body))
	if strings.Contains(out, tsBootstrapPassword) {
		t.Fatalf("凭据值应被剥掉: %q", out)
	}
	for _, want := range []string{"line1: 用户口令策略已更新", "line3: 口令已修改", bootstrapCredentialMarker, model.RedactedPlaceholder} {
		if !strings.Contains(out, want) {
			t.Fatalf("应保留 %q: %q", want, out)
		}
	}
	// 无标记的日志原样返回（不做猜测式改写）
	plain := []byte("no marker here\n")
	if got := scrubBootstrapCredential(plain); string(got) != string(plain) {
		t.Fatalf("无标记应原样返回: %q", got)
	}
}

// 归档落盘 0600、目录 0700；既有安装留下的 0755 目录也要被收紧。
func TestTechSupportArchivePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows 不实现 POSIX 权限位（Go 一律报 0666/0777），本断言只在类 Unix 上有意义
		// ——沿用决策 #77 既有用例的口径。跨平台的那一层见 TestTechSupportArchiveModeIsExplicit0600。
		t.Skip("跳过：Windows 无 POSIX 权限位")
	}
	dir := filepath.Join(t.TempDir(), "tech-support")
	if err := os.MkdirAll(dir, 0o755); err != nil { // 模拟旧版本已建好的目录
		t.Fatal(err)
	}
	ts := NewTechSupport(dir, TechSupportSources{}, "1.0.0")
	f, err := ts.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	path, _ := ts.Path(f.File)
	if fi, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("诊断归档权限应为 0600，实际 %04o（包内含配置与日志，不得对他人可读）", perm)
	}
	if di, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	} else if perm := di.Mode().Perm(); perm != 0o700 {
		t.Fatalf("诊断归档目录应为 0700，实际 %04o（MkdirAll 不改已存在目录，须显式收紧）", perm)
	}
}

// 归档落盘权限的**跨平台**守护：Windows 上 POSIX 断言必跳过，若没有这一条，
// 「把 0600 改回 os.Create（0666&~umask → 0644）」在本机就无人报错。
// 判据是**开档语句本身**，不是行为——行为那层由 TestTechSupportArchivePermissions 在类 Unix 上验。
func TestTechSupportArchiveModeIsExplicit0600(t *testing.T) {
	b, err := os.ReadFile("techsupport.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if strings.Contains(src, "os.Create(") {
		t.Fatalf("诊断归档不得用 os.Create 落盘（0666&~umask → 通常 0644）：必须显式 0600")
	}
	openLine := ""
	for _, ln := range strings.Split(src, "\n") {
		if strings.Contains(ln, "os.OpenFile(path") {
			openLine = strings.TrimSpace(ln)
		}
	}
	if openLine == "" {
		t.Fatalf("找不到归档开档语句（os.OpenFile(path …)）——守护需同步")
	}
	if !strings.Contains(openLine, "0o600") {
		t.Fatalf("归档开档语句应显式 0600，实际: %s", openLine)
	}
	for _, want := range []struct{ frag, why string }{
		{"os.MkdirAll(t.Dir", "归档目录创建点"},
		{"os.Chmod(t.Dir", "既有目录权限收紧点（MkdirAll 不改已存在目录的权限）"},
		{"0o700", "目录 0700"},
	} {
		if !strings.Contains(src, want.frag) {
			t.Fatalf("源码里找不到%s（%q）", want.why, want.frag)
		}
	}
}

// 启动提示与日志脱敏的标识必须同源：若 main.go 改了文案而没同步 bootstrapCredentialMarker，
// 明文口令会**静默**留在诊断包里（脱敏代码还在、只是不再命中）——那正是最坏的一种假绿。
func TestBootstrapCredentialMarkerMatchesStartupMessage(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "cmd", "nfvisd", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), bootstrapCredentialMarker) {
		t.Fatalf("cmd/nfvisd/main.go 里已无 %q —— 改启动提示后请同步 internal/system/techsupport.go 的 bootstrapCredentialMarker（否则日志脱敏静默失效）",
			bootstrapCredentialMarker)
	}
}
