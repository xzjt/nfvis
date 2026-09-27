package system

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// libvirt 的 AppArmor 助手（virt-aa-helper）默认只放行 /var/lib/libvirt/images 等路径；NFViS 的
// 镜像与 VM 磁盘在 /var/lib/nfvis 下，放行缺失时 VM 启动会被 AppArmor 拒读（干净快照首装实测）。
//
// 为什么由这里（运行期）保证，而不是只在安装脚本里做（决策 #182）：libvirt 可能**晚于** nfvis 安装，
// 或与 nfvis 落在**同一次 apt 事务**里而被 apt 先配置 nfvis、后配置 libvirt —— 那时安装脚本的探测
// 条件还不成立，放行会被静默跳过：装机全程报成功，VNF 直到被启动时才失败（round85 干净快照离线
// 安装实测）。运行期幂等补齐与安装顺序无关，也覆盖「事后才装 libvirt」的场景；安装期脚本与本实现
// 共用同一份规则（单源），不再各自内联一段。
const (
	// AALibvirtProfile 是 libvirt 助手的 profile（随 libvirt 包安装）。
	AALibvirtProfile = "/etc/apparmor.d/usr.lib.libvirt.virt-aa-helper"
	// AALibvirtLocal 是它的本地放行片段（发行版约定：升级不覆盖该文件）。
	AALibvirtLocal = "/etc/apparmor.d/local/usr.lib.libvirt.virt-aa-helper"
	// aaMarker 用于幂等判定：本地片段里已含它就不再追加。
	aaMarker = "/var/lib/nfvis/images"
)

// aaBlock 是追加到本地片段的放行规则（只读镜像 + 读写加锁 VM 磁盘）。
const aaBlock = "\n# NFViS 镜像与 VM 磁盘路径\n" +
	"  /var/lib/nfvis/images/** r,\n" +
	"  /var/lib/nfvis/vms/** rk,\n"

// AppArmorLibvirt 保证 libvirt 的 AppArmor 助手放行 NFViS 路径（幂等，可重复调用）。
type AppArmorLibvirt struct {
	Runner      Runner // 重载 profile（apparmor_parser）；nil = 只写文件不重载（测试用）
	ProfilePath string // 空 = AALibvirtProfile
	LocalPath   string // 空 = AALibvirtLocal
}

func (a *AppArmorLibvirt) profilePath() string {
	if a.ProfilePath != "" {
		return a.ProfilePath
	}
	return AALibvirtProfile
}

func (a *AppArmorLibvirt) localPath() string {
	if a.LocalPath != "" {
		return a.LocalPath
	}
	return AALibvirtLocal
}

// Ensure 幂等补齐放行：profile 不在（本机没装 libvirt / 非该发行版布局）或已放行时都不动系统。
// 返回 changed = 本次是否真的改了；重载失败会带错误返回（写进去了但没生效，不能当成功）。
func (a *AppArmorLibvirt) Ensure(ctx context.Context) (bool, error) {
	profile, local := a.profilePath(), a.localPath()
	if _, err := os.Stat(profile); err != nil {
		return false, nil
	}
	if b, err := os.ReadFile(local); err == nil && strings.Contains(string(b), aaMarker) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return false, fmt.Errorf("建目录 %s: %w", filepath.Dir(local), err)
	}
	f, err := os.OpenFile(local, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return false, fmt.Errorf("打开 %s: %w", local, err)
	}
	if _, err := f.WriteString(aaBlock); err != nil {
		_ = f.Close()
		return false, fmt.Errorf("写 %s: %w", local, err)
	}
	if err := f.Close(); err != nil {
		return false, fmt.Errorf("写 %s: %w", local, err)
	}
	if a.Runner != nil {
		if _, err := a.Runner(ctx, "apparmor_parser", "-r", profile); err != nil {
			return true, fmt.Errorf("重载 %s: %w", profile, err)
		}
	}
	return true, nil
}
