package netkernel

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/xzjt/nfvis/internal/model"
)

// DefaultVxlanDstPort VXLAN 缺省目的端口（与内核 vxlan 驱动缺省一致）。
const DefaultVxlanDstPort = 4789

// ApplyVxlan 收敛一条 VXLAN 隧道为内核 vxlan 设备。
//
// 元组变化时先按旧元组删设备再按新元组建（内核 vxlan 的 local/remote/vni 不可原地改）：
// prev 非 nil 时按它判；prev 为 nil（恢复重放/备份恢复路径）时**读本地设备的实际元组**比对
// ——旧实现把 `File exists` 一吞了之，本地隧道保持旧 VNI/remote，配置与数据面不一致且无提示
// （R2-13②）。VirtualSwitch 非空时把隧道口挂进该交换机对应的 bridge。
func (p *Provider) ApplyVxlan(ctx context.Context, t model.VxlanTunnel, prev *model.VxlanTunnel) error {
	if prev != nil && !sameVxlanTuple(*prev, t) {
		if err := p.DeleteVxlan(ctx, *prev); err != nil {
			return err
		}
	}
	dev := LinkName(t.Name)
	port := effectiveDstPort(t.DstPort)
	if row, ok := p.linkDetail(ctx, dev); ok && vxlanAttrsDiffer(row.LinkInfo, t, port) {
		if err := p.DeleteVxlan(ctx, t); err != nil {
			return err
		}
	}
	if err := p.ipIdem(ctx, "link", "add", dev, "type", "vxlan", "id", fmt.Sprint(t.Vni),
		"local", t.Local, "remote", t.Remote, "dstport", fmt.Sprint(port)); err != nil {
		return err
	}
	if err := p.ensureLinkUp(ctx, dev); err != nil {
		return err
	}
	if t.VirtualSwitch != "" {
		if err := p.ipReq(ctx, "link", "set", "dev", dev, "master", LinkName(t.VirtualSwitch)); err != nil {
			return err
		}
	}
	return nil
}

// vxlanInfoData `ip -d -j link show` 里 vxlan 设备的参数（linkinfo.info_data）。
//
// 同一含义在不同 iproute2 版本下的键名有差异（`id`/`vxlan_id`、`dstport`/`port`），两种都读；
// 两边都为 0/空表示该版本没输出该项。
type vxlanInfoData struct {
	ID      int    `json:"id"`
	VxlanID int    `json:"vxlan_id"`
	Local   string `json:"local"`
	Remote  string `json:"remote"`
	DstPort int    `json:"dstport"`
	Port    int    `json:"port"`
}

// vxlanAttrsDiffer 已在场的 vxlan 设备元组是否**可证**与声明不同（可证不同才删掉重建）。
//
// 读不到的字段按「无法证明不同」处理：重建会短暂中断隧道，不能凭猜测反复做。
func vxlanAttrsDiffer(li *ipLinkInfo, t model.VxlanTunnel, port int) bool {
	if li == nil || li.InfoKind != "vxlan" {
		return true // 同名设备不是 vxlan：必须重建
	}
	if li.InfoData == nil {
		return false
	}
	var d vxlanInfoData
	if json.Unmarshal(li.InfoData, &d) != nil {
		return false
	}
	id := d.ID
	if id == 0 {
		id = d.VxlanID
	}
	if id != 0 && id != t.Vni {
		return true
	}
	if d.Local != "" && d.Local != t.Local {
		return true
	}
	if d.Remote != "" && d.Remote != t.Remote {
		return true
	}
	curPort := d.DstPort
	if curPort == 0 {
		curPort = d.Port
	}
	if curPort != 0 && curPort != port {
		return true
	}
	return false
}

// DeleteVxlan 撤销一条 VXLAN 隧道（先出 bridge 再删设备）。
func (p *Provider) DeleteVxlan(ctx context.Context, t model.VxlanTunnel) error {
	dev := LinkName(t.Name)
	if err := p.ipBest(ctx, "link", "set", "dev", dev, "nomaster"); err != nil {
		return err
	}
	return p.ipBest(ctx, "link", "del", dev)
}

// sameVxlanTuple 两条隧道的下垫/标识元组是否一致（一致则无需重建）。
func sameVxlanTuple(a, b model.VxlanTunnel) bool {
	return a.Vni == b.Vni && a.Local == b.Local && a.Remote == b.Remote &&
		effectiveDstPort(a.DstPort) == effectiveDstPort(b.DstPort) && a.VirtualSwitch == b.VirtualSwitch
}

func effectiveDstPort(p int) int {
	if p == 0 {
		return DefaultVxlanDstPort
	}
	return p
}
