package system

// R37-2 收口（决策 #118）：组件版本探测器的单测。
// 手法与仓库一致——runner 与 os-release 路径都可注入（桩），不打真命令、不依赖真机底座。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeOSRelease(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "os-release")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写桩 os-release: %v", err)
	}
	return path
}

func fakeProbe(t *testing.T, osRelease string, run func(name string, args ...string) (string, error)) *VersionProbe {
	t.Helper()
	return &VersionProbe{
		run: func(_ context.Context, name string, args ...string) (string, error) {
			return run(name, args...)
		},
		osReleasePath: osRelease,
	}
}

// 全底座都在：四个键都探到，且**不含** dpdk/vpp/nfvis（探测范围外/无来源）。
func TestVersionProbeAllComponents(t *testing.T) {
	p := fakeProbe(t, writeOSRelease(t, "PRETTY_NAME=\"Ubuntu 26.04.1 LTS\"\nVERSION_ID=\"26.04\"\n"),
		func(name string, _ ...string) (string, error) {
			switch name {
			case "libvirtd":
				return "libvirtd (libvirt) 12.0.0\n", nil
			case "qemu-system-x86_64":
				return "QEMU emulator version 10.2.1 (Debian 1:10.2.1+ds-1ubuntu3.2)\n", nil
			case "docker":
				return "29.1.3\n", nil
			}
			return "", errors.New("unexpected command: " + name)
		})
	got := p.Components(context.Background())
	want := map[string]string{"ubuntu": "26.04", "libvirt": "12.0.0", "qemu": "10.2.1", "docker": "29.1.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Components() = %v，期望 %v", got, want)
	}
}

// 底座不在/命令失败/输出解析不到：对应键**不给**（不编造空串），其它键照常。
func TestVersionProbeMissingComponentsYieldNoKeys(t *testing.T) {
	p := fakeProbe(t, writeOSRelease(t, "VERSION_ID=\"26.04\"\n"),
		func(name string, _ ...string) (string, error) {
			switch name {
			case "libvirtd":
				return "", errors.New("exec: libvirtd: executable file not found in $PATH")
			case "qemu-system-x86_64":
				return "unexpected output without version\n", nil // 解析不到 → 不给
			case "docker":
				return "\n", nil // 空输出 → 不给
			}
			return "", nil
		})
	got := p.Components(context.Background())
	want := map[string]string{"ubuntu": "26.04"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Components() = %v，期望 %v", got, want)
	}
}

// 非 Linux（无 /etc/os-release）且底座都不在：空 map（调用方据此只回 NFViS 版本）。
func TestVersionProbeNothingAvailable(t *testing.T) {
	p := fakeProbe(t, filepath.Join(t.TempDir(), "nonexistent"),
		func(string, ...string) (string, error) { return "", errors.New("not found") })
	if got := p.Components(context.Background()); len(got) != 0 {
		t.Fatalf("Components() = %v，期望空", got)
	}
}

// VERSION_ID 缺失时退回 PRETTY_NAME（os-release 的常规回退路径）。
func TestUbuntuVersionFallsBackToPrettyName(t *testing.T) {
	if got := ubuntuVersion(writeOSRelease(t, "PRETTY_NAME=\"Ubuntu 26.04.1 LTS\"\n")); got != "Ubuntu 26.04.1 LTS" {
		t.Fatalf("ubuntuVersion = %q", got)
	}
	// 文件不存在（非 Linux）→ 空串，不给该键。
	if got := ubuntuVersion(filepath.Join(t.TempDir(), "nope")); got != "" {
		t.Fatalf("文件不存在时应为空串，得到 %q", got)
	}
}

func TestParseLibvirtVersion(t *testing.T) {
	if got := parseLibvirtVersion("libvirtd (libvirt) 12.0.0\n"); got != "12.0.0" {
		t.Fatalf("parseLibvirtVersion = %q", got)
	}
}

func TestParseQEMUVersion(t *testing.T) {
	if got := parseQEMUVersion("QEMU emulator version 10.2.1 (Debian 1:10.2.1+ds-1ubuntu3.2)"); got != "10.2.1" {
		t.Fatalf("parseQEMUVersion = %q", got)
	}
	if got := parseQEMUVersion("no version here"); got != "" {
		t.Fatalf("无 version 字样应为空串，得到 %q", got)
	}
}
