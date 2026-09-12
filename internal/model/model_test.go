package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// 契约序列化测试：JSON 字段名必须与 OpenAPI ConfigDocument 一致。
func TestConfigJSONFieldNames(t *testing.T) {
	c := Config{
		System: &SystemConfig{
			Hostname:           "nfvis-node1",
			Ntp:                []NtpServer{{Server: "10.0.0.1", Prefer: true}},
			DNSServers:         []string{"8.8.8.8", "1.1.1.1"},
			Management:         &MgmtConfig{Address: "192.168.1.10/24", Gateway: "192.168.1.1"},
			IdleTimeoutMinutes: 10,
		},
		Interfaces: []InterfaceConfig{{Name: "ens2f0", Description: "to-TOR", MTU: 9000, Sriov: &InterfaceSriov{VFCount: 4}, IngressPolicy: "pol-1"}},
		Bonds:      []Bond{{Name: "bond0", Members: []string{"ens2f0", "ens2f1"}, Lacp: &Lacp{Mode: "active", Interval: "fast"}}},
		VirtualSwitches: []VirtualSwitch{{
			Name: "vs-app", Type: "l2", VlanAccess: 100,
			Gateway: &VSGateway{Addresses: []string{"192.168.100.1/24"}, Vrf: "vr-vs-app"},
			Ports: []VSwitchPort{
				{Seq: 1, Interface: "ens2f0", TrunkVlans: []int{100, 200}, NativeVlan: 100},
				{Seq: 2, Vnf: "fw-vm", VnfInterface: "eth0", AclIn: "acl-web"},
			},
		}, {Name: "vs-xc", Type: "l2", CrossConnect: true}},
		Vrfs: []Vrf{{Name: "vs-mgmt", L3Interfaces: []L3Interface{{Interface: "ens2f0.100", Vlan: 100, Addresses: []string{"10.10.0.1/24"}, AclIn: "acl-mgmt"}}, Routes: []Route{{Prefix: "0.0.0.0/0", NextHop: "10.10.0.254", Distance: 1}}}},
		Acls: []Acl{{Name: "acl-web", Rules: []AclRule{{Seq: 10, Direction: "ingress", Source: "10.0.0.0/8", Destination: "any", Protocol: "tcp", SourcePort: "1024-65535", DestinationPort: "80", Action: "permit"}}}},
		Nat: &NatConfig{
			SourcePools: []NatSourcePool{{Name: "pool1", AddressRange: "100.64.0.10 to 100.64.0.50"}},
			Rules:       []NatRule{{Seq: 1, MatchSource: "192.168.100.0/24", VirtualSwitch: "vs-app", Action: NatAction{SourcePool: "pool1"}}},
			Static:      []NatStatic{{InsideIP: "192.168.100.5", OutsideIP: "100.64.0.5"}},
		},
		PortMirroring: []PortMirroring{{Name: "span1", Source: PMSource{Interface: "ens2f0", Direction: "both"}, Analyzer: "ens2f1"}},
		QosPolicies:   []QosPolicy{{Name: "pol-1", Cir: 1000000, Cbs: 100000}},
		ResourcePools: &ResourcePool{
			Hugepages: []HPool{{PageSize: "1G", Count: 32}},
			CPU:       &CPUSetup{IsolatedCores: []int{4, 5, 6}, Numa: []NumaNode{{Node: 0, Cores: []int{4, 5, 6}}}},
		},
		Vpp: &VppConfig{
			CPU:    &VppCPU{MainCore: 4, CorelistWorkers: "5,7"},
			Memory: &VppMemory{MainHeapSize: "1G", BuffersPerNuma: 16385, HugepagePreference: "1G"},
			DPDK: &VppDPDK{
				Dev:       VppDevDefault{RxQueues: 2, TxQueues: 2},
				PerDev:    []VppDevOverride{{Interface: "ens2f0", RxQueues: 4}},
				UIODriver: "vfio-pci",
			},
			Plugins: []VppPlugin{{Name: "acl", State: "enable"}, {Name: "nat", State: "disable"}},
		},
		Protocols: &ProtocolsConfig{LLDP: &LldpConfig{Enabled: true, AdvertisementInterval: 30, Interfaces: []LldpInterface{{Interface: "ens2f0", Enabled: true}}}},
		VirtualMachineFunctions: []VMFunction{{
			Name: "fw-vm", Image: "ubuntu22-vm",
			VCPU:   VMCpu{Count: 4, Pin: boolPtr(true)},
			Memory: VMMemory{SizeMB: 8192, HugepageSize: "1G", NumaNode: intPtr(0), Backing: "hugepage"},
			Disks:  []VMDisk{{Name: "data1", SizeGB: 100}},
			Interfaces: []VnfInterface{
				{Name: "eth0", Type: "vhost-user", VirtualSwitch: "vs-app", MAC: "52:54:00:aa:00:01", Vlan: 100},
				{Name: "eth1", Type: "sriov-vf", VirtualSwitch: "vs-underlay", Sriov: &SriovBind{PhysicalInterface: "ens2f1", VFID: 3}},
			},
			CloudInit:     &CloudInit{UserData: "#cloud-init", SSHKeys: []string{"ssh-ed25519 AAAA"}, Hostname: "fw"},
			SerialConsole: boolPtr(true),
			Autostart:     true,
		}},
		ContainerFunctions: []ContainerFunction{{
			Name: "sbc-ct1", Image: "alpine-ct", VCPU: 2, MemoryMB: 512,
			Interfaces:    []VnfInterface{{Name: "eth0", Type: "memif", VirtualSwitch: "vs-app", MAC: "52:54:00:bb:00:01"}},
			Env:           map[string]string{"FOO": "bar"},
			Command:       "/bin/sbc",
			Args:          []string{"-v"},
			RestartPolicy: "on-failure",
			Autostart:     true,
		}},
	}

	b, err := json.Marshal(&c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"system"`, `"hostname"`, `"ntp"`, `"dns_servers"`, `"management"`, `"idle_timeout_minutes"`,
		`"interfaces"`, `"sriov"`, `"vf_count"`, `"ingress_policy"`,
		`"bonds"`, `"lacp"`,
		`"virtual_switches"`, `"vlan_access"`, `"cross_connect"`, `"gateway"`, `"ports"`,
		`"trunk"`, `"native"`, `"acl_in"`,
		`"vrfs"`, `"l3_interfaces"`, `"routes"`, `"next_hop"`,
		`"acls"`, `"rules"`, `"seq"`, `"action"`,
		`"nat"`, `"source_pools"`, `"address_range"`, `"match_source"`, `"static"`, `"inside_ip"`,
		`"port_mirroring"`, `"analyzer"`, `"direction"`,
		`"qos_policies"`, `"cir"`, `"cbs"`,
		`"resource_pools"`, `"hugepages"`, `"page_size"`, `"isolated_cores"`, `"numa"`,
		`"vpp"`, `"main_core"`, `"corelist_workers"`, `"hugepage_preference"`, `"per_dev"`, `"uio_driver"`, `"plugins"`,
		`"protocols"`, `"lldp"`, `"advertisement_interval"`,
		`"virtual_machine_functions"`, `"vcpu"`, `"memory"`, `"size_mb"`, `"hugepage_size"`, `"backing"`,
		`"disks"`, `"size_gb"`, `"cloud_init"`, `"ssh_keys"`, `"serial_console"`, `"autostart"`,
		`"sriov-vf"`, `"virtual_switch"`, `"mac"`, `"vlan"`, `"vf_id"`,
		`"container_functions"`, `"memory_mb"`, `"env"`, `"restart_policy"`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("序列化输出缺少契约字段 %s", want)
		}
	}

	// 往返一致
	var back Config
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	b2, _ := json.Marshal(&back)
	if string(b) != string(b2) {
		t.Errorf("JSON 往返不一致")
	}

	// 未配置的单例不产生键
	empty, _ := json.Marshal(&Config{})
	if strings.Contains(string(empty), "system") {
		t.Errorf("空配置不应序列化出 system: %s", empty)
	}
}

func boolPtr(b bool) *bool { return &b }

func intPtr(n int) *int { return &n }
