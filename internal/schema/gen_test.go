package schema

import (
	"reflect"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// gen_test：schema 一致性测试（骨架 §2 internal/schema/gen_test.go）。
// 1) 配置语句树分支 ⇄ model.Config 字段双向覆盖；
// 2) 命令树文档 §1/§2 的代表语句路径在树中可解析（命令树与执行器同源）。

// branchCovered 命令树分支 → model.Config 顶层字段的映射表。
// vrfs 由 virtual-switches 分支承载（附录 B：L3 交换机映射为同名 VRF 条目，
// 决策 #24）；qos_policies 由 qos 分支承载。
var branchCovered = map[string]string{
	"system":                    "system",
	"interfaces":                "interfaces",
	"bonds":                     "bonds",
	"virtual-switches":          "virtual_switches",
	"acls":                      "acls",
	"nat":                       "nat",
	"port-mirroring":            "port_mirroring",
	"qos":                       "qos_policies",
	"resource-pools":            "resource_pools",
	"vpp":                       "vpp",
	"protocols":                 "protocols",
	"virtual-machine-functions": "virtual_machine_functions",
	"container-functions":       "container_functions",
}

// configBranchForModelField model.Config 字段 → 命令树分支（covered 的反向）。
var configBranchForModelField = map[string]string{
	"system":                    "system",
	"interfaces":                "interfaces",
	"bonds":                     "bonds",
	"virtual_switches":          "virtual-switches",
	"vrfs":                      "virtual-switches", // 附录 B 映射
	"acls":                      "acls",
	"nat":                       "nat",
	"port_mirroring":            "port-mirroring",
	"qos_policies":              "qos",
	"resource_pools":            "resource-pools",
	"vpp":                       "vpp",
	"protocols":                 "protocols",
	"virtual_machine_functions": "virtual-machine-functions",
	"container_functions":       "container-functions",
}

func modelConfigFields(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	typ := reflect.TypeOf(model.Config{})
	for i := 0; i < typ.NumField(); i++ {
		tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if tag != "" && tag != "-" {
			out[tag] = true
		}
	}
	return out
}

func TestConfigBranchesMatchModelFields(t *testing.T) {
	fields := modelConfigFields(t)

	// 命令树 → 模型：语句树每个顶层分支都必须映射到 model.Config 字段
	path := ConfigPathTree()
	seen := map[string]bool{}
	for _, branch := range path.Children {
		field, ok := branchCovered[branch.Name]
		if !ok {
			t.Errorf("语句树分支 %q 未映射到 model.Config 字段", branch.Name)
			continue
		}
		if !fields[field] {
			t.Errorf("分支 %q 映射的字段 %q 在 model.Config 中不存在", branch.Name, field)
		}
		seen[branch.Name] = true
	}

	// 模型 → 命令树：model.Config 每个字段都必须有承载分支
	for f := range fields {
		branch, ok := configBranchForModelField[f]
		if !ok {
			t.Errorf("model.Config 字段 %q 无命令树分支映射", f)
			continue
		}
		if !seen[branch] {
			t.Errorf("字段 %q 映射的分支 %q 不在语句树中", f, branch)
		}
	}
}

// TestStatementPathsExist 验证命令树文档 §2 的代表语句路径均可被 Match 解析
// （含实例参数消耗与枚举值）。命令树文档与代码必须同源（AGENTS.md 常见错误）。
func TestStatementPathsExist(t *testing.T) {
	path := ConfigPathTree()
	cases := [][]string{
		// system
		{"system", "hostname", "nfvis-node1"},
		{"system", "timezone", "Asia/Shanghai"},
		{"system", "ntp", "server", "10.0.0.1", "prefer"},
		{"system", "dns", "server", "8.8.8.8", "secondary", "1.1.1.1"},
		{"system", "api", "port", "443"},
		{"system", "api", "tls", "cert-file", "/etc/ssl/c.pem"},
		{"system", "api", "tls", "self-signed", "regenerate"},
		{"system", "management", "ip", "address", "192.168.1.10/24"},
		{"system", "management", "gateway", "192.168.1.1"},
		{"system", "health", "thresholds", "disk-used-percent", "90"},
		{"system", "syslog", "host", "10.0.0.9", "port", "514", "severity", "warn"},
		{"system", "syslog", "local", "retention-days", "30"},
		{"system", "login", "user", "admin", "password", "x", "class", "super-user"},
		{"system", "login", "class", "netops", "allow", "show"},
		{"system", "login", "password-policy", "min-length", "12"},
		{"system", "idle-timeout-minutes", "10"},
		// protocols
		{"protocols", "lldp", "enable", "true"},
		{"protocols", "lldp", "advertisement-interval", "30"},
		{"protocols", "lldp", "interface", "ens2f0", "enable", "true"},
		// interfaces / bonds
		{"interfaces", "ens2f0", "description", "to-TOR"},
		{"interfaces", "ens2f0", "mtu", "9000"},
		{"interfaces", "ens2f0", "sriov", "vf-count", "4"},
		{"interfaces", "ens2f0", "ingress-policy", "pol-1"},
		{"bonds", "bond0", "members", "1", "ens2f0"},
		{"bonds", "bond0", "lacp", "mode", "active", "interval", "fast"},
		{"bonds", "bond0", "lacp", "disable"},
		// virtual-switches
		{"virtual-switches", "vs-app", "type", "l2"},
		{"virtual-switches", "vs-app", "vlan", "access", "100"},
		{"virtual-switches", "vs-app", "gateway", "ip", "192.168.100.1/24"},
		{"virtual-switches", "vs-app", "gateway", "vrf", "vs-mgmt"},
		{"virtual-switches", "vs-app", "ports", "1", "interface", "ens2f0", "trunk", "vlans", "100,200"},
		{"virtual-switches", "vs-app", "ports", "2", "vnf", "fw-vm", "interface", "eth0"},
		{"virtual-switches", "vs-app", "ports", "3", "container", "sbc-ct1", "interface", "eth0"},
		{"virtual-switches", "vs-app", "cross-connect", "1", "2"},
		{"virtual-switches", "vs-l3", "l3-interface", "vlan100", "ip", "address", "10.10.0.1/24"},
		{"virtual-switches", "vs-l3", "static-routes", "0.0.0.0/0", "next-hop", "10.10.0.254"},
		{"virtual-switches", "vs-l3", "static-routes", "0.0.0.0/0", "distance", "1"},
		{"virtual-switches", "vs-l3", "static-routes", "default", "next-hop", "10.10.0.254"},
		// acls / nat / port-mirroring / qos
		{"acls", "acl-web", "rule", "10", "source", "any", "destination", "10.0.0.0/8", "protocol", "tcp", "action", "permit"},
		{"acls", "acl-web", "rule", "10", "direction", "ingress"},
		{"nat", "source-pool", "pool1", "address-range", "100.64.0.10", "to", "100.64.0.50"},
		{"nat", "rules", "1", "match", "source", "192.168.100.0/24", "virtual-switch", "vs-l3", "action", "source-pool", "pool1"},
		{"nat", "static", "192.168.100.5", "to", "100.64.0.5"},
		{"port-mirroring", "span1", "source", "interface", "ens2f0", "direction", "both"},
		{"port-mirroring", "span1", "source", "vnf", "fw-vm", "interface", "eth0", "direction", "ingress"},
		{"port-mirroring", "span1", "analyzer", "interface", "ens2f1"},
		{"qos", "policies", "pol-1", "cir", "1000000", "cbs", "100000"},
		// resource-pools
		{"resource-pools", "hugepages", "page-size", "1G", "count", "32"},
		{"resource-pools", "cpu", "isolated-cores", "4-15"},
		{"resource-pools", "cpu", "numa", "node", "0", "cores", "4-7"},
		// vpp
		{"vpp", "cpu", "main-core", "4"},
		{"vpp", "cpu", "corelist-workers", "5,7"},
		{"vpp", "memory", "main-heap-size", "1G"},
		{"vpp", "memory", "hugepage-preference", "1G"},
		{"vpp", "dpdk", "dev", "rx-queues", "2"},
		{"vpp", "dpdk", "dev", "ens2f0", "rx-queues", "4"},
		{"vpp", "dpdk", "uio-driver", "vfio-pci"},
		{"vpp", "plugins", "acl", "state", "disable"},
		// virtual-machine-functions
		{"virtual-machine-functions", "fw-vm", "image", "ubuntu22-vm"},
		{"virtual-machine-functions", "fw-vm", "vcpu", "count", "4", "pin", "true"},
		{"virtual-machine-functions", "fw-vm", "memory", "size-mb", "8192"},
		{"virtual-machine-functions", "fw-vm", "memory", "hugepage-size", "1G"},
		{"virtual-machine-functions", "fw-vm", "memory", "backing", "hugepage"},
		{"virtual-machine-functions", "fw-vm", "memory", "numa", "node", "0"},
		{"virtual-machine-functions", "fw-vm", "disks", "data1", "size-gb", "100"},
		{"virtual-machine-functions", "fw-vm", "interfaces", "eth0", "type", "vhost-user"},
		{"virtual-machine-functions", "fw-vm", "interfaces", "eth1", "type", "sriov-vf"},
		{"virtual-machine-functions", "fw-vm", "interfaces", "eth1", "sriov", "physical-interface", "ens2f1", "vf", "3"},
		{"virtual-machine-functions", "fw-vm", "interfaces", "eth0", "virtual-switch", "vs-app"},
		{"virtual-machine-functions", "fw-vm", "cloud-init", "ssh-key", "ssh-ed25519 AAAA"},
		{"virtual-machine-functions", "fw-vm", "serial", "console", "enable"},
		{"virtual-machine-functions", "fw-vm", "autostart", "true"},
		// container-functions
		{"container-functions", "sbc-ct1", "image", "alpine-ct"},
		{"container-functions", "sbc-ct1", "vcpu", "count", "2"},
		{"container-functions", "sbc-ct1", "memory", "size-mb", "512"},
		{"container-functions", "sbc-ct1", "interfaces", "eth0", "type", "memif"},
		{"container-functions", "sbc-ct1", "interfaces", "eth0", "virtual-switch", "vs-app"},
		{"container-functions", "sbc-ct1", "env", "FOO", "bar"},
		{"container-functions", "sbc-ct1", "command", "/bin/sbc"},
		{"container-functions", "sbc-ct1", "restart-policy", "on-failure"},
	}
	for _, tc := range cases {
		if _, _, err := Match(path, tc); err != nil {
			t.Errorf("语句路径 %v 无法解析: %v", tc, err)
		}
	}
}

// TestOperPathsExist 抽验操作模式代表路径（命令树文档 §1）。
func TestOperPathsExist(t *testing.T) {
	oper := OperRoot()
	cases := [][]string{
		{"show", "version"},
		{"show", "system", "configuration", "sessions"},
		{"show", "interfaces", "physical", "ens2f0", "statistics"},
		{"show", "virtual-switches", "vs-app", "mac-table"},
		{"show", "vrfs", "vs-mgmt", "routes"},
		{"show", "vpp", "runtime", "thread", "1"},
		{"show", "lldp", "neighbors", "interface", "ens2f0"},
		{"show", "configuration", "candidate"},
		{"show", "log", "audit", "last", "100"},
		{"request", "virtual-machine-functions", "fw-vm", "snapshot", "create", "name", "snap1"},
		{"request", "container-functions", "sbc-ct1", "log", "last", "50"},
		{"request", "images", "upload", "name", "ubuntu22-vm", "type", "vm-image", "file", "/data/incoming/a.img"},
		{"request", "images", "download", "name", "x", "type", "container-image", "url", "https://a/b", "sha256", "deadbeef"},
		{"request", "interfaces", "ens2f0", "disable"},
		{"request", "sriov", "create-vfs", "ens2f0", "count", "4"},
		{"request", "vpp", "trace", "start", "interface", "ens2f0", "count", "1000"},
		{"request", "system", "software", "add", "/tmp/nfvis.deb", "sha256", "ff00"},
		{"request", "system", "configuration", "backup", "to", "/var/backup"},
		{"request", "system", "zeroize"},
		{"request", "system", "password", "change"},
		{"request", "alarms", "clear", "all"},
		{"ping", "10.0.0.1", "count", "3", "vrf", "vs-mgmt"},
		{"monitor", "interfaces", "ens2f0", "interval", "2"},
		{"clear", "interfaces", "statistics", "ens2f0"},
		{"start", "shell"},
	}
	for _, tc := range cases {
		if _, _, err := Match(oper, tc); err != nil {
			t.Errorf("操作路径 %v 无法解析: %v", tc, err)
		}
	}
}

// TestDeleteUsesSamePathTree delete/show/edit 与 set 消费同一语句树
// （命令树与执行器同源，?/Tab 与实际行为不漂移）。
func TestDeleteUsesSamePathTree(t *testing.T) {
	cfg := ConfigRoot()
	for _, cmd := range []string{"set", "delete", "show", "edit"} {
		set, err := Find(cfg, cmd)
		if err != nil {
			t.Fatalf("%s 不存在: %v", cmd, err)
		}
		if len(set.Children) == 0 {
			t.Fatalf("%s 应挂载配置语句树", cmd)
		}
		// 与 ConfigPathTree 的分支集合一致
		path := ConfigPathTree()
		if len(set.Children) != len(path.Children) {
			t.Fatalf("%s 分支数 %d != 语句树 %d", cmd, len(set.Children), len(path.Children))
		}
		for i, c := range set.Children {
			if c.Name != path.Children[i].Name {
				t.Fatalf("%s 第 %d 个分支不符: %s != %s", cmd, i, c.Name, path.Children[i].Name)
			}
		}
	}
}
