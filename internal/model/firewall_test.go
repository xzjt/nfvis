package model

// 决策 #388：管理面主机防火墙的配置校验（序号/动作/来源/协议/端口/至少一条匹配条件/
// 重复规则/默认策略，以及「配置了防火墙但未声明管理口」这条前置）。

import (
	"strings"
	"testing"
)

// fwBase 一份带管理口的基线（防火墙校验的唯一前置就是管理口已声明）。
func fwBase() Config {
	return Config{System: &SystemConfig{
		Management: &MgmtConfig{Interface: "ens160"},
		Firewall:   &FirewallConfig{Rules: []FirewallRule{{Seq: 10, Action: "accept", Source: "192.168.1.0/24"}}},
	}}
}

func fwSet(c Config, fw *FirewallConfig) Config {
	c.System.Firewall = fw
	return c
}

func fwErrors(c Config) []ValidateError {
	return Validate(c)
}

func TestFirewallValid(t *testing.T) {
	cases := []*FirewallConfig{
		{Rules: []FirewallRule{{Seq: 1, Action: "drop", Source: "10.0.0.0/8"}}},
		// 裸 IP（渲染时按 /32、/128 归一）与 v6 前缀
		{Rules: []FirewallRule{{Seq: 1, Action: "accept", Source: "10.1.2.3"}}},
		{Rules: []FirewallRule{{Seq: 2, Action: "accept", Source: "2001:db8::1"}}},
		// protocol 一族：tcp+port、udp 无 port、icmp 无 source（二者皆匹配）、any+port（any 时 port 拒）
		{Rules: []FirewallRule{{Seq: 3, Action: "drop", Source: "192.0.2.0/24", Protocol: "tcp", Port: 22}}},
		{Rules: []FirewallRule{{Seq: 4, Action: "drop", Protocol: "udp"}}},
		{Rules: []FirewallRule{{Seq: 5, Action: "drop", Protocol: "icmp"}}},
		{Rules: []FirewallRule{{Seq: 6, Action: "accept", Source: "2001:db8::/32", Protocol: "icmp"}}},
		// 仅默认策略（无规则）：合法；accept 显式写入也是合法配置（启用判据＝drop 或规则）
		{DefaultPolicy: "drop"},
		{DefaultPolicy: "accept"},
	}
	for i, fw := range cases {
		if errs := fwErrors(fwSet(fwBase(), fw)); len(errs) != 0 {
			t.Errorf("第 %d 例应通过，实际: %v", i, errs)
		}
	}
}

func TestFirewallInvalid(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*FirewallConfig)
		pathP   string
		msgPart string
	}{
		{"默认策略非法", func(f *FirewallConfig) { f.DefaultPolicy = "deny" }, "default_policy", "accept|drop"},
		{"序号越界", func(f *FirewallConfig) { f.Rules[0].Seq = 0 }, "rules[0]", "1-9999"},
		{"序号超上限", func(f *FirewallConfig) { f.Rules[0].Seq = 10000 }, "rules[10000]", "1-9999"},
		{"动作缺失", func(f *FirewallConfig) { f.Rules[0].Action = "" }, "rules[10].action", "accept 或 drop"},
		{"动作非法", func(f *FirewallConfig) { f.Rules[0].Action = "permit" }, "rules[10].action", "accept 或 drop"},
		{"来源非法", func(f *FirewallConfig) { f.Rules[0].Source = "192.168.1.0/33" }, "rules[10].source", "前缀"},
		{"来源是域名", func(f *FirewallConfig) { f.Rules[0].Source = "example.com" }, "rules[10].source", "前缀"},
		{"协议非法", func(f *FirewallConfig) { f.Rules[0].Protocol = "gre" }, "rules[10].protocol", "tcp|udp|icmp|any"},
		{"icmp 上给端口", func(f *FirewallConfig) {
			f.Rules[0].Protocol, f.Rules[0].Port = "icmp", 22
		}, "rules[10].port", "仅 tcp/udp"},
		{"端口越界", func(f *FirewallConfig) {
			f.Rules[0].Protocol, f.Rules[0].Port = "tcp", 70000
		}, "rules[10].port", "1-65535"},
		{"缺少匹配条件", func(f *FirewallConfig) {
			f.Rules[0].Source = ""
		}, "rules[10]", "至少给一条匹配条件"},
		{"any 协议且无来源无端口", func(f *FirewallConfig) {
			f.Rules[0].Source, f.Rules[0].Protocol = "", "any"
		}, "rules[10]", "至少给一条匹配条件"},
		{"序号重复", func(f *FirewallConfig) {
			f.Rules = append(f.Rules, FirewallRule{Seq: 10, Action: "drop", Source: "10.0.0.0/8"})
		}, "rules[10]", "重复定义"},
		{"完全重复规则", func(f *FirewallConfig) {
			f.Rules = append(f.Rules, FirewallRule{Seq: 11, Action: "accept", Source: "192.168.1.0/24"})
		}, "rules[11]", "完全重复"},
		{"重复规则（any 与未设协议归一后相同）", func(f *FirewallConfig) {
			f.Rules = append(f.Rules, FirewallRule{Seq: 12, Action: "accept", Source: "192.168.1.0/24", Protocol: "any"})
		}, "rules[12]", "完全重复"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			fw := &FirewallConfig{Rules: []FirewallRule{{Seq: 10, Action: "accept", Source: "192.168.1.0/24"}}, DefaultPolicy: "accept"}
			c.mutate(fw)
			mustErrContaining(t, fwErrors(fwSet(fwBase(), fw)), c.pathP, c.msgPart)
		})
	}
}

// 前置：防火墙被配置（有规则或默认策略 drop）而管理口未声明 ⇒ 提交期拒绝并给照做路径。
func TestFirewallRequiresManagementInterface(t *testing.T) {
	base := Config{System: &SystemConfig{}}
	for _, fw := range []*FirewallConfig{
		{Rules: []FirewallRule{{Seq: 1, Action: "accept", Source: "10.0.0.0/8"}}},
		{DefaultPolicy: "drop"},
	} {
		c := fwSet(base, fw)
		errs := fwErrors(c)
		mustErrContaining(t, errs, "system.firewall", "set system management interface")
	}
	// 未声明管理口但防火墙未启用（无规则且默认 accept 未设/accept）⇒ 不算「被配置」，放行。
	for _, fw := range []*FirewallConfig{nil, {}, {DefaultPolicy: "accept"}} {
		c := fwSet(base, fw)
		if errs := fwErrors(c); len(errs) != 0 {
			t.Fatalf("未启用的防火墙不应要求管理口，实际: %v", errs)
		}
	}
}

// 启用判据与生效策略的单一事实源（model 访问器）：渲染/校验/读视图共用。
func TestFirewallEnabledAndPolicy(t *testing.T) {
	var nilFw *FirewallConfig
	if nilFw.FirewallEnabled() || nilFw.FirewallPolicy() != "accept" {
		t.Fatal("nil 防火墙应视为未启用且缺省 accept")
	}
	if (&FirewallConfig{}).FirewallEnabled() {
		t.Fatal("空配置应视为未启用")
	}
	if !(&FirewallConfig{DefaultPolicy: "drop"}).FirewallEnabled() {
		t.Fatal("default-policy=drop 应视为启用")
	}
	if (&FirewallConfig{Rules: []FirewallRule{{Seq: 1, Action: "accept", Source: "10.0.0.0/8"}}}).FirewallPolicy() != "accept" {
		t.Fatal("未设默认策略时应生效 accept")
	}
	if !strings.HasPrefix((&FirewallConfig{DefaultPolicy: "drop"}).FirewallPolicy(), "drop") {
		t.Fatal("显式 drop 应生效 drop")
	}
}
