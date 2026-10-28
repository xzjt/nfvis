package model

// round51 评审：四条提交期校验缺口的守护（每条都先红后绿）。
//
// 覆盖：
//   - F1 数据盘名（`vm.Disks[].Name`）此前零校验，却被拼进真实文件路径（compute.DataDiskPath
//     的 `data-<名>.qcow2`，root 权限 MkdirAll + qemu-img）——`../` 可越出仓库；
//   - F2 内核附加参数（`system.kernel.params`）此前零校验，整条原样拼进 root 执行的 GRUB 片段；
//   - F3 NTP 服务器取值此前只判非空，会写进 root 拥有的 chrony 源文件；
//   - F4 两个各自合法的 vNIC 声明可派生同一个 VPP 接口名（VPP 名全局唯一）。

import (
	"strings"
	"testing"
)

// F1：数据盘名与其它资源名同档校验（nameRe），越界取值在提交期就被拒。
func TestFixR51VMDataDiskNameValidated(t *testing.T) {
	c := validBase()
	c.VirtualMachineFunctions[0].Disks = []VMDisk{{Name: "../../../../etc/nfvis-pwn", SizeGB: 8}}
	errs := Validate(c)
	mustErrContaining(t, errs, ".disks[", "数据盘")

	// 负控：合法数据盘名照旧放行（本修复不得把正确写法也拦下）。
	ok := validBase()
	ok.VirtualMachineFunctions[0].Disks = []VMDisk{{Name: "data0", SizeGB: 8}}
	mustNoErr(t, Validate(ok))
}

// F2a：内核启动基线的每个字段按它落到哪里逐个收严（params 是自由文本逃生口）。
func TestFixR51KernelBaselineValidated(t *testing.T) {
	// params 里的引号能闭合 GRUB_CMDLINE_LINUX 赋值、把其余内容当 root 命令执行。
	c := validBase()
	c.System.Kernel = &KernelConfig{Params: []string{`quiet mitigation"=off`}}
	mustErrContaining(t, Validate(c), "system.kernel.params[0]", "内核附加参数")

	// 枚举字段：iommu / transparent_hugepages 不在契约取值内一律拒绝。
	c2 := validBase()
	c2.System.Kernel = &KernelConfig{IOMMU: "yes"}
	mustErrContaining(t, Validate(c2), "system.kernel.iommu", "on|off|pt")

	c3 := validBase()
	c3.System.Kernel = &KernelConfig{TransparentHugepages: "sometimes"}
	mustErrContaining(t, Validate(c3), "system.kernel.transparent_hugepages", "always|madvise|never")

	// tuned 档位名会进 GRUB 片段注释行与 /etc/nfvis/tuned-profile，须是安全名字。
	c4 := validBase()
	c4.System.Kernel = &KernelConfig{TunedProfile: "bad name\nGRUB_CMDLINE_LINUX=evil"}
	mustErrContaining(t, Validate(c4), "system.kernel.tuned_profile", "tuned 档位名")

	// 负控：合法取值（内核常用写法 + 合法枚举 + 合法档位名）照旧放行。
	ok := validBase()
	ok.System.Kernel = &KernelConfig{
		Params:               []string{"isolcpus=2-5", "default_hugepagesz=1G", "console=ttyS0,115200n8", "processor.max_cstate=1"},
		IOMMU:                "pt",
		TransparentHugepages: "madvise",
		TunedProfile:         "nfvis-throughput",
	}
	mustNoErr(t, Validate(ok))
}

// F3a：NTP 服务器取值须是 IP 或安全主机名（换行能向 chrony 源文件注入任意指令）。
func TestFixR51NtpServerValidated(t *testing.T) {
	c := validBase()
	c.System.Ntp = []NtpServer{{Server: "10.0.0.1\nserver evil.example"}}
	mustErrContaining(t, Validate(c), "system.ntp[0].server", "NTP 服务器")

	// 前导 '-' 与空白/引号同样拒绝。
	for _, bad := range []string{"-oProxyCommand=x", "pool ntp.example", `x"y`} {
		cc := validBase()
		cc.System.Ntp = []NtpServer{{Server: bad}}
		mustErrContaining(t, Validate(cc), "system.ntp[0].server", "NTP 服务器")
	}

	// 负控：IP（v4/v6）与主机名（含连字符）照旧放行。
	ok := validBase()
	ok.System.Ntp = []NtpServer{{Server: "10.0.0.1"}, {Server: "2001:db8::1"}, {Server: "ntp.ubuntu.com"}, {Server: "time-1.example"}}
	mustNoErr(t, Validate(ok))
}

// F4：两个各自合法的声明派生同一个 VPP 接口名时提交期拒绝，并点名两者的归属路径。
func TestFixR51VPPDerivedNameCollision(t *testing.T) {
	c := validBase()
	// 两处都合法（名字都过 nameRe），却都派生 `vh-a-b-c`：VM `a` 的 vNIC `b-c` 与 VM `a-b` 的 vNIC `c`。
	c.VirtualMachineFunctions = []VMFunction{
		fixR51VM("a", "b-c"),
		fixR51VM("a-b", "c"),
	}
	errs := Validate(c)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "vh-a-b-c") && strings.Contains(e.Message, `"a"`) &&
			strings.Contains(e.Message, `"a-b"`) {
			found = true
			if !strings.Contains(e.Path, "interfaces") {
				t.Fatalf("撞名错误应定位到具体 vNIC 路径，实际: %s", e.Path)
			}
		}
	}
	if !found {
		t.Fatalf("派生名撞名应被拒绝并点名两个归属，实际: %v", errs)
	}

	// 负控：派生名不同的两个 VM 照旧合法。
	ok := validBase()
	ok.VirtualMachineFunctions = []VMFunction{fixR51VM("a", "b-c"), fixR51VM("x", "y-z")}
	mustNoErr(t, Validate(ok))
}

// fixR51VM 造一台最小可过校验的 VM（vhost-user 单口，名字与 vNIC 名由调用方给定）。
func fixR51VM(vm, nic string) VMFunction {
	return VMFunction{
		Name:   vm,
		Image:  "img",
		VCPU:   VMCpu{Count: 2},
		Memory: VMMemory{SizeMB: 4096},
		Interfaces: []VnfInterface{
			{Name: nic, Type: "vhost-user", VirtualSwitch: "vs-app"},
		},
	}
}
