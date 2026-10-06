package system

// 决策 #388：管理面主机防火墙的渲染（纯函数字面）、下发幂等/原子与回读判定的单测。
// nft 执行与网卡存在性全注入——不在宿主上跑真的 nft。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

func fwCfg(rules []model.FirewallRule, policy string) model.Config {
	return model.Config{System: &model.SystemConfig{
		Management: &model.MgmtConfig{Interface: "ens160"},
		Firewall:   &model.FirewallConfig{DefaultPolicy: policy, Rules: rules},
	}}
}

func TestRenderFirewallScriptDisabled(t *testing.T) {
	for _, c := range []model.Config{
		{},
		{System: &model.SystemConfig{}},
		{System: &model.SystemConfig{Firewall: &model.FirewallConfig{}}},
		{System: &model.SystemConfig{Firewall: &model.FirewallConfig{DefaultPolicy: "accept"}}},
	} {
		script, enabled, err := RenderFirewallScript(c)
		if err != nil || enabled || script != "" {
			t.Fatalf("未启用的配置应渲染为空（enabled=false）: %q %v %v", script, enabled, err)
		}
	}
}

func TestRenderFirewallScriptLiteral(t *testing.T) {
	cfg := fwCfg([]model.FirewallRule{
		// 故意乱序：渲染必须按 seq 升序（首命中口径 + 输出确定性）
		{Seq: 400, Action: "accept", Source: "10.1.2.3"}, // 裸 IP → /32 归一
		{Seq: 100, Action: "drop", Source: "2001:db8::/32", Protocol: "icmp"},
		{Seq: 300, Action: "accept", Protocol: "udp"}, // 无 source、无 port
		{Seq: 200, Action: "accept", Source: "192.168.1.0/24", Protocol: "tcp", Port: 22},
	}, "drop")
	script, enabled, err := RenderFirewallScript(cfg)
	if err != nil || !enabled {
		t.Fatalf("应渲染启用脚本: %v %v", enabled, err)
	}
	want := "table inet nfvis-firewall {\n" +
		"\tchain input {\n" +
		"\t\ttype filter hook input priority filter; policy drop;\n" +
		"\t\tiifname != \"ens160\" accept\n" +
		"\t\tct state established,related accept\n" +
		"\t\ticmp type { destination-unreachable, time-exceeded } accept\n" +
		"\t\ticmpv6 type { destination-unreachable, packet-too-big, time-exceeded, parameter-problem, nd-router-solicit, nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert } accept\n" +
		"\t\tip6 saddr 2001:db8::/32 meta l4proto ipv6-icmp counter drop comment \"nfvis-rule-100\"\n" +
		"\t\tip saddr 192.168.1.0/24 tcp dport 22 counter accept comment \"nfvis-rule-200\"\n" +
		"\t\tmeta l4proto udp counter accept comment \"nfvis-rule-300\"\n" +
		"\t\tip saddr 10.1.2.3/32 counter accept comment \"nfvis-rule-400\"\n" +
		"\t}\n}\n"
	if script != want {
		t.Fatalf("渲染脚本与期望不一致:\n--- got ---\n%s--- want ---\n%s", script, want)
	}
}

// 协议分支逐一钉住：tcp/udp 有 port 与无 port、icmp 按来源族与无来源、any 不渲染协议。
func TestRenderFirewallScriptMatchBranches(t *testing.T) {
	cases := []struct {
		rule model.FirewallRule
		want string
	}{
		{model.FirewallRule{Seq: 1, Action: "accept", Source: "10.0.0.0/8", Protocol: "tcp", Port: 443}, "ip saddr 10.0.0.0/8 tcp dport 443 counter accept"},
		{model.FirewallRule{Seq: 2, Action: "drop", Source: "fd00::/64", Protocol: "tcp"}, "ip6 saddr fd00::/64 meta l4proto tcp counter drop"},
		{model.FirewallRule{Seq: 3, Action: "accept", Protocol: "udp", Port: 53}, "udp dport 53 counter accept"},
		{model.FirewallRule{Seq: 4, Action: "accept", Source: "192.0.2.1", Protocol: "icmp"}, "ip saddr 192.0.2.1/32 meta l4proto icmp counter accept"},
		{model.FirewallRule{Seq: 5, Action: "accept", Source: "2001:db8::1", Protocol: "icmp"}, "ip6 saddr 2001:db8::1/128 meta l4proto ipv6-icmp counter accept"},
		{model.FirewallRule{Seq: 6, Action: "accept", Protocol: "icmp"}, "meta l4proto { icmp, ipv6-icmp } counter accept"},
		{model.FirewallRule{Seq: 7, Action: "accept", Protocol: "any", Port: 0, Source: "198.51.100.0/24"}, "ip saddr 198.51.100.0/24 counter accept"},
	}
	for _, c := range cases {
		script, _, err := RenderFirewallScript(fwCfg([]model.FirewallRule{c.rule}, "drop"))
		if err != nil {
			t.Fatalf("%d: %v", c.rule.Seq, err)
		}
		if !strings.Contains(script, "\t\t"+c.want+" comment \"nfvis-rule-") {
			t.Fatalf("seq %d 的匹配渲染不符:\n%s", c.rule.Seq, script)
		}
	}
}

// 确定性：规则数组声明序不同、两次渲染必须逐字相同（幂等指纹的前提）。
func TestRenderFirewallScriptDeterministic(t *testing.T) {
	a := fwCfg([]model.FirewallRule{{Seq: 10, Action: "accept", Source: "10.0.0.0/8"}, {Seq: 20, Action: "drop", Protocol: "udp"}}, "")
	b := fwCfg([]model.FirewallRule{{Seq: 20, Action: "drop", Protocol: "udp"}, {Seq: 10, Action: "accept", Source: "10.0.0.0/8"}}, "")
	sa, _, _ := RenderFirewallScript(a)
	sb, _, _ := RenderFirewallScript(b)
	if sa != sb || sa == "" {
		t.Fatalf("同配置（仅数组序不同）两次渲染应逐字相同:\n%s\n---\n%s", sa, sb)
	}
	if _, _, err := RenderFirewallScript(fwCfg([]model.FirewallRule{{Seq: 1, Action: "accept", Source: "10.0.0.0/8"}}, "drop")); err != nil {
		t.Fatal(err)
	}
}

// 启用但未声明管理口：渲染直接报错（不生成半套脚本）。
func TestRenderFirewallScriptRequiresMgmt(t *testing.T) {
	cfg := model.Config{System: &model.SystemConfig{
		Firewall: &model.FirewallConfig{DefaultPolicy: "drop"},
	}}
	_, enabled, err := RenderFirewallScript(cfg)
	if !enabled || err == nil || !strings.Contains(err.Error(), "管理口") {
		t.Fatalf("启用但未声明管理口应报错: enabled=%v err=%v", enabled, err)
	}
}

// ---------- 下发（Apply）----------

// fakeNft 记录调用并模拟 nft 输出（list tables / -f - / delete / -j list）。
type fakeNft struct {
	tablesOut string
	readOut   string
	readErr   error
	readErrS  string
	applyErr  error

	calls   []string
	scripts []string
}

func (f *fakeNft) run(ctx context.Context, stdin string, args ...string) (string, error) {
	key := strings.Join(args, " ")
	f.calls = append(f.calls, key)
	switch key {
	case "list tables":
		return f.tablesOut, nil
	case "-f -":
		f.scripts = append(f.scripts, stdin)
		if f.applyErr != nil {
			return "", f.applyErr
		}
		f.tablesOut = "table inet nfvis-firewall\n" // 下发后表在
		return "", nil
	case "delete table inet nfvis-firewall":
		f.tablesOut = ""
		return "", nil
	case "-j list table inet nfvis-firewall":
		return f.readErrS + f.readOut, f.readErr
	}
	return "", errors.New("fakeNft: 意外命令 " + key)
}

func newFwApplier(t *testing.T, f *fakeNft) *FirewallApplier {
	t.Helper()
	return &FirewallApplier{
		Run:             f.run,
		FingerprintPath: filepath.Join(t.TempDir(), "firewall.fingerprint"),
		IfaceExists:     func(name string) bool { return name == "ens160" },
	}
}

func TestApplyDisabledSkipsAndClears(t *testing.T) {
	f := &fakeNft{}
	a := newFwApplier(t, f)
	if err := os.WriteFile(a.FingerprintPath, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(context.Background(), model.Config{}); err != nil {
		t.Fatal(err)
	}
	if len(f.scripts) != 0 || len(f.calls) != 1 || f.calls[0] != "list tables" {
		t.Fatalf("未启用且无表应只查一次 list tables: %v", f.calls)
	}
	if _, err := os.Stat(a.FingerprintPath); !os.IsNotExist(err) {
		t.Fatal("无表时应清掉陈旧指纹")
	}
}

func TestApplyDisabledReclaimsTable(t *testing.T) {
	f := &fakeNft{tablesOut: "table inet nfvis-firewall\n"}
	a := newFwApplier(t, f)
	if err := a.Apply(context.Background(), model.Config{}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 || f.calls[1] != "delete table inet nfvis-firewall" {
		t.Fatalf("未启用但有表应收敛删表: %v", f.calls)
	}
}

func TestApplyEnabledIdempotent(t *testing.T) {
	f := &fakeNft{}
	a := newFwApplier(t, f)
	cfg := fwCfg([]model.FirewallRule{{Seq: 100, Action: "accept", Source: "192.0.2.0/24", Protocol: "tcp", Port: 9999}}, "")
	if err := a.Apply(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(f.scripts) != 1 || !strings.HasPrefix(f.scripts[0], "table inet nfvis-firewall {") {
		t.Fatalf("首次下发应经 -f - 喂整表脚本（无 delete 前缀）: %v", f.scripts)
	}
	if b, err := os.ReadFile(a.FingerprintPath); err != nil || strings.TrimSpace(string(b)) == "" {
		t.Fatalf("应写指纹: %v %q", err, b)
	}
	// 同配置再下发：list tables 说表在 + 指纹一致 ⇒ 跳过（计数连续，零重建）
	f.calls = nil
	if err := a.Apply(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(f.scripts) != 1 || len(f.calls) != 1 {
		t.Fatalf("同配置应幂等跳过: calls=%v scripts=%d", f.calls, len(f.scripts))
	}
	// 配置变更：批内 delete+建表（原子替换），指纹更新
	f.calls = nil
	cfg.System.Firewall.Rules[0].Action = "drop"
	if err := a.Apply(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(f.scripts) != 2 || !strings.HasPrefix(f.scripts[1], "delete table inet nfvis-firewall\n") {
		t.Fatalf("变更应含删除前缀的原子替换批: %v", f.scripts)
	}
}

func TestApplyEnabledRejectsMissingMgmtIface(t *testing.T) {
	f := &fakeNft{}
	a := newFwApplier(t, f)
	a.IfaceExists = func(string) bool { return false }
	cfg := fwCfg([]model.FirewallRule{{Seq: 1, Action: "accept", Source: "10.0.0.0/8"}}, "")
	err := a.Apply(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("管理口不存在应拒绝下发: %v", err)
	}
	if len(f.scripts) != 0 {
		t.Fatalf("拒绝时不得下发: %v", f.scripts)
	}
}

// ---------- 回读（Read）----------

// realShapeJSON 真机形状样例（决策 #388 契约里给出的形态：reserved 无 comment、
// 用户规则带 comment 与 counter）。保留项字面须与渲染脚本一致——决策 #395 起回读按内容比对。
//
// ct state 采用**真机 dev115 实证**的真实形状：`match.left.ct` + 数组右值
// `["established","related"]`（不是 {"set":…}）——旧解析只认后者会假报未收敛。
const realShapeJSON = `{"nftables":[
{"metainfo":{"version":"1.1.6","release_name":"x"}},
{"table":{"family":"inet","name":"nfvis-firewall"}},
{"chain":{"family":"inet","table":"nfvis-firewall","name":"input","type":"filter","hook":"input","prio":0,"policy":"drop"}},
{"rule":{"family":"inet","table":"nfvis-firewall","chain":"input","expr":[{"match":{"op":"!=","left":{"meta":{"key":"iifname"}},"right":"ens160"}},{"accept":null}]}},
{"rule":{"family":"inet","table":"nfvis-firewall","chain":"input","expr":[{"match":{"op":"in","left":{"ct":{"key":"state"}},"right":["established","related"]}},{"accept":null}]}},
{"rule":{"family":"inet","table":"nfvis-firewall","chain":"input","expr":[{"match":{"op":"==","left":{"payload":{"protocol":"icmp","field":"type"}},"right":{"set":["destination-unreachable","time-exceeded"]}}},{"accept":null}]}},
{"rule":{"family":"inet","table":"nfvis-firewall","chain":"input","expr":[{"match":{"op":"==","left":{"payload":{"protocol":"icmpv6","field":"type"}},"right":{"set":["destination-unreachable","packet-too-big","time-exceeded","parameter-problem","nd-router-solicit","nd-router-advert","nd-neighbor-solicit","nd-neighbor-advert"]}}},{"accept":null}]}},
{"rule":{"family":"inet","table":"nfvis-firewall","chain":"input","comment":"nfvis-rule-100","expr":[{"match":{"op":"==","left":{"payload":{"protocol":"tcp","field":"dport"}},"right":22}},{"counter":{"packets":3,"bytes":180}},{"accept":null}]}}
]}`

func TestReadAppliedWithCounters(t *testing.T) {
	f := &fakeNft{readOut: realShapeJSON}
	a := newFwApplier(t, f)
	cfg := fwCfg([]model.FirewallRule{{Seq: 100, Action: "accept", Source: "192.168.1.0/24", Protocol: "tcp", Port: 22}}, "drop")
	st := a.Read(context.Background(), cfg)
	if !st.Applied || st.Error != "" {
		t.Fatalf("形状与配置一致应 applied=true: %+v", st)
	}
	c, ok := st.Counters[100]
	if !ok || c.Packets != 3 || c.Bytes != 180 {
		t.Fatalf("逐规则计数应解析: %+v", st.Counters)
	}
}

// realMachineShapeJSON 真机 dev115 的 `nft -j` 形状（用户规则 `source 192.0.2.0/24`）——
// ct state 右值是**数组**、icmp/icmpv6 是 `{"set":…}`、iifname 是字符串、saddr 是 `{"prefix":…}`。
// 这是「真机形状」守护：旧解析只认 ct state 的 {"set":…}/{"match":…}，数组形态抽成空串 ⇒ 假未收敛。
const realMachineShapeJSON = `{"nftables":[
{"metainfo":{"version":"1.1.6","release_name":"x"}},
{"table":{"family":"inet","name":"nfvis-firewall"}},
{"chain":{"family":"inet","table":"nfvis-firewall","name":"input","type":"filter","hook":"input","prio":0,"policy":"accept"}},
{"rule":{"family":"inet","table":"nfvis-firewall","chain":"input","expr":[{"match":{"op":"!=","left":{"meta":{"key":"iifname"}},"right":"ens160"}},{"accept":null}]}},
{"rule":{"family":"inet","table":"nfvis-firewall","chain":"input","expr":[{"match":{"op":"in","left":{"ct":{"key":"state"}},"right":["established","related"]}},{"accept":null}]}},
{"rule":{"family":"inet","table":"nfvis-firewall","chain":"input","expr":[{"match":{"op":"==","left":{"payload":{"protocol":"icmp","field":"type"}},"right":{"set":["destination-unreachable","time-exceeded"]}}},{"accept":null}]}},
{"rule":{"family":"inet","table":"nfvis-firewall","chain":"input","expr":[{"match":{"op":"==","left":{"payload":{"protocol":"icmpv6","field":"type"}},"right":{"set":["destination-unreachable","packet-too-big","time-exceeded","parameter-problem","nd-router-solicit","nd-router-advert","nd-neighbor-solicit","nd-neighbor-advert"]}}},{"accept":null}]}},
{"rule":{"family":"inet","table":"nfvis-firewall","chain":"input","comment":"nfvis-rule-1","expr":[{"match":{"op":"==","left":{"payload":{"protocol":"ip","field":"saddr"}},"right":{"prefix":{"addr":"192.0.2.0","len":24}}}},{"counter":{"packets":0,"bytes":0}},{"accept":null}]}}
]}`

// TestReadAppliedRealNftShape 决策 #395 修正回归：**真机形状**下正常配置不得被误判未收敛
// （旧解析把 ct state 数组右值抽成空串 ⇒ 「保留项与配置不符：ct state 数据面 ""」假未收敛）。
func TestReadAppliedRealNftShape(t *testing.T) {
	f := &fakeNft{readOut: realMachineShapeJSON}
	a := newFwApplier(t, f)
	cfg := fwCfg([]model.FirewallRule{{Seq: 1, Action: "accept", Source: "192.0.2.0/24"}}, "accept")
	st := a.Read(context.Background(), cfg)
	if !st.Applied || st.Error != "" {
		t.Fatalf("真机形状的正常配置应 applied=true，实得 %+v", st)
	}
	c, ok := st.Counters[1]
	if !ok || c.Packets != 0 || c.Bytes != 0 {
		t.Fatalf("逐规则计数应可见（真机 packets 0 / bytes 0）: %+v", st.Counters)
	}
}

func TestReadMissingTable(t *testing.T) {
	f := &fakeNft{readErr: errors.New("exit status 1"), readErrS: "Error: Could not process rule: No such file or directory\n"}
	a := newFwApplier(t, f)
	// 未配置 + 表缺失 = 一致（无过滤）
	if st := a.Read(context.Background(), model.Config{}); !st.Applied || st.Error != "" {
		t.Fatalf("未配置且无表应 applied=true: %+v", st)
	}
	// 已配置 + 表缺失 = 未收敛
	st := a.Read(context.Background(), fwCfg([]model.FirewallRule{{Seq: 1, Action: "accept", Source: "10.0.0.0/8"}}, ""))
	if st.Applied || !strings.Contains(st.Error, "表不存在") {
		t.Fatalf("已配置但表缺失应如实报未收敛: %+v", st)
	}
}

func TestReadNftUnavailable(t *testing.T) {
	f := &fakeNft{readErr: errors.New("exec: \"nft\": executable file not found in $PATH")}
	a := newFwApplier(t, f)
	st := a.Read(context.Background(), fwCfg([]model.FirewallRule{{Seq: 1, Action: "accept", Source: "10.0.0.0/8"}}, ""))
	if st.Applied || !strings.Contains(st.Error, "读取 nftables 表失败") {
		t.Fatalf("nft 不可用应如实报读不出来（不得答成未下发）: %+v", st)
	}
}

func TestReadDetectsManualEdits(t *testing.T) {
	cfg := fwCfg([]model.FirewallRule{{Seq: 100, Action: "accept", Source: "192.168.1.0/24", Protocol: "tcp", Port: 22}}, "drop")
	cases := []struct {
		name string
		json string
		part string
	}{
		{"外来规则", strings.Replace(realShapeJSON, `"comment":"nfvis-rule-100"`, `"comment":"manual-rule"`, 1), "手工修改"},
		{"保留项条数不符", strings.Replace(realShapeJSON, `{"rule":{"family":"inet","table":"nfvis-firewall","chain":"input","expr":[{"match":{"op":"in","left":{"ct":{"key":"state"}},"right":["established","related"]}},{"accept":null}]}},`, "", 1), "保留项"},
		{"规则集合不一致", strings.Replace(realShapeJSON, `"comment":"nfvis-rule-100"`, `"comment":"nfvis-rule-999"`, 1), "规则集合与配置不一致"},
		{"默认策略不一致", strings.Replace(realShapeJSON, `"policy":"drop"`, `"policy":"accept"`, 1), "默认策略不一致"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeNft{readOut: c.json}
			a := newFwApplier(t, f)
			st := a.Read(context.Background(), cfg)
			if st.Applied || !strings.Contains(st.Error, c.part) {
				t.Fatalf("应报 %q，实际 %+v", c.part, st)
			}
		})
	}
}

func TestReadDisabledButTablePresent(t *testing.T) {
	f := &fakeNft{readOut: realShapeJSON}
	a := newFwApplier(t, f)
	st := a.Read(context.Background(), model.Config{})
	if st.Applied || !strings.Contains(st.Error, "仍有") {
		t.Fatalf("未配置但表还在应如实报未收敛: %+v", st)
	}
}

// 决策 #395（R171-14/F3）：iifname 作用面内容比对——管理口改名后 apply 失败、旧表原样保留
// （条数/策略/seq 全同），回读必须如实报未收敛，而不是「已收敛」。
func TestReadDetectsMgmtScopeMismatch(t *testing.T) {
	// 表仍按 ens160 过滤，配置已改为 ens161（apply 失败、旧表留存）。
	cfg := fwCfg([]model.FirewallRule{{Seq: 100, Action: "accept", Source: "192.168.1.0/24", Protocol: "tcp", Port: 22}}, "drop")
	cfg.System.Management.Interface = "ens161"
	f := &fakeNft{readOut: realShapeJSON}
	a := newFwApplier(t, f)
	a.IfaceExists = func(string) bool { return true }
	st := a.Read(context.Background(), cfg)
	if st.Applied || !strings.Contains(st.Error, "作用面与配置不符") {
		t.Fatalf("管理口作用面不符应如实报未收敛: %+v", st)
	}
	// 未收敛时**不展示**逐规则计数（旧表 counts 会张冠李戴）。
	if len(st.Counters) != 0 {
		t.Fatalf("未收敛时不应带逐规则计数: %+v", st.Counters)
	}
}

// 决策 #395：保留项**内容**比对——条数仍是 4，但 ct 规则被换成一条裸 accept 也要判出。
func TestReadDetectsReservedContentMismatch(t *testing.T) {
	cfg := fwCfg([]model.FirewallRule{{Seq: 100, Action: "accept", Source: "192.168.1.0/24", Protocol: "tcp", Port: 22}}, "drop")
	// 把 ct 规则换成裸 accept（仍是无 comment 的保留项，条数不变）。
	edited := strings.Replace(realShapeJSON,
		`{"match":{"op":"in","left":{"ct":{"key":"state"}},"right":["established","related"]}},{"accept":null}`, `{"accept":null}`, 1)
	f := &fakeNft{readOut: edited}
	a := newFwApplier(t, f)
	st := a.Read(context.Background(), cfg)
	if st.Applied || !strings.Contains(st.Error, "保留项与配置不符") {
		t.Fatalf("保留项内容被换掉应如实报未收敛: %+v", st)
	}
	if len(st.Counters) != 0 {
		t.Fatalf("未收敛时不应带逐规则计数: %+v", st.Counters)
	}
}
