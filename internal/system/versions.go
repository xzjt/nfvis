package system

// 组件版本探测（R37-2 收口，决策 #118）。
//
// 由来：`GET /system/version` 曾长期是**桩**——7 个键都在、只有 `nfvis` 有值，
// 其余全空串（CLI `show version` 同样只印 NFViS 版本，而命令树契约写的是七组件汇总）。
//
// 口径（与响应形状守护的白名单一致）：
//   - 只汇报**探测到的**版本——取不到的键直接不给，不编造空串（"给了但恒空"与
//     "没实现"是一回事，决策 #116 的判据）；
//   - DPDK **没有可靠来源**：本机 DPDK 静态链接进 VPP 的 dpdk_plugin.so，VPP binary API
//     的 show_version 不含 DPDK 版本，运行进程也没有独立 DPDK 库可读，故有意永不汇报；
//   - 每条命令 2s 上限：本探测在 Web 控制台的轮询路径上（页面每 5 秒刷新一次），
//     不能因底座命令卡住而拖住响应（同 clock.go 的探针超时思路）。

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// versionProbeTimeout 单条探测命令的上限。
const versionProbeTimeout = 2 * time.Second

// VersionProbe 组件版本探测器。run 与 osReleasePath 可注入（单测用桩，不打真命令）。
type VersionProbe struct {
	run           func(ctx context.Context, name string, args ...string) (string, error)
	osReleasePath string
}

// NewVersionProbe 构造经 PATH 执行真实命令、读 /etc/os-release 的探测器。
func NewVersionProbe() *VersionProbe {
	return &VersionProbe{
		run: func(ctx context.Context, name string, args ...string) (string, error) {
			c, cancel := context.WithTimeout(ctx, versionProbeTimeout)
			defer cancel()
			out, err := exec.CommandContext(c, name, args...).Output()
			return string(out), err
		},
		osReleasePath: "/etc/os-release",
	}
}

// Components 探测宿主与底座组件版本，返回"键 → 版本"（**只含探测到的**）。
// 键名与契约 VersionInfo 一致：ubuntu/libvirt/qemu/docker。vpp 由调用方经
// VppController 提供（与 /vpp/status 同源），nfvis 是构建期常量，dpdk 无来源——都不在这里。
func (p *VersionProbe) Components(ctx context.Context) map[string]string {
	out := map[string]string{}
	if v := ubuntuVersion(p.osReleasePath); v != "" {
		out["ubuntu"] = v
	}
	if v := p.cmdVersion(ctx, "libvirtd", []string{"--version"}, parseLibvirtVersion); v != "" {
		out["libvirt"] = v
	}
	if v := p.cmdVersion(ctx, "qemu-system-x86_64", []string{"--version"}, parseQEMUVersion); v != "" {
		out["qemu"] = v
	}
	// docker 取**守护进程**版本（容器实际由它运行）；守护进程没起时取不到，不给。
	if v := p.cmdVersion(ctx, "docker", []string{"version", "--format", "{{.Server.Version}}"}, nil); v != "" {
		out["docker"] = v
	}
	return out
}

// cmdVersion 执行一条探测命令并按 parse 取版本；失败/解析不到 → 空串。
func (p *VersionProbe) cmdVersion(ctx context.Context, name string, args []string, parse func(string) string) string {
	out, err := p.run(ctx, name, args...)
	if err != nil {
		return ""
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	if parse == nil {
		return out
	}
	return strings.TrimSpace(parse(out))
}

// ubuntuVersion 读 os-release 的 VERSION_ID（取不到退回 PRETTY_NAME）。
// 非 Linux 上没有该文件 → 空串（调用方据此不给该键，不伪造）。
func ubuntuVersion(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var versionID, pretty string
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		switch strings.TrimSpace(k) {
		case "VERSION_ID":
			versionID = v
		case "PRETTY_NAME":
			pretty = v
		}
	}
	if versionID != "" {
		return versionID
	}
	return pretty
}

// parseLibvirtVersion "libvirtd (libvirt) 12.0.0" → "12.0.0"（取最后一个字段）。
func parseLibvirtVersion(out string) string {
	f := strings.Fields(out)
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}

// parseQEMUVersion "QEMU emulator version 10.2.1 (Debian …)" → "10.2.1"。
func parseQEMUVersion(out string) string {
	m := regexp.MustCompile(`version\s+([0-9][^\s(]+)`).FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	return m[1]
}
