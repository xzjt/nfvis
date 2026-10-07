package netkernel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

// 接口入向端口安全（允许源 MAC 白名单）的内核数据面实现。
//
// 与 VPP 侧（macip ACL 绑定）语义对齐，底座换成「bridge 关学习 + nftables 入向丢帧」两件：
//   - `bridge link set dev <dev> learning off`：停止该桥口学习新源 MAC；
//   - nftables `netdev` 表 `nfvis-portsec` 里每接口一条 ingress 链，规则
//     `ether saddr != { <白名单> } drop`：源 MAC 不在白名单的帧直接丢弃（IP 帧与非 IP 帧一律）。
//
// 为什么用 netdev 家族而不是 inet：真机实测（见交付说明）`inet` 家族的 `hook ingress` 虽被
// nft 接受、却收不到任何帧（IPv4 帧同样为 0），`inet`/`bridge` 的 prerouting 也看不到被桥转发
// 的帧；只有 `netdev` 家族的 ingress 挂在设备 tc ingress 点上、按 L2 收到全部帧（含 ARP 等非 IP
// 帧），与「按源 MAC、不分 L3 协议」的语义相符。
//
// 独立的表（不碰 ACL/NAT 族的表）。内核即事实源：读视图直接查 nft 规则集与桥口学习状态，
// 不维护进程内登记。

const (
	portSecTableFamily = "netdev"
	portSecTableName   = "nfvis-portsec"
	portSecChainPrefix = "ps_"
)

// portSecChainName 该接口的 nft 链名（每接口一条 ingress 链）。
func portSecChainName(dev string) string { return portSecChainPrefix + dev }

// portSecManager 接口入向端口安全的内核下发与读视图。
type portSecManager struct{ run Runner }

// newPortSecManager 以给定 Runner 构造（run 为 nil 时用真实宿主命令）。
func newPortSecManager(run Runner) *portSecManager {
	if run == nil {
		run = NewExecRunner()
	}
	return &portSecManager{run: run}
}

// Apply 把接口的端口安全收敛到声明；macs 为空表示「未声明」——撤除。
func (m *portSecManager) Apply(ctx context.Context, dev string, macs []model.PortSecMAC) error {
	if len(macs) == 0 {
		return m.Teardown(ctx, dev)
	}
	if dev == "" {
		return fmt.Errorf("端口安全接口名不能为空")
	}
	// 先关桥口学习：设备不是 L2 成员时这一步就报错（比先建 nft 对象更早暴露配置错误）。
	// 判据取实况（是否已是 bridge 成员）而不是"试着设一下"——`bridge link set` 对非成员口
	// 报的是 `RTNETLINK answers: Operation not supported`，看不出真正原因。
	if member, err := m.isBridgeMember(ctx, dev); err != nil {
		return err
	} else if !member {
		return fmt.Errorf("接口 %s 不是二层交换机成员口，端口安全需要它先挂到交换机上", dev)
	}
	if err := bridgeReq(ctx, m.run, "link", "set", "dev", dev, "learning", "off"); err != nil {
		return err
	}
	chain := portSecChainName(dev)
	spec := fmt.Sprintf("type filter hook ingress device %s priority filter ;", dev)
	if err := ensureTableChain(ctx, m.run, portSecTableFamily, portSecTableName, chain, spec); err != nil {
		return err
	}
	// 先清空本接口的链、再按声明加（幂等：白名单变化/重放都从干净态重建）。
	if err := nftReqOn(ctx, m.run, "flush", "chain", portSecTableFamily, portSecTableName, chain); err != nil {
		return err
	}
	return nftReqOn(ctx, m.run, "add", "rule", portSecTableFamily, portSecTableName, chain,
		"ether", "saddr", "!=", "{", portSecMacSet(macs), "}", "drop")
}

// Teardown 撤除该接口的端口安全并恢复桥口学习（幂等：对象本就不在按已达成）。
func (m *portSecManager) Teardown(ctx context.Context, dev string) error {
	if dev == "" {
		return fmt.Errorf("端口安全接口名不能为空")
	}
	if err := deleteChain(ctx, m.run, portSecTableFamily, portSecTableName, portSecChainName(dev)); err != nil {
		return err
	}
	if err := m.dropTableIfEmpty(ctx); err != nil {
		return err
	}
	// 恢复学习：只在设备**确实还是 bridge 成员**时才设——非成员口上 `bridge link set`
	// 会报 `RTNETLINK answers: Operation not supported`（真机实测：接口声明早于交换机下发
	// 时必然走到这里），那不是失败，而是"没有学习可恢复"。设备已不在时同样跳过。
	member, err := m.isBridgeMember(ctx, dev)
	if err != nil || !member {
		return nil
	}
	return bridgeBest(ctx, m.run, "link", "set", "dev", dev, "learning", "on")
}

// isBridgeMember 该设备当前是否挂在某个 bridge 下（`ip -j -d link show` 的 master 字段）。
// 设备不存在 ⇒ (false, nil)（"不是成员"）；读取失败 ⇒ 如实报错。
func (m *portSecManager) isBridgeMember(ctx context.Context, dev string) (bool, error) {
	out, err := m.run.Run(ctx, "ip", "-j", "-d", "link", "show", "dev", dev)
	if err != nil {
		if notFound(out, err) {
			return false, nil
		}
		return false, fmt.Errorf("ip -j -d link show dev %s: %w（%s）", dev, err, trimOut(out))
	}
	var rows []struct {
		Master string `json:"master"`
	}
	if json.Unmarshal([]byte(out), &rows) != nil {
		return false, fmt.Errorf("解析 ip -j -d link show 输出失败：%s", trimOut(out))
	}
	return len(rows) > 0 && rows[0].Master != "", nil
}

// Dataplane 读该接口端口安全的内核实况：白名单规则是否在场、桥口学习是否已关闭。
// err != nil 表示「读不到实况」（例如设备不存在），而不是「未下发」。
func (m *portSecManager) Dataplane(ctx context.Context, dev string) (attached bool, detail string, err error) {
	rulePresent, macCount, err := m.chainState(ctx, dev)
	if err != nil {
		return false, "", err
	}
	learningOff, err := m.learningOff(ctx, dev)
	if err != nil {
		return false, "", err
	}
	switch {
	case rulePresent && learningOff:
		return true, fmt.Sprintf("白名单 %d 条源 MAC；桥口学习已关闭（入向丢弃白名单外源 MAC）", macCount), nil
	case rulePresent:
		return false, fmt.Sprintf("白名单 %d 条源 MAC 已下发，但桥口学习未关闭（端口安全未完全生效）", macCount), nil
	case learningOff:
		return false, "桥口学习已关闭，但白名单规则不在（端口安全未完全生效）", nil
	default:
		return false, "未下发端口安全", nil
	}
}

// dropTableIfEmpty 本产品的表里没有别的接口的链时回收整张表。
//
// 表是各接口共用的（一接口一条链）：删本接口的链后若还有别的接口在用，必须留着；
// 都没有了才删表（否则会在机器上长期留一张空表）。
func (m *portSecManager) dropTableIfEmpty(ctx context.Context) error {
	out, err := nftRunOn(ctx, m.run, "list", "table", portSecTableFamily, portSecTableName)
	if err != nil {
		if notFound(out, err) {
			return nil
		}
		return fmt.Errorf("nft list table %s %s: %w（%s）", portSecTableFamily, portSecTableName, err, trimOut(out))
	}
	if strings.Contains(out, "chain ") {
		return nil // 还有别的接口的链在用这张表
	}
	return nftBestOn(ctx, m.run, "delete", "table", portSecTableFamily, portSecTableName)
}

// chainState 读该接口 nft 链的实况：白名单规则是否存在、白名单条数。
// 链（或表）不存在 = 未下发（不是错误）；其余 nft 失败如实上报。
func (m *portSecManager) chainState(ctx context.Context, dev string) (present bool, macCount int, err error) {
	out, err := nftRunOn(ctx, m.run, "list", "chain", portSecTableFamily, portSecTableName, portSecChainName(dev))
	if err != nil {
		if notFound(out, err) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("nft list chain %s %s %s: %w（%s）",
			portSecTableFamily, portSecTableName, portSecChainName(dev), err, trimOut(out))
	}
	if !strings.Contains(out, "ether saddr") || !strings.Contains(out, "drop") {
		return false, 0, nil
	}
	return true, portSecRuleMACCount(out), nil
}

// learningOff 读该桥口是否已关闭学习（`ip -j -d link show` 的 bridge_slave.learning）。
// 设备不存在 = 无法判定（返回错误）；设备在但不是桥口 = 学习无从关闭（返回 false）。
func (m *portSecManager) learningOff(ctx context.Context, dev string) (bool, error) {
	out, err := m.run.Run(ctx, "ip", "-j", "-d", "link", "show", "dev", dev)
	if err != nil {
		if notFound(out, err) {
			return false, fmt.Errorf("设备 %s 不存在，无法读取端口安全实况", dev)
		}
		return false, fmt.Errorf("ip -j -d link show dev %s: %w（%s）", dev, err, trimOut(out))
	}
	var rows []portSecLinkRow
	if json.Unmarshal([]byte(out), &rows) != nil || len(rows) == 0 {
		return false, fmt.Errorf("解析 ip -j -d link show dev %s 输出失败", dev)
	}
	li := rows[0].LinkInfo
	if li == nil || li.SlaveData == nil {
		return false, nil // 不是桥口：学习无从关闭
	}
	return !li.SlaveData.Learning, nil
}

// portSecLinkRow `ip -j -d link show` 里端口安全读视图需要的字段（桥口学习状态）。
type portSecLinkRow struct {
	LinkInfo *struct {
		SlaveData *struct {
			Learning bool `json:"learning"`
		} `json:"info_slave_data"`
	} `json:"linkinfo"`
}

// portSecMacSet 把白名单拼成 nft 集合字面量（`m1, m2`；调用方用 `{`/`}` 包起来）。
func portSecMacSet(macs []model.PortSecMAC) string {
	parts := make([]string, 0, len(macs))
	for _, m := range macs {
		if s := strings.TrimSpace(string(m)); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ", ")
}

// portSecRuleMACCount 从 nft 链输出里数出白名单条数（规则形如
// `ether saddr != { m1, m2 } drop`；无花括号时按 1 计）。取不到返回 0。
func portSecRuleMACCount(chainOut string) int {
	i := strings.Index(chainOut, "ether saddr !=")
	if i < 0 {
		return 0
	}
	rest := chainOut[i:]
	j := strings.Index(rest, "{")
	if j < 0 {
		return 1 // 单条（无花括号）
	}
	k := strings.Index(rest[j:], "}")
	if k < 0 {
		return 0
	}
	n := 0
	for _, part := range strings.Split(rest[j+1:j+k], ",") {
		if strings.TrimSpace(part) != "" {
			n++
		}
	}
	return n
}
