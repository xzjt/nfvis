package network

// 解绑守卫（发现 #13，决策 #102）：**不要解绑仍被数据面占用的口**。
//
// 为什么：2026-09-18 真机实测（本人踩到）——对正在被 VPP 使用的口执行
// `request interfaces <PCI> unbind-dpdk to-driver vmxnet3 --yes`：
//   · 内核侧的 vfio 解绑/重绑写会**阻塞**（VPP 还持着该设备的 vfio group），请求永不返回；
//   · 设备被摘出 vfio-pci 后**停在无驱动**状态；
//   · 而 cliExecutor.Execute **全程持锁**，卡住的那条命令把整个 CLI 通道占死——
//     此后任何 CLI 命令都超时，只有重启 nfvisd 才恢复（REST 其它端点反而正常）。
// 恢复顺序（真机验证）：`systemctl stop vpp`（释放 vfio group）→ `systemctl restart nfvis` →
// `request vpp restart`。
//
// 正确做法是**先让口离开数据面**：在配置里删掉它的 DPDK 声明与接口声明 → `request vpp restart`
// （确认它已不在数据面）→ 再解绑。这也是产品一贯的「声明式先收敛，再动底座」。
//
// 判据只取一条**高置信度事实**：该口此刻确实在数据面（VPP）里。探测不到（VPP 未连接等）
// **不拦**——VPP 都没跑就没有数据面占用它，此时解绑是安全的。

import (
	"errors"
	"fmt"
	"strings"
)

// ErrIfaceInDataplane 拒绝对仍被数据面占用的接口做驱动层操作。
var ErrIfaceInDataplane = errors.New("接口仍被数据面占用")

// CheckUnbindAllowed 判定解绑是否安全：inDataplane 为真时拒绝并给出正确顺序。
func CheckUnbindAllowed(ifname string, inDataplane bool) error {
	name := strings.TrimSpace(ifname)
	if name == "" || !inDataplane {
		return nil
	}
	return fmt.Errorf("%w：%s 当前仍在数据面中。请先在配置里删除它的 DPDK 声明与接口声明，"+
		"执行 request vpp restart 并确认它已不在数据面，然后再解绑——"+
		"直接解绑会让该口悬空，并可能阻塞后续命令",
		ErrIfaceInDataplane, name)
}
