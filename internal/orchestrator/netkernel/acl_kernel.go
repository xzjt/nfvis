package netkernel

import (
	"context"
	"fmt"
	"hash/fnv"
	"net"
	"sort"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

// ACL（五元组访问控制）的内核数据面实现。
//
// 与 VPP 侧（acl plugin 的 acl-in/acl-out）语义对齐，底座换成 nftables：
//   - 独立表 `netdev nfvis-acl`（不碰 NAT 表，也不碰端口安全表）；
//   - 每条 ACL 一条普通链 `acl_<派生名>`，规则按 Seq 顺序下发，链尾**无条件 drop**
//     （对齐 VPP「ACL 末尾隐式拒绝」的语义）；
//   - 绑定接口时给该接口建一条 `netdev` 家族的 ingress 基链 `bind_<dev>`，链内先
//     `meta protocol != { ip, ip6 } accept`（放行非 IP），再 `jump` 到 ACL 链。
//
// 为什么用 `netdev` 家族的 ingress 而不是 `inet`：真机实测本产品的绑定点是 L3 接口
// （物理口或 VLAN 子接口），`netdev` 的 ingress 挂在设备 tc ingress 点上、按 L2 收到全部帧
// （含 ARP 等非 IP 帧），一次 hook 同时覆盖「本机收包」与「转发收包」；`inet` 家族的 ingress
// 收不到任何帧，input/forward 要分两处且天然看不到非 IP 帧。
//
// 为什么必须显式放行非 IP：jump 是无条件的，若不先放行，ARP/LLDP 等非 IP 帧也会进 ACL 链
// 并撞上链尾 drop——VPP 侧就吃过这个亏（绑上 IP ACL 把域内 ARP 丢了）。实测：先放行非 IP 后，
// 即使 ACL 是「全 drop」，域内 ARP 仍能解析（邻居表 REACHABLE）。
//
// 内核即事实源：读视图（Bound）直接查 nft 规则集，不维护进程内登记。
//
// 规则翻译（真机实测过的语法）：
//
//	source/destination → `ip saddr`/`ip daddr`（v4）或 `ip6 saddr`/`ip6 daddr`（v6）；any/空省略
//	tcp/udp           → `meta l4proto tcp|udp` + `tcp sport`/`tcp dport`（写了端口才加）
//	icmp              → v4 `ip protocol icmp`；v6 `ip6 nexthdr ipv6-icmp`
//	协议未声明但写了端口 → `meta l4proto { tcp, udp }` + `th sport`/`th dport`
//	action            → permit `accept`；deny `drop`

const (
	aclTableFamily = "netdev"
	aclTableName   = "nfvis-acl"
	// aclChainPrefix/aclBindPrefix 两条前缀互不为对方的前缀，且都以字母开头，
	// 故「ACL 链」与「设备绑定链」两类对象不会互相撞名。
	aclChainPrefix = "acl_"
	aclBindPrefix  = "bind_"
)

// aclManager ACL 规则下发与接口绑定。
type aclManager struct{ run Runner }

// newACLManager 以给定 Runner 构造（run 为 nil 时用真实宿主命令）。
func newACLManager(run Runner) *aclManager {
	if run == nil {
		run = NewExecRunner()
	}
	return &aclManager{run: run}
}

// aclChainName 由 ACL 名确定性派生 nft 链名。
//
// nft 标识符只保证字母/数字/下划线可移植（本产品的 ACL 名还允许 `.`/`-`），故把其余字符
// 一律替换为下划线；替换过字符时追加名字派生的 4 位十六进制后缀——否则 `a.b` 与 `a-b` 都会
// 落到同一条链上（撞链）。原始名另以 jump 规则的 comment 记录，读视图据此还原精确名字。
func aclChainName(name string) string {
	s := aclSanitize(name)
	if s != name {
		return aclChainPrefix + s + "_" + aclNameHash(name)
	}
	return aclChainPrefix + s
}

// aclBindChainName 设备绑定链名（每设备一条 ingress 基链）。
func aclBindChainName(dev string) string { return aclBindPrefix + dev }

// aclSanitize 把 ACL 名收敛到 `[A-Za-z0-9_]`。
func aclSanitize(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// aclNameHash ACL 名的 FNV-1a 低 16 位（撞名后缀）。名字本身已由提交期校验唯一，
// 此处只用于「做过字符替换」的名字之间避免撞链。
func aclNameHash(name string) string {
	sum := fnv.New32a()
	_, _ = sum.Write([]byte(name))
	return fmt.Sprintf("%04x", sum.Sum32()&0xffff)
}

// aclSortedRules 返回按 Seq 升序排列的规则副本（链内顺序即匹配顺序，Seq 小者先匹配）。
func aclSortedRules(rules []model.AclRule) []model.AclRule {
	out := append([]model.AclRule(nil), rules...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// Apply 收敛一条 ACL 的规则：先清空本链再按 Seq 顺序重建，链尾补无条件 drop（幂等）。
//
// 同名重放/规则变化都从干净态重建；已绑定的接口仍 jump 到同一条链，规则随之生效。
func (m *aclManager) Apply(ctx context.Context, acl model.Acl) error {
	if acl.Name == "" {
		return fmt.Errorf("ACL 名不能为空")
	}
	if err := m.aclEnsureTable(ctx); err != nil {
		return err
	}
	chain := aclChainName(acl.Name)
	if err := nftIdemOn(ctx, m.run, "add", "chain", aclTableFamily, aclTableName, chain); err != nil {
		return err
	}
	if err := nftReqOn(ctx, m.run, "flush", "chain", aclTableFamily, aclTableName, chain); err != nil {
		return err
	}
	for _, r := range aclSortedRules(acl.Rules) {
		args, err := aclRuleArgs(r)
		if err != nil {
			return fmt.Errorf("ACL %s 规则 %d: %w", acl.Name, r.Seq, err)
		}
		full := append([]string{"add", "rule", aclTableFamily, aclTableName, chain}, args...)
		if err := nftReqOn(ctx, m.run, full...); err != nil {
			return err
		}
	}
	// 链尾无条件 drop：VPP 的 ACL 在规则用尽后隐式拒绝，内核侧用显式兜底 drop 对齐。
	return nftReqOn(ctx, m.run, "add", "rule", aclTableFamily, aclTableName, chain, "drop")
}

// Delete 删除 ACL 链；仍被接口绑定时**拒绝**（不静默留下悬空 jump）。
func (m *aclManager) Delete(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("ACL 名不能为空")
	}
	chain := aclChainName(name)
	devs, err := m.aclBoundDevices(ctx, chain)
	if err != nil {
		return err
	}
	if len(devs) > 0 {
		return fmt.Errorf("ACL %s 仍绑定在接口 %s 上：请先解绑再删除，否则会留下指向已删链的 jump 规则",
			name, strings.Join(devs, "、"))
	}
	if err := deleteChain(ctx, m.run, aclTableFamily, aclTableName, chain); err != nil {
		return err
	}
	return m.aclDropTableIfEmpty(ctx)
}

// Bind 把 ACL 绑定到设备：为该设备建 ingress 基链，链内先放行非 IP、再 jump 到 ACL 链。
//
// 幂等：先移除该设备的既有绑定再重建（重绑到另一条 ACL 也走这条路径）。
func (m *aclManager) Bind(ctx context.Context, dev string, acl model.Acl) error {
	if dev == "" {
		return fmt.Errorf("绑定接口名不能为空")
	}
	if acl.Name == "" {
		return fmt.Errorf("ACL 名不能为空")
	}
	if err := m.aclEnsureTable(ctx); err != nil {
		return err
	}
	chain := aclChainName(acl.Name)
	if err := m.aclRequireChain(ctx, chain); err != nil {
		return fmt.Errorf("ACL %s 尚未下发（应先下发 ACL 再绑定接口）: %w", acl.Name, err)
	}
	if err := m.Unbind(ctx, dev); err != nil {
		return err
	}
	bindChain := aclBindChainName(dev)
	spec := fmt.Sprintf("type filter hook ingress device %s priority filter ;", dev)
	if err := ensureTableChain(ctx, m.run, aclTableFamily, aclTableName, bindChain, spec); err != nil {
		return err
	}
	// 先放行非 IP（ARP/LLDP…）：jump 无条件，不先放行就会让非 IP 帧进 ACL 链并撞链尾 drop。
	if err := nftReqOn(ctx, m.run, "add", "rule", aclTableFamily, aclTableName, bindChain,
		"meta", "protocol", "!=", "{", "ip,", "ip6", "}", "accept"); err != nil {
		return err
	}
	return nftReqOn(ctx, m.run, "add", "rule", aclTableFamily, aclTableName, bindChain,
		"jump", chain, "comment", fmt.Sprintf("%q", acl.Name))
}

// Unbind 撤除设备的 ACL 绑定（幂等：本就不在按已达成）。
//
// 每设备一条独立绑定链，故只动本设备的链、不扰其它设备的绑定。链内规则逐条按 handle 删除
// （`nft -a list chain` 解析 `# handle N`），再回收空链。
func (m *aclManager) Unbind(ctx context.Context, dev string) error {
	if dev == "" {
		return fmt.Errorf("绑定接口名不能为空")
	}
	chain := aclBindChainName(dev)
	if err := m.aclDeleteRules(ctx, chain); err != nil {
		return err
	}
	if err := deleteChain(ctx, m.run, aclTableFamily, aclTableName, chain); err != nil {
		return err
	}
	return m.aclDropTableIfEmpty(ctx)
}

// Bound 读设备当前绑定的 ACL 名（内核即事实源，不查进程内登记）。
//
// 链不存在/无 jump 规则 = 未绑定；读不到（命令失败）同样按未绑定返回。
func (m *aclManager) Bound(ctx context.Context, dev string) (string, bool) {
	if dev == "" {
		return "", false
	}
	out, err := nftRunOn(ctx, m.run, "list", "chain", aclTableFamily, aclTableName, aclBindChainName(dev))
	if err != nil {
		return "", false
	}
	name := aclParseBound(out)
	if name == "" {
		return "", false
	}
	return name, true
}

// ---------- 内部辅助 ----------

// aclEnsureTable 确保本产品的 ACL 表存在（幂等）。
func (m *aclManager) aclEnsureTable(ctx context.Context) error {
	return nftIdemOn(ctx, m.run, "add", "table", aclTableFamily, aclTableName)
}

// aclRequireChain 要求链已存在（不存在即报错，用于暴露 apply 顺序错误）。
func (m *aclManager) aclRequireChain(ctx context.Context, chain string) error {
	out, err := nftRunOn(ctx, m.run, "list", "chain", aclTableFamily, aclTableName, chain)
	if err != nil {
		if notFound(out, err) {
			return fmt.Errorf("链 %s 不存在", chain)
		}
		return fmt.Errorf("nft list chain %s %s %s: %w（%s）", aclTableFamily, aclTableName, chain, err, trimOut(out))
	}
	return nil
}

// aclDropTableIfEmpty 表里没有链时回收整张表（否则会在机器上长期留一张空表）。
func (m *aclManager) aclDropTableIfEmpty(ctx context.Context) error {
	out, err := nftRunOn(ctx, m.run, "list", "table", aclTableFamily, aclTableName)
	if err != nil {
		if notFound(out, err) {
			return nil
		}
		return fmt.Errorf("nft list table %s %s: %w（%s）", aclTableFamily, aclTableName, err, trimOut(out))
	}
	if strings.Contains(out, "chain ") {
		return nil // 还有链在用这张表
	}
	return nftBestOn(ctx, m.run, "delete", "table", aclTableFamily, aclTableName)
}

// aclBoundDevices 列出当前 jump 到该 ACL 链的设备（内核即事实源）。
func (m *aclManager) aclBoundDevices(ctx context.Context, chain string) ([]string, error) {
	out, err := nftRunOn(ctx, m.run, "list", "table", aclTableFamily, aclTableName)
	if err != nil {
		if notFound(out, err) {
			return nil, nil
		}
		return nil, fmt.Errorf("nft list table %s %s: %w（%s）", aclTableFamily, aclTableName, err, trimOut(out))
	}
	var devs []string
	cur := ""
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "chain ") {
			if f := strings.Fields(t); len(f) >= 2 {
				cur = f[1]
			}
			continue
		}
		if !strings.HasPrefix(cur, aclBindPrefix) {
			continue
		}
		f := strings.Fields(t)
		if len(f) >= 2 && f[0] == "jump" && f[1] == chain {
			devs = append(devs, strings.TrimPrefix(cur, aclBindPrefix))
		}
	}
	sort.Strings(devs)
	return devs, nil
}

// aclDeleteRules 逐条按 handle 删掉链里的规则（只删本链，不触碰其它链）。
// 链不存在按已达成；`-a` 让 nft 在每条规则末尾打印 `# handle N`。
func (m *aclManager) aclDeleteRules(ctx context.Context, chain string) error {
	out, err := nftRunOn(ctx, m.run, "-a", "list", "chain", aclTableFamily, aclTableName, chain)
	if err != nil {
		if notFound(out, err) {
			return nil
		}
		return fmt.Errorf("nft -a list chain %s %s %s: %w（%s）", aclTableFamily, aclTableName, chain, err, trimOut(out))
	}
	for _, h := range aclRuleHandles(out) {
		if err := nftReqOn(ctx, m.run, "delete", "rule", aclTableFamily, aclTableName, chain, "handle", h); err != nil {
			return err
		}
	}
	return nil
}

// aclParseBound 从设备绑定链的输出里取回 ACL 名：优先读 jump 规则的 comment（精确原名），
// 无 comment 时回退为去掉链名前缀（手工规则/老现场）。
func aclParseBound(out string) string {
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "jump ") {
			continue
		}
		f := strings.Fields(t)
		if len(f) < 2 {
			continue
		}
		if name := aclCommentValue(t); name != "" {
			return name
		}
		return strings.TrimPrefix(f[1], aclChainPrefix)
	}
	return ""
}

// aclCommentValue 取 nft 规则行里 comment "…" 的值（无则空串）。
func aclCommentValue(line string) string {
	const marker = `comment "`
	i := strings.Index(line, marker)
	if i < 0 {
		return ""
	}
	rest := line[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// aclRuleHandles 从 `nft -a list chain` 输出里收集**规则**的 handle（跳过链/表声明行）。
func aclRuleHandles(out string) []string {
	var hs []string
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "chain ") || strings.HasPrefix(t, "table ") {
			continue
		}
		i := strings.LastIndex(t, "# handle ")
		if i < 0 {
			continue
		}
		rest := strings.TrimSpace(t[i+len("# handle "):])
		end := 0
		for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
			end++
		}
		if end > 0 {
			hs = append(hs, rest[:end])
		}
	}
	return hs
}

// aclRuleArgs 把一条规则翻译成 nft 参数（不含 add rule 前缀）。
//
// 地址族：任一侧为显式 v6 即整条按 v6 下发，any/空一侧跟随显式侧（一条规则只匹配单族，
// 两侧都显式且不同族由提交期校验拒绝，此处返回同文案错误作纵深防御）。
// Direction 与 VPP 实现口径一致，不参与翻译（ACL 整体作用于绑定接口的方向）。
func aclRuleArgs(r model.AclRule) ([]string, error) {
	srcV6, dstV6 := aclIsExplicitV6(r.Source), aclIsExplicitV6(r.Destination)
	if srcV6 != dstV6 && !aclIsAnyAddr(r.Source) && !aclIsAnyAddr(r.Destination) {
		return nil, fmt.Errorf("source/destination 地址族不一致（%s 与 %s）：一条规则仅匹配单族，双族需两条规则",
			r.Source, r.Destination)
	}
	v6 := srcV6 || dstV6
	var args []string
	if !aclIsAnyAddr(r.Source) {
		args = append(args, aclAddrArgs("saddr", r.Source, v6)...)
	}
	if !aclIsAnyAddr(r.Destination) {
		args = append(args, aclAddrArgs("daddr", r.Destination, v6)...)
	}
	switch strings.ToLower(strings.TrimSpace(r.Protocol)) {
	case "tcp", "udp":
		proto := strings.ToLower(strings.TrimSpace(r.Protocol))
		args = append(args, "meta", "l4proto", proto)
		args = append(args, aclPortArgs(proto, "sport", r.SourcePort)...)
		args = append(args, aclPortArgs(proto, "dport", r.DestinationPort)...)
	case "icmp":
		if v6 {
			args = append(args, "ip6", "nexthdr", "ipv6-icmp")
		} else {
			args = append(args, "ip", "protocol", "icmp")
		}
	default: // ""/"any"：协议不限；写了端口则限定 tcp/udp 后按传输头匹配
		if r.SourcePort != "" || r.DestinationPort != "" {
			args = append(args, "meta", "l4proto", "{", "tcp,", "udp", "}")
			args = append(args, aclPortArgs("th", "sport", r.SourcePort)...)
			args = append(args, aclPortArgs("th", "dport", r.DestinationPort)...)
		}
	}
	switch strings.ToLower(strings.TrimSpace(r.Action)) {
	case "permit":
		args = append(args, "accept")
	case "deny":
		args = append(args, "drop")
	default:
		return nil, fmt.Errorf("action 必须为 permit 或 deny，实际 %q", r.Action)
	}
	return args, nil
}

// aclAddrArgs 地址匹配（`ip saddr <prefix>` / `ip6 daddr <prefix>`）。
func aclAddrArgs(kind, prefix string, v6 bool) []string {
	fam := "ip"
	if v6 {
		fam = "ip6"
	}
	return []string{fam, kind, strings.TrimSpace(prefix)}
}

// aclPortArgs 端口匹配（`tcp sport 80` / `th dport 1024-65535`）；空/any 表示不匹配。
// proto 为 "th" 时按传输头匹配（协议未声明但写了端口的情形，已限定 tcp/udp）。
func aclPortArgs(proto, kind, port string) []string {
	p := strings.TrimSpace(port)
	if p == "" || strings.EqualFold(p, "any") {
		return nil
	}
	return []string{proto, kind, p}
}

// aclIsAnyAddr 规则地址字段是否为「any/空」。
func aclIsAnyAddr(s string) bool {
	s = strings.TrimSpace(s)
	return s == "" || strings.EqualFold(s, "any")
}

// aclIsExplicitV6 判定地址字段是否为显式 IPv6（any/空与解析失败的值都不算）。
func aclIsExplicitV6(s string) bool {
	if aclIsAnyAddr(s) {
		return false
	}
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() == nil
}
