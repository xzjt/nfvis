package system

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 未装 libvirt（没有 profile）时不得动系统，也不得凭空创建目录。
func TestAppArmorEnsureSkipsWithoutProfile(t *testing.T) {
	dir := t.TempDir()
	aa := &AppArmorLibvirt{ProfilePath: filepath.Join(dir, "no-such-profile"), LocalPath: filepath.Join(dir, "local")}
	changed, err := aa.Ensure(context.Background())
	if err != nil {
		t.Fatalf("无 profile 应静默跳过，得到错误: %v", err)
	}
	if changed {
		t.Fatal("无 profile 不应报告改动")
	}
	if _, err := os.Stat(aa.LocalPath); !os.IsNotExist(err) {
		t.Fatalf("无 profile 不应创建本地片段: %v", err)
	}
}

// 有 profile 且未放行：追加规则 + 重载 profile；第二次调用必须是空操作（幂等）。
func TestAppArmorEnsureAppendsAndReloadsOnce(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, "usr.lib.libvirt.virt-aa-helper")
	if err := os.WriteFile(profile, []byte("profile virt-aa-helper {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	aa := &AppArmorLibvirt{
		ProfilePath: profile,
		LocalPath:   filepath.Join(dir, "local", "virt-aa-helper"),
		Runner: func(_ context.Context, name string, args ...string) (string, error) {
			calls = append(calls, append([]string{name}, args...))
			return "", nil
		},
	}
	changed, err := aa.Ensure(context.Background())
	if err != nil {
		t.Fatalf("首次 Ensure: %v", err)
	}
	if !changed {
		t.Fatal("首次 Ensure 应报告改动")
	}
	b, err := os.ReadFile(aa.LocalPath)
	if err != nil {
		t.Fatalf("本地片段应已创建: %v", err)
	}
	for _, want := range []string{"/var/lib/nfvis/images/** r,", "/var/lib/nfvis/vms/** rk,"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("本地片段缺少规则 %q，实际:\n%s", want, b)
		}
	}
	if len(calls) != 1 || calls[0][0] != "apparmor_parser" || calls[0][1] != "-r" || calls[0][2] != profile {
		t.Fatalf("应重载一次 profile，实际调用: %v", calls)
	}
	first := string(b)

	changed, err = aa.Ensure(context.Background())
	if err != nil {
		t.Fatalf("二次 Ensure: %v", err)
	}
	if changed {
		t.Fatal("二次 Ensure 应为空操作（已放行）")
	}
	if len(calls) != 1 {
		t.Fatalf("二次 Ensure 不应再重载 profile，实际调用: %v", calls)
	}
	b2, _ := os.ReadFile(aa.LocalPath)
	if string(b2) != first {
		t.Fatalf("二次 Ensure 改变了文件内容:\n首次: %q\n二次: %q", first, b2)
	}
}

// 本地片段已有别的内容时必须**追加**（发行版约定该文件可能被别人写过），不能覆盖。
func TestAppArmorEnsureKeepsExistingContent(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, "profile")
	local := filepath.Join(dir, "local")
	if err := os.WriteFile(profile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	pre := "# 别的组件写的\n  /srv/other/** r,\n"
	if err := os.WriteFile(local, []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}
	aa := &AppArmorLibvirt{ProfilePath: profile, LocalPath: local}
	if _, err := aa.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	b, _ := os.ReadFile(local)
	if !strings.HasPrefix(string(b), pre) {
		t.Fatalf("原有内容被破坏:\n%s", b)
	}
	if !strings.Contains(string(b), aaMarker) {
		t.Fatalf("追加后应含放行规则:\n%s", b)
	}
}

// 重载失败（apparmor_parser 报错）时不能当成功：要有 changed 与错误，供调用方告警。
func TestAppArmorEnsureReportsReloadFailure(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, "profile")
	if err := os.WriteFile(profile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	aa := &AppArmorLibvirt{
		ProfilePath: profile,
		LocalPath:   filepath.Join(dir, "local"),
		Runner: func(context.Context, string, ...string) (string, error) {
			return "apparmor_parser: 解析失败", errors.New("exit 1")
		},
	}
	changed, err := aa.Ensure(context.Background())
	if err == nil {
		t.Fatal("重载失败必须返回错误（放行没生效）")
	}
	if !changed {
		t.Fatal("文件已写，应报告 changed=true")
	}
}
