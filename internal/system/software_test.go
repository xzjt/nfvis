package system

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner 记录命令并按预期返回。
type fakeRunner struct {
	calls   []string
	replies map[string]string // "dpkg-deb -f <path> Version" → 值（按子串匹配）
	failOn  string
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, line)
	if f.failOn != "" && strings.Contains(line, f.failOn) {
		return "", errors.New("command failed")
	}
	// 最长匹配优先（避免 "Version" 抢先匹配到 dpkg-query 的 -f=${Version}）
	bestKey, bestVal := "", ""
	for k, v := range f.replies {
		if strings.Contains(line, k) && len(k) > len(bestKey) {
			bestKey, bestVal = k, v
		}
	}
	if bestKey != "" {
		return bestVal, nil
	}
	return "", nil
}

func newSWFixture(t *testing.T, cur string) (*SoftwareManager, *fakeRunner, string) {
	t.Helper()
	dir := t.TempDir()
	fr := &fakeRunner{replies: map[string]string{}}
	version := cur
	// dpkg-deb -f <pkg> Package|Version
	fr.replies[" Package"] = "nfvis\n"
	m := NewSoftwareManager(dir, fr.run, func() string { return version })
	// 版本随安装变化：在 run 被调用后由测试通过 helper 调整
	m.version = func() string { return version }
	_ = m
	return m, fr, dir
}

func writeDeb(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("deb"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSoftwareAddLocalDeb(t *testing.T) {
	dir := t.TempDir()
	pkgDir := t.TempDir()
	src := writeDeb(t, pkgDir, "nfvis_1.0.1_amd64.deb")
	cur := "1.0.0"
	fr := &fakeRunner{replies: map[string]string{" Package": "nfvis\n"}}
	fr.replies["Version"] = "1.0.1\n"
	fr.replies["dpkg-query"] = "1.0.0\n"
	m := NewSoftwareManager(dir, fr.run, func() string { return cur })

	res, err := m.Add(context.Background(), src, "")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if res.Previous != "1.0.0" || res.Version != "1.0.1" {
		t.Fatalf("结果版本: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "nfvis_1.0.1_amd64.deb")); err != nil {
		t.Fatalf("应归档到软件目录: %v", err)
	}
	joined := strings.Join(fr.calls, "\n")
	if !strings.Contains(joined, "dpkg -i") {
		t.Fatalf("应调用 dpkg -i: %v", fr.calls)
	}
}

func TestSoftwareAddRejectsNonNfvisPackage(t *testing.T) {
	dir := t.TempDir()
	src := writeDeb(t, t.TempDir(), "other_1.0.0_amd64.deb")
	fr := &fakeRunner{replies: map[string]string{" Package": "other\n"}}
	m := NewSoftwareManager(dir, fr.run, func() string { return "1.0.0" })
	if _, err := m.Add(context.Background(), src, ""); err == nil || !strings.Contains(err.Error(), "不是 nfvis") {
		t.Fatalf("非 nfvis 包应拒绝: %v", err)
	}
}

func TestSoftwareAddSHA256Mismatch(t *testing.T) {
	dir := t.TempDir()
	src := writeDeb(t, t.TempDir(), "nfvis_1.0.1_amd64.deb")
	fr := &fakeRunner{}
	m := NewSoftwareManager(dir, fr.run, func() string { return "1.0.0" })
	if _, err := m.Add(context.Background(), src, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "sha256 校验失败") {
		t.Fatalf("sha256 不符应拒绝: %v", err)
	}
}

func TestSoftwareRollbackPicksHighestNonCurrent(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"1.0.0", "1.0.2", "1.0.1"} {
		writeDeb(t, dir, "nfvis_"+v+"_amd64.deb")
	}
	fr := &fakeRunner{replies: map[string]string{"dpkg-query": "1.0.2\n"}}
	cur := "1.0.2"
	m := NewSoftwareManager(dir, fr.run, func() string { return cur })
	res, err := m.Rollback(context.Background())
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if !strings.Contains(res.Package, "nfvis_1.0.1_amd64.deb") {
		t.Fatalf("应回退到版本最高的非当前包: %+v", res)
	}
	if !strings.Contains(strings.Join(fr.calls, " "), "dpkg -i") {
		t.Fatalf("应调用 dpkg -i: %v", fr.calls)
	}

	// 归档仅当前版本 → 报错
	dir2 := t.TempDir()
	writeDeb(t, dir2, "nfvis_1.0.2_amd64.deb")
	fr2 := &fakeRunner{replies: map[string]string{"dpkg-query": "1.0.2\n"}}
	m2 := NewSoftwareManager(dir2, fr2.run, func() string { return "1.0.2" })
	if _, err := m2.Rollback(context.Background()); err == nil || !strings.Contains(err.Error(), "无可回退") {
		t.Fatalf("无候选应报错: %v", err)
	}
}

func TestSoftwarePowerAndNTP(t *testing.T) {
	fr := &fakeRunner{}
	m := NewSoftwareManager(t.TempDir(), fr.run, func() string { return "1.0.0" })
	if err := m.Reboot(context.Background()); err != nil {
		t.Fatalf("Reboot: %v", err)
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if _, err := m.NTPSync(context.Background(), nil); err != nil {
		t.Fatalf("NTPSync: %v", err)
	}
	joined := strings.Join(fr.calls, "\n")
	for _, want := range []string{"systemctl reboot", "systemctl poweroff", "chronyc makestep"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺少命令 %q: %v", want, fr.calls)
		}
	}
}

func TestCompareVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.1", -1},
		{"1.0.10", "1.0.9", 1},
		{"1.0.0", "1.0.0", 0},
		{"2.0.0", "1.9.9", 1},
	}
	for _, c := range cases {
		if got := compareVersion(c.a, c.b); (got < 0 && c.want >= 0) || (got > 0 && c.want <= 0) || (got == 0 && c.want != 0) {
			t.Fatalf("compareVersion(%s,%s)=%d，期望 %d", c.a, c.b, got, c.want)
		}
	}
}
