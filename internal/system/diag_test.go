package system

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
