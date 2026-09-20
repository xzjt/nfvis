package compute

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

func TestLayoutPaths(t *testing.T) {
	l := NewLayout("/var/lib/nfvis/vms", "fw-vm")
	if l.Dir != "/var/lib/nfvis/vms/fw-vm" ||
		l.DiskPath != "/var/lib/nfvis/vms/fw-vm/disk.qcow2" ||
		l.SeedISO != "/var/lib/nfvis/vms/fw-vm/seed.iso" {
		t.Fatalf("布局不符: %+v", l)
	}
	if got := DataDiskPath("/var/lib/nfvis/vms", "fw-vm", "data0"); got != "/var/lib/nfvis/vms/fw-vm/data-data0.qcow2" {
		t.Fatalf("数据盘路径: %s", got)
	}
	if got := VhostSocketPath("/run/nfvis/vhost", "fw-vm", "eth0"); got != "/run/nfvis/vhost/fw-vm-eth0.sock" {
		t.Fatalf("vhost socket 路径: %s", got)
	}
}

func TestImageIsISO(t *testing.T) {
	if !ImageIsISO("ubuntu.iso") || !ImageIsISO("INSTALLER.ISO") {
		t.Fatal(".iso 应识别为 ISO")
	}
	if ImageIsISO("ubuntu.qcow2") || ImageIsISO("img") {
		t.Fatal("qcow2/无扩展名不应识别为 ISO")
	}
}

func TestBuildUserData(t *testing.T) {
	// 仅显式 user_data（无 keys/hostname）原样使用（补尾换行）。
	vm := model.VMFunction{Name: "fw-vm", CloudInit: &model.CloudInit{UserData: "#cloud-config\nruncmd:\n  - echo hi"}}
	got := BuildUserData(vm)
	if !strings.HasPrefix(got, "#cloud-config\nruncmd:") || !strings.HasSuffix(got, "\n") {
		t.Fatalf("仅 user-data 应原样保留并补换行: %q", got)
	}

	// user_data + SSH 公钥：须用 MIME multipart 同时注入两者（FR-CMP-016）。
	vm = model.VMFunction{Name: "fw-vm", CloudInit: &model.CloudInit{
		Hostname: "fw",
		UserData: "#cloud-config\nruncmd:\n  - echo MARK > /dev/ttyS0\n",
		SSHKeys:  []string{"ssh-ed25519 AAA test@host"},
	}}
	got = BuildUserData(vm)
	for _, want := range []string{"multipart/mixed", "text/cloud-config", "#cloud-config", "hostname: fw",
		"ssh-ed25519 AAA test@host", "runcmd:", "echo MARK"} {
		if !strings.Contains(got, want) {
			t.Errorf("multipart user-data 应含 %q\n%s", want, got)
		}
	}
	if strings.Count(got, "ssh-ed25519 AAA test@host") != 2 {
		t.Errorf("公钥应在顶层与用户级各注入一次:\n%s", got)
	}

	// 未声明 user_data：据 hostname/ssh_keys 生成最小 #cloud-config。
	vm = model.VMFunction{Name: "fw-vm", CloudInit: &model.CloudInit{
		Hostname: "fw", SSHKeys: []string{"ssh-ed25519 AAA test@host", ""},
	}}
	got = BuildUserData(vm)
	for _, want := range []string{"#cloud-config", "hostname: fw", "fqdn: fw", "ssh_authorized_keys:", "ssh-ed25519 AAA test@host", "name: nfvis"} {
		if !strings.Contains(got, want) {
			t.Errorf("user-data 缺少 %q\n%s", want, got)
		}
	}
	if strings.Count(got, "ssh-ed25519 AAA test@host") != 2 {
		t.Errorf("公钥应在顶层与用户级各注入一次:\n%s", got)
	}

	// 无 hostname 时回落 VM 名；单引号按 YAML 双写。
	vm = model.VMFunction{Name: "probe-vm", CloudInit: &model.CloudInit{SSHKeys: []string{"key'with'quote"}}}
	got = BuildUserData(vm)
	if !strings.Contains(got, "hostname: probe-vm") {
		t.Errorf("应回落 VM 名: %s", got)
	}
	if !strings.Contains(got, "key''with''quote") {
		t.Errorf("YAML 单引号应双写: %s", got)
	}

	if BuildUserData(model.VMFunction{Name: "x"}) != "" {
		t.Fatal("无 cloud-init 应返回空串")
	}
}

func TestBuildMetaData(t *testing.T) {
	vm := model.VMFunction{Name: "fw-vm", CloudInit: &model.CloudInit{Hostname: "fw"}}
	got := BuildMetaData(vm)
	if !strings.Contains(got, "instance-id: fw-vm-") || !strings.Contains(got, "local-hostname: fw") {
		t.Fatalf("meta-data 不符:\n%s", got)
	}
	vm = model.VMFunction{Name: "probe-vm"}
	got = BuildMetaData(vm)
	if !strings.Contains(got, "local-hostname: probe-vm") {
		t.Fatalf("无 hostname 应回落 VM 名:\n%s", got)
	}
}

func TestVMStateFromLibvirtAndActive(t *testing.T) {
	cases := map[int]string{
		domRunning: "running", domBlocked: "running",
		domPaused: "paused", domPMSuspended: "paused",
		domCrashed: "crashed", domShutoff: "shutoff",
		domShutdown: "shutoff", domNoState: "shutoff", 99: "shutoff",
	}
	for in, want := range cases {
		if got := VMStateFromLibvirt(in); got != want {
			t.Errorf("state %d → %q，期望 %q", in, got, want)
		}
	}
	active := map[int]bool{domRunning: true, domBlocked: true, domPaused: true, domPMSuspended: true,
		domShutoff: false, domCrashed: false, domShutdown: false, domNoState: false}
	for in, want := range active {
		if got := isActiveState(in); got != want {
			t.Errorf("isActiveState(%d) = %v，期望 %v", in, got, want)
		}
	}
}

func TestDefaultConfigAndNormalize(t *testing.T) {
	p := NewProvider(Config{}, newMockLibvirt(), newMockStorage(), nil)
	cfg := p.Config()
	if cfg.URI != DefaultURI || cfg.VMsDir == "" || cfg.VhostDir == "" || cfg.ImagesDir == "" || cfg.StopTimeout <= 0 {
		t.Fatalf("零值配置应取默认: %+v", cfg)
	}
}

// FR-CMP-017：外部 kill QEMU → SHUTOFF+CRASHED reason 应映射为 crashed。
func TestVMStateFromLibvirtReason(t *testing.T) {
	if got := VMStateFromLibvirtReason(domShutoff, shutoffReasonCrashed); got != "crashed" {
		t.Fatalf("SHUTOFF+CRASHED 应映射 crashed，实际 %q", got)
	}
	if got := VMStateFromLibvirtReason(domShutoff, 2 /*destroyed*/); got != "shutoff" {
		t.Fatalf("正常 destruction 应 shutoff，实际 %q", got)
	}
	if got := VMStateFromLibvirtReason(domRunning, 0); got != "running" {
		t.Fatalf("running 应保持 running，实际 %q", got)
	}
	if got := VMStateFromLibvirtReason(domCrashed, 0); got != "crashed" {
		t.Fatalf("CRASHED 应保持 crashed，实际 %q", got)
	}
}

func TestDeterministicUUID(t *testing.T) {
	a := DeterministicUUID("fw-vm")
	if a != DeterministicUUID("fw-vm") {
		t.Fatal("同名应恒等")
	}
	if a == DeterministicUUID("other-vm") {
		t.Fatal("不同名应不同")
	}
	if len(a) != 36 || a[14] != '5' || strings.Contains(a, " ") {
		t.Fatalf("应为 uuid v5 格式: %q", a)
	}
}

// user-data「文本或文件」归一与内容类型校验（决策 #114，round34 真机实证的坑）。
func TestResolveUserDataFileOrText(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "ud.yaml")
	content := "#cloud-config\nhostname: v1\n"
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// 绝对路径 → 读入内容
	if got, err := ResolveUserData(f); err != nil || got != content {
		t.Fatalf("绝对路径应读入文件内容: %q, %v", got, err)
	}
	// 相对路径 ./ 前缀同样识别（以临时目录为工作目录）
	cwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	if got, err := ResolveUserData("./ud.yaml"); err != nil || got != content {
		t.Fatalf("./ 路径应读入文件内容: %q, %v", got, err)
	}
	// 内联文本（不以 / 或 ./ 开头）原样返回
	if got, err := ResolveUserData("#cloud-config\nhostname: v2"); err != nil || got != "#cloud-config\nhostname: v2" {
		t.Fatalf("内联文本应原样返回: %q, %v", got, err)
	}
	// 文件不存在 → 明确报错，不静默回退成文本（用各平台都算绝对的路径）
	missing := filepath.Join(dir, "missing.yaml")
	if _, err := ResolveUserData(missing); err == nil {
		t.Fatal("路径不存在应报错（此前会静默把路径当文本注入）")
	}
}

func TestValidateUserDataType(t *testing.T) {
	ok := []string{"", "#cloud-config\nhostname: x\n", "#!/bin/sh\necho hi\n", "  #! /bin/sh\n"}
	for _, s := range ok {
		if err := ValidateUserDataType(s); err != nil {
			t.Fatalf("%q 应通过校验: %v", s, err)
		}
	}
	bad := []string{"/data/incoming/ud.yaml", "hostname: x\n", "echo hi\n"}
	for _, s := range bad {
		if err := ValidateUserDataType(s); err == nil {
			t.Fatalf("%q 应被拒绝（cloud-init 不会执行它）", s)
		}
	}
}
