package netkernel

// 数据面整体 teardown（R12-7 / R12-8）。
//
// 数据面实现（vpp/kernel）是**配置 + 整机重启**才切换的。带对象从一种数据面切到另一种后，
// 旧面的内核对象（bridge/VRF/bond/vxlan/veth/tap + 四张 nft 表）此前无人回收：它们继续
// 转发、与新面双活，残留的内核 DHCP tap 名还会占住接口名、让新面的内置 tap 创建失败。
//
// 本文件提供**一次性的整体 teardown 入口**：按 committed 声明逐个复用既有的删除原语
// （DeleteBridgeDomain / DeleteVRF / DeleteBond / DeleteVxlan / DeleteContainerVeths /
// TeardownInterface / ApplyNAT / ApplyLLDP / ApplyDNSProxy / ApplyDHCPServer 的 teardown 语义），
// 最后整体回收四张 nfvis-* nft 表。口径：
//   - **幂等**：删除原语对「对象已不存在」按已达成处理（ipBest/nftBest/notFound）；
//   - **尽力而为**：逐条收集错误、一条失败不打断其余，最后 errors.Join 上报；
//   - **不阻塞启动**：调用方给有界 ctx，失败只告警、启动继续；
//   - **不误删操作者自有设备**：只按产品派生的对象名动作（LinkName 派生的 bridge/VRF、
//     声明的 bond/vxlan、产品自持的容器 veth 名与内置 DHCP tap 名），严格身份核对由各原语承担；
//   - **失败进告警**：有对象未清理干净 ⇒ Raise（独立 scope，不被恢复对账的 Sync 误消）。

import (
	"context"
	"errors"
	"fmt"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// 数据面整体 teardown 的告警面。独立 scope：它由装配期代际切换触发一次，与恢复收敛
// （recovery scope）各自收敛——混用会被恢复对账的 Sync 误消。
const (
	alarmScopeTeardown = "dataplane-teardown"
	// AlarmTeardownFailed 数据面切换时旧的内核网络对象未全部清理干净（R12-7）。
	AlarmTeardownFailed = "DATAPLANE_TEARDOWN_FAILED"
	teardownSource      = "kernel"
)

// TeardownDataplane 数据面整体 teardown：按 committed 声明删除全部内核侧产品对象，并整体
// 回收四张 nfvis-* nft 表。幂等、尽力而为；返回未清理干净的项（nil = 全部清理完成）。
//
// 调用时机：装配期检测到数据面代际变化（上一代是内核、本次切到 VPP）时，在**装配新面之前**
// 调用一次。ctx 由调用方给上界（不阻塞启动）。
func (p *Provider) TeardownDataplane(ctx context.Context, cfg model.Config) error {
	if p == nil {
		return nil
	}
	var errs []error
	collect := func(what string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", what, err))
		}
	}

	// 1) 进程内收发面与依赖它们的 L2 服务器/中继（先解引用、后删被引用）。
	collect("lldp", p.ApplyLLDP(ctx, nil, nil))
	collect("dns-proxy", p.ApplyDNSProxy(ctx, orchestrator.DNSProxyUpstreams{}))
	collect("dhcp-relay", p.relayMgr().Close())
	for _, vs := range cfg.VirtualSwitches {
		// 未启用（无池）的声明走 teardown 语义：关单播 socket + 删内置 tap + 清租约文件。
		collect("virtual-switches/"+vs.Name+"/dhcp-server",
			p.ApplyDHCPServer(ctx, model.VirtualSwitch{Name: vs.Name}))
	}

	// 2) 容器 vNIC 宿主端 veth（产品自持；按簿记 ∪ 快照里该容器的声明逐条删）。
	for _, ct := range cfg.ContainerFunctions {
		collect("container-functions/"+ct.Name, p.DeleteContainerVeths(ctx, ct.Name))
	}

	// 3) 接口级绑定（限速/风暴抑制/端口安全）与镜像会话。
	for _, iface := range cfg.Interfaces {
		collect("interfaces/"+iface.Name, p.TeardownInterface(ctx, iface))
	}
	for _, pm := range cfg.PortMirroring {
		src, err := spanSourceDev(pm)
		if err != nil {
			collect("port-mirroring/"+pm.Name, err)
			continue
		}
		dir := pm.Source.Direction
		if dir == "" {
			dir = "both"
		}
		collect("port-mirroring/"+pm.Name, p.spanMgr().Delete(ctx, src, dir))
	}

	// 4) 策略对象与 NAT 表。
	for _, acl := range cfg.Acls {
		collect("acls/"+acl.Name, p.aclMgr().Delete(ctx, acl.Name))
	}
	for _, q := range cfg.QosPolicies {
		collect("qos/"+q.Name, p.DeleteQos(ctx, q.Name))
	}
	collect("nat", p.ApplyNAT(ctx, model.NatConfig{}))

	// 5) 交换机 / VRF / bond / vxlan（被引用者后删；内核自动释放成员口）。
	for _, vs := range cfg.VirtualSwitches {
		collect("virtual-switches/"+vs.Name, p.DeleteBridgeDomain(ctx, vs.Name))
	}
	for _, vrf := range cfg.Vrfs {
		collect("vrfs/"+vrf.Name, p.DeleteVRF(ctx, vrf.Name))
	}
	for _, b := range cfg.Bonds {
		collect("bonds/"+b.Name, p.DeleteBond(ctx, b.Name))
	}
	for _, vx := range cfg.VxlanTunnels {
		collect("vxlan/"+vx.Name, p.DeleteVxlan(ctx, vx))
	}

	// 6) 四张 nfvis-* nft 表整体回收（幂等；不存在按已达成）。放在最后：即使前面某族删除
	//    失败，nft 表也能被整体清掉，不留「配置已清、表还在」的残留（含转发前置链 nfvis-forward）。
	collect("nft-tables", p.deleteNftTables(ctx))

	err := errors.Join(errs...)
	p.recordTeardownOutcome(err)
	return err
}

// deleteNftTables 整体回收四张产品自持的 nft 表（netdev nfvis-acl / netdev nfvis-portsec /
// inet nfvis-nat / inet nfvis-forward）。幂等：表不存在按已达成。
//
// ⚠️ round23 真机更正：这里曾把**族常量** `natFamilyName`（"inet"）当**表名**用，生成
// `nft delete table inet inet`——它必报语法错 ⇒ 每次内核→VPP 切换都误判「未清理干净」并
// 落假告警 `DATAPLANE_TEARDOWN_FAILED`（真残留对象其实已被步骤 4 的 `ApplyNAT` 空声明回收；
// 单测的 `has()` 子串断言被那条正确命令喂绿，抓不到本处——故测试补了负面断言）。
func (p *Provider) deleteNftTables(ctx context.Context) error {
	tables := []struct{ family, name string }{
		{"netdev", aclTableName},
		{"netdev", portSecTableName},
		{"inet", natTable},
		{"inet", forwardTable},
	}
	var errs []error
	for _, t := range tables {
		if err := nftBestOn(ctx, p.run, "delete", "table", t.family, t.name); err != nil {
			errs = append(errs, fmt.Errorf("nft delete table %s %s: %w", t.family, t.name, err))
		}
	}
	return errors.Join(errs...)
}

// recordTeardownOutcome 把一次整体 teardown 的结局落到告警表（独立 scope）：
// 全部清理完成 ⇒ 消解；有残留 ⇒ Raise（附对象清单与自查路径）。未注入告警表即只返回错误。
func (p *Provider) recordTeardownOutcome(err error) {
	if p.alarms == nil {
		return
	}
	if err == nil {
		p.alarms.Resolve(alarmScopeTeardown, AlarmTeardownFailed, teardownSource)
		return
	}
	p.alarms.Raise(alarmScopeTeardown, network.SeverityWarning, AlarmTeardownFailed,
		"数据面切换时旧的内核网络对象未全部清理："+err.Error()+
			"。这些对象可能继续转发、或占用接口名（如内置 DHCP tap），与新数据面冲突；"+
			"请用 ip link / nft list tables 复核，排查后重试切换或重启 nfvis 服务",
		teardownSource)
}
