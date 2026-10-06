package system

// 管理面主机防火墙的落地与回读（FR-NET-002/FR-SEC-001，决策 #388）。
//
// 形态：把 committed 的 `system.firewall` 渲染成宿主 nftables 的一张**独立表**
// `table inet nfvis-firewall`，用 `nft -f -` 下发；只作用于**管理口入向**。
// 三条硬边界（真机 spike 结论，round168）：
//  1. 只建/只动自家这张表——系统上既有六张表（filter/nat/mangle × ip/ip6）全由
//     iptables-nft（docker/libvirt）管理，任何 `flush ruleset` 类动作都会把它们一起清掉；
//  2. 同 hook 的 base chain 相互独立、drop 全局生效（高优先 accept 不能豁免别处 drop），
//     故保留项必须写在本表里（非管理口 accept / ct established,related / 必要 ICMP 与 ND）；
//  3. 逐规则计数（`counter`）真实递增可读，且 `nft -j list table` 能结构化回读。
//
// 幂等与原子：指纹文件（sha256(脚本)）+ `nft list tables` 的实况共同决定是否重建；重建时把
// 「delete table + 新表定义」放进**同一个** `nft -f -` 批里（nft 批内原子），不产生无过滤窗口。
// 命令只准出现 `table inet nfvis-firewall`，绝不触碰其它表。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

// FirewallTable 本产品在宿主 nftables 上的独立表名（命令中只准出现它）。
const FirewallTable = "nfvis-firewall"

// firewallTableSpec 表标识（`inet` 族 + 本产品表名）。
const firewallTableSpec = "inet " + FirewallTable

// FirewallFingerprintPath 已下发脚本的指纹文件（幂等判据；/run 为 tmpfs，重启自然失效）。
const FirewallFingerprintPath = "/run/nfvis/firewall.fingerprint"

// FirewallCountersNote 计数口径说明（CLI/REST/Web 三面同源）。
const FirewallCountersNote = "计数由数据面（nftables）统计，自本表最近一次下发起；防火墙配置变更会重建表并清零"

// FirewallReserved 保留项说明（用户规则**不可覆盖**；三面同源）。
var FirewallReserved = []string{
	"非管理口入向流量不参与本防火墙",
	"已建立/相关连接（含当前会话）",
	"必要 ICMP/ICMPv6（v4 差错；v6 差错与邻居发现）",
}

// FirewallCounter 一条规则在数据面的计数读数。
type FirewallCounter struct {
	Packets uint64
	Bytes   uint64
}

// FirewallState 数据面对照读数（读视图用；全部为实测，读不到就 applied=false + 原因）。
type FirewallState struct {
	Applied  bool
	Error    string
	Counters map[int]FirewallCounter // seq → 计数（仅可读的规则入场）
}

// RenderFirewallScript 把 committed 配置渲染成 nftables 脚本（纯函数，便于单测钉住字面）。
//
// 返回 enabled=false 表示配置全空（无规则且默认策略未设/accept）——「未配置＝无过滤」，
// 调用方据此回收表。启用但未声明管理口时返回错误（规则没有作用对象，不生成半套脚本）。
//
// 输出确定性：保留项固定顺序、用户规则按 seq 升序——同配置两次渲染逐字相同（幂等指纹的前提）。
func RenderFirewallScript(cfg model.Config) (string, bool, error) {
	fw := cfg.FirewallOf()
	if !fw.FirewallEnabled() {
		return "", false, nil
	}
	mgmt := cfg.MgmtInterfaceOf()
	if mgmt == "" {
		return "", true, fmt.Errorf("未声明管理口：主机防火墙作用于管理口入向，请先 set system management interface <ifname>")
	}
	rules := append([]model.FirewallRule(nil), fw.Rules...)
	sort.Slice(rules, func(i, j int) bool { return rules[i].Seq < rules[j].Seq })

	var b strings.Builder
	fmt.Fprintf(&b, "table %s {\n", firewallTableSpec)
	b.WriteString("\tchain input {\n")
	fmt.Fprintf(&b, "\t\ttype filter hook input priority filter; policy %s;\n", fw.FirewallPolicy())
	// 非管理口入向不参与（iifname 反匹配）；随后是与数据面冲突无关的三条保留项。
	fmt.Fprintf(&b, "\t\tiifname != %q accept\n", mgmt)
	b.WriteString("\t\tct state established,related accept\n")
	b.WriteString("\t\ticmp type { destination-unreachable, time-exceeded } accept\n")
	b.WriteString("\t\ticmpv6 type { destination-unreachable, packet-too-big, time-exceeded, parameter-problem, nd-router-solicit, nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert } accept\n")
	for _, r := range rules {
		fmt.Fprintf(&b, "\t\t%s counter %s comment \"%s%d\"\n", firewallMatch(r), r.Action, firewallCommentPrefix, r.Seq)
	}
	b.WriteString("\t}\n}\n")
	return b.String(), true, nil
}

// firewallCommentPrefix 规则 comment 前缀：身份标记（`nfvis-rule-<seq>`），回读据此映射计数
// 与识别「非本产品规则」。
const firewallCommentPrefix = "nfvis-rule-"

// firewallMatch 渲染一条规则的匹配部分（动作/计数由调用方拼接）。
//
// 口径（与提交校验同源）：
//   - source：v4→`ip saddr`、v6→`ip6 saddr`；裸 IP 在渲染时按 /32、/128 归一（校验已放行裸 IP）；
//   - protocol：tcp 有 port→`tcp dport <n>`、无 port→`meta l4proto tcp`（udp 同理）；
//     icmp 随 source 族取 `meta l4proto icmp`/`ipv6-icmp`，无 source 时二者皆匹配；
//   - any/未设协议：不渲染协议匹配（只按 source 收窄）。
func firewallMatch(r model.FirewallRule) string {
	var parts []string
	fam := ""
	if r.Source != "" {
		fam = firewallSourceFamily(r.Source)
		src := normalizeFirewallSource(r.Source, fam)
		if fam == "ipv6" {
			parts = append(parts, "ip6 saddr "+src)
		} else {
			parts = append(parts, "ip saddr "+src)
		}
	}
	switch r.Protocol {
	case "tcp":
		parts = append(parts, transportMatch("tcp", r.Port))
	case "udp":
		parts = append(parts, transportMatch("udp", r.Port))
	case "icmp":
		switch fam {
		case "ipv4":
			parts = append(parts, "meta l4proto icmp")
		case "ipv6":
			parts = append(parts, "meta l4proto ipv6-icmp")
		default:
			parts = append(parts, "meta l4proto { icmp, ipv6-icmp }")
		}
	}
	return strings.Join(parts, " ")
}

func transportMatch(proto string, port int) string {
	if port > 0 {
		return fmt.Sprintf("%s dport %d", proto, port)
	}
	return "meta l4proto " + proto
}

// firewallSourceFamily 来源地址族（"ipv4"/"ipv6"；解析不了返回 ""，由提交校验拦下，不在这里猜）。
func firewallSourceFamily(s string) string {
	if p, err := netip.ParsePrefix(s); err == nil {
		if p.Addr().Is6() {
			return "ipv6"
		}
		return "ipv4"
	}
	if a, err := netip.ParseAddr(s); err == nil {
		if a.Is6() {
			return "ipv6"
		}
		return "ipv4"
	}
	return ""
}

// normalizeFirewallSource 前缀原样、裸 IP 补 /32 或 /128（nft 对二者语义相同，显式前缀更可读）。
func normalizeFirewallSource(s, fam string) string {
	if _, err := netip.ParsePrefix(s); err == nil {
		return s
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return s
	}
	if a.Is6() {
		return a.String() + "/128"
	}
	return a.String() + "/32"
}

// FirewallApplier 主机防火墙的下发与回读（决策 #388）。行为可在单测注入（Run/IfaceExists）。
type FirewallApplier struct {
	// Run 执行一条 nft 命令；stdin 非空时喂给标准输入（`nft -f -` 的脚本）。
	Run func(ctx context.Context, stdin string, args ...string) (string, error)
	// FingerprintPath 指纹文件路径（空 = FirewallFingerprintPath）。
	FingerprintPath string
	// IfaceExists 管理口存在性判据（空 = net.InterfaceByName）。**安全前置**：管理口名写错时
	// 下发会把真实管理口置于 drop 之下，存在性核不过即拒绝下发（不静默）。
	IfaceExists func(name string) bool
}

// NewFirewallApplier 构造真实系统上的落地器。
func NewFirewallApplier() *FirewallApplier { return &FirewallApplier{} }

func (a *FirewallApplier) run(ctx context.Context, stdin string, args ...string) (string, error) {
	if a.Run != nil {
		return a.Run(ctx, stdin, args...)
	}
	return defaultFirewallExec(ctx, stdin, args...)
}

func (a *FirewallApplier) fingerprintPath() string {
	if a.FingerprintPath != "" {
		return a.FingerprintPath
	}
	return FirewallFingerprintPath
}

func (a *FirewallApplier) ifaceExists(name string) bool {
	if a.IfaceExists != nil {
		return a.IfaceExists(name)
	}
	_, err := net.InterfaceByName(name)
	return err == nil
}

// fingerprint 脚本指纹（sha256 十六进制）。
func fingerprint(script string) string {
	sum := sha256.Sum256([]byte(script))
	return hex.EncodeToString(sum[:])
}

func (a *FirewallApplier) readFingerprint() string {
	b, err := os.ReadFile(a.fingerprintPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (a *FirewallApplier) writeFingerprint(fp string) error {
	p := a.fingerprintPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("建状态目录 %s: %w", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, []byte(fp+"\n"), 0o644); err != nil {
		return fmt.Errorf("写指纹 %s: %w", p, err)
	}
	return nil
}

func (a *FirewallApplier) clearFingerprint() {
	_ = os.Remove(a.fingerprintPath())
}

// tablePresent 数据面是否存在本产品的表（`nft list tables` 实况）。
func (a *FirewallApplier) tablePresent(ctx context.Context) (bool, error) {
	out, err := a.run(ctx, "", "list", "tables")
	if err != nil {
		return false, fmt.Errorf("执行 nft list tables: %w（%s）", err, strings.TrimSpace(out))
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "table "+firewallTableSpec {
			return true, nil
		}
	}
	return false, nil
}

// Apply 把主机防火墙收敛到 committed 配置（幂等）。
//
// 未启用（配置全空）：无表则跳过、有表则删表并清指纹。
// 已启用：管理口不存在即拒绝（安全前置）；指纹一致且表在 ⇒ 跳过（计数连续）；否则
// 「（表在时）delete table + 新表定义」同批 `nft -f -` 原子重建，成功后写指纹。
//
// 失败一律返回错误（不吞）：调用方（启动/提交回调）只记日志、不阻塞，读视图以 applied=false
// 如实呈现——宿主防火墙下发失败不得让 nfvisd 起不来。
func (a *FirewallApplier) Apply(ctx context.Context, cfg model.Config) error {
	script, enabled, err := RenderFirewallScript(cfg)
	if err != nil {
		return err
	}
	present, err := a.tablePresent(ctx)
	if err != nil {
		return err
	}
	if !enabled {
		if !present {
			a.clearFingerprint() // 无表：清掉可能与实况不符的陈旧指纹
			return nil
		}
		if _, err := a.run(ctx, "", "delete", "table", firewallTableSpec); err != nil {
			return fmt.Errorf("回收 %s 表: %w", firewallTableSpec, err)
		}
		a.clearFingerprint()
		return nil
	}
	mgmt := cfg.MgmtInterfaceOf()
	if !a.ifaceExists(mgmt) {
		return fmt.Errorf("管理口 %s 不存在：下发会把真实管理口置于过滤之下，已拒绝（请核对 set system management interface 的取值）", mgmt)
	}
	fp := fingerprint(script)
	if present && a.readFingerprint() == fp {
		return nil // 幂等：脚本未变且表在 ⇒ 不重建（计数因此连续）
	}
	batch := script
	if present {
		// 批内先删后建 ⇒ 单一 nft 事务，无「表已删、新表未建」的无过滤窗口。
		batch = "delete table " + firewallTableSpec + "\n" + script
	}
	if out, err := a.run(ctx, batch, "-f", "-"); err != nil {
		return fmt.Errorf("下发 %s 表: %w（%s）", firewallTableSpec, err, strings.TrimSpace(out))
	}
	return a.writeFingerprint(fp)
}

// Read 回读数据面并与配置对照（读视图用；**任何不可读都不得报成 0**）。
//
// applied 的判据：未配置 ⇒ 无表即一致、有表即不一致；已配置 ⇒ 表在且 input 链策略/规则
// 集合（comment 序列与保留项条数）与配置一致。表缺失、nft 不可用/权限不足、检测到手工修改
// 等一律 applied=false + 原因（不把「读不出来」答成「没有」）。
func (a *FirewallApplier) Read(ctx context.Context, cfg model.Config) FirewallState {
	_, enabled, rerr := RenderFirewallScript(cfg)
	if rerr != nil {
		return FirewallState{Applied: false, Error: rerr.Error()}
	}
	out, err := a.run(ctx, "", "-j", "list", "table", firewallTableSpec)
	if err != nil {
		if firewallTableMissing(out, err) {
			if !enabled {
				return FirewallState{Applied: true}
			}
			return FirewallState{Applied: false,
				Error: "表不存在（配置已启用但数据面没有 " + firewallTableSpec + " 表：尚未下发或已被手工删除）"}
		}
		return FirewallState{Applied: false, Error: "读取 nftables 表失败：" + strings.TrimSpace(err.Error())}
	}
	tbl, perr := parseFirewallTableJSON(out)
	if perr != nil {
		return FirewallState{Applied: false, Error: "解析 nftables 回读失败：" + perr.Error()}
	}
	if !enabled {
		return FirewallState{Applied: false,
			Error: "配置未启用主机防火墙但数据面仍有 " + firewallTableSpec + " 表（等待下发回收，或该表由手工创建）"}
	}
	st := FirewallState{Applied: true, Counters: tbl.counters()}
	if err := compareFirewallTable(cfg, tbl); err != nil {
		st.Applied = false
		st.Error = err.Error()
	}
	return st
}

// firewallTable 回读到的表实况（parseFirewallTableJSON 的产物）。
type firewallTable struct {
	HasTable    bool
	HasInput    bool
	ChainPolicy string
	Rules       []firewallTableRule
}

type firewallTableRule struct {
	Comment    string
	Counter    FirewallCounter
	HasCounter bool
}

func (t *firewallTable) counters() map[int]FirewallCounter {
	out := map[int]FirewallCounter{}
	for _, r := range t.Rules {
		if !r.HasCounter {
			continue
		}
		if seq, ok := firewallCommentSeq(r.Comment); ok {
			out[seq] = r.Counter
		}
	}
	return out
}

// nftJSONItem 一条 `nft -j` 顶层项：对象恰好一个键（table/chain/rule/metainfo/…）。
type nftJSONItem map[string]json.RawMessage

type nftTableJSON struct {
	Family string `json:"family"`
	Name   string `json:"name"`
}

type nftChainJSON struct {
	Family string `json:"family"`
	Table  string `json:"table"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Hook   string `json:"hook"`
	Policy string `json:"policy"`
}

type nftRuleJSON struct {
	Family  string                       `json:"family"`
	Table   string                       `json:"table"`
	Chain   string                       `json:"chain"`
	Comment string                       `json:"comment"`
	Expr    []map[string]json.RawMessage `json:"expr"`
}

type nftCounterJSON struct {
	Packets uint64 `json:"packets"`
	Bytes   uint64 `json:"bytes"`
}

// parseFirewallTableJSON 解析 `nft -j list table inet nfvis-firewall` 的输出
// （真机形状：`{"nftables":[{"metainfo":…},{"table":…},{"chain":…},{"rule":…},…]}`）。
func parseFirewallTableJSON(raw string) (*firewallTable, error) {
	var doc struct {
		Nftables []nftJSONItem `json:"nftables"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, err
	}
	t := &firewallTable{}
	for _, item := range doc.Nftables {
		if body, ok := item["table"]; ok {
			var tb nftTableJSON
			if err := json.Unmarshal(body, &tb); err == nil && tb.Family == "inet" && tb.Name == FirewallTable {
				t.HasTable = true
			}
		}
		if body, ok := item["chain"]; ok {
			var ch nftChainJSON
			if err := json.Unmarshal(body, &ch); err == nil && ch.Table == FirewallTable && ch.Name == "input" {
				t.HasInput = ch.Type == "filter" && ch.Hook == "input"
				t.ChainPolicy = ch.Policy
			}
		}
		if body, ok := item["rule"]; ok {
			var rl nftRuleJSON
			if err := json.Unmarshal(body, &rl); err != nil || rl.Table != FirewallTable {
				continue
			}
			r := firewallTableRule{Comment: rl.Comment}
			for _, e := range rl.Expr {
				rawCounter, ok := e["counter"]
				if !ok {
					continue
				}
				var c nftCounterJSON
				if err := json.Unmarshal(rawCounter, &c); err != nil {
					continue
				}
				r.Counter, r.HasCounter = FirewallCounter{Packets: c.Packets, Bytes: c.Bytes}, true
				break
			}
			t.Rules = append(t.Rules, r)
		}
	}
	return t, nil
}

// firewallCommentSeq 解析身份 comment（`nfvis-rule-<seq>`）。
func firewallCommentSeq(comment string) (int, bool) {
	if !strings.HasPrefix(comment, firewallCommentPrefix) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(comment, firewallCommentPrefix))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// compareFirewallTable 配置 ⇄ 数据面一致性核对（启用态；不一致返回可照读的原因）。
func compareFirewallTable(cfg model.Config, tbl *firewallTable) error {
	fw := cfg.FirewallOf()
	if !tbl.HasTable {
		return fmt.Errorf("表不存在（配置已启用但数据面没有 %s 表）", firewallTableSpec)
	}
	if !tbl.HasInput {
		return fmt.Errorf("未找到 input 链（hook input / type filter）；表可能已被手工修改")
	}
	if want := fw.FirewallPolicy(); tbl.ChainPolicy != want {
		return fmt.Errorf("默认策略不一致：配置 %s、数据面 %s", want, tbl.ChainPolicy)
	}
	// 规则集合：保留项（无 comment）恰 4 条；带 comment 的必须全是本产品的 `nfvis-rule-<seq>`
	// 且按序与配置的 seq 逐一相等——多/少/改序/外来 comment 都是「与配置不一致」。
	wantSeqs := make([]int, 0, len(fw.Rules))
	for _, r := range fw.Rules {
		wantSeqs = append(wantSeqs, r.Seq)
	}
	sort.Ints(wantSeqs)
	var reserved int
	var gotSeqs []int
	for _, r := range tbl.Rules {
		if r.Comment == "" {
			reserved++
			continue
		}
		seq, ok := firewallCommentSeq(r.Comment)
		if !ok {
			return fmt.Errorf("检测到手工修改：%s 表含非本产品规则（comment=%q）；产品不会覆盖它，请先手工清理", firewallTableSpec, r.Comment)
		}
		gotSeqs = append(gotSeqs, seq)
	}
	if reserved != 4 {
		return fmt.Errorf("规则集合与配置不一致：数据面保留项 %d 条、期望 4 条（表可能已被手工修改）", reserved)
	}
	if !slices.Equal(gotSeqs, wantSeqs) {
		return fmt.Errorf("规则集合与配置不一致：数据面 %v、配置 %v", gotSeqs, wantSeqs)
	}
	return nil
}

// firewallTableMissing 判断 `nft list table` 的失败是否只是「表不存在」（与「nft 不可用/
// 权限不足」区分开：后者必须如实报读不出来，不能答成「未下发」）。
func firewallTableMissing(out string, err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(out + " " + err.Error())
	return strings.Contains(s, "no such file or directory") || strings.Contains(s, "does not exist")
}
