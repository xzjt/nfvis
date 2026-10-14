package compute

import (
	"os"
	"strings"
)

// uefiFirmwareDirs OVMF（UEFI 固件）在常见发行版中的安装目录（best-effort 探测范围）。
var uefiFirmwareDirs = []string{
	"/usr/share/OVMF",
	"/usr/share/ovmf",
	"/usr/share/edk2/ovmf",
	"/usr/share/edk2-ovmf",
}

// hostUEFIFirmwareMissing 尽力探测宿主是否缺少 UEFI 固件（OVMF）（决策 #485 复审）。
//
// 只有**能做出判断**时才下结论：以 POSIX 形态的 /usr/share 存在、且上述候选目录均不含 .fd
// 固件文件为「缺少」判据。否则（非 POSIX 宿主、未知目录布局）保守返回「不缺」——宁可让 libvirt
// 到定义期自行报错，也不误拒合法配置（同时保证跨平台单测不受宿主差异影响）。
func hostUEFIFirmwareMissing() (bool, string) {
	const shareRoot = "/usr/share"
	st, err := os.Stat(shareRoot)
	if err != nil || !st.IsDir() {
		return false, "" // 无法判定（非 POSIX 宿主/未知布局）⇒ 保守放行
	}
	for _, dir := range uefiFirmwareDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".fd") {
				return false, "" // 找到固件文件（OVMF_CODE*.fd 等）
			}
		}
	}
	return true, "标准位置（" + strings.Join(uefiFirmwareDirs, "、") + "）下未发现任何 .fd UEFI 固件文件"
}
