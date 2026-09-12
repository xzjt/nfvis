package model

import "testing"

func TestMergeObjectsAndScalars(t *testing.T) {
	dst := Config{System: &SystemConfig{Hostname: "a", IdleTimeoutMinutes: 10}}
	patch := Config{System: &SystemConfig{Hostname: "b"}}
	got, err := Merge(dst, patch)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got.System.Hostname != "b" || got.System.IdleTimeoutMinutes != 10 {
		t.Errorf("合并结果不符: %+v", got.System)
	}
}

func TestMergeNamedArrayElements(t *testing.T) {
	dst := Config{
		VirtualMachineFunctions: []VMFunction{{
			Name: "fw-vm", Image: "img1",
			CloudInit:  &CloudInit{SSHKeys: []string{"key1"}},
			Interfaces: []VnfInterface{{Name: "eth0", Type: "vhost-user", VirtualSwitch: "vs-a"}},
		}},
	}
	// 修改既有 VM 的一条字段、追加一条 ssh-key、新增一台 VM
	patch := Config{
		VirtualMachineFunctions: []VMFunction{{
			Name:      "fw-vm",
			CloudInit: &CloudInit{SSHKeys: []string{"key2"}},
		}, {
			Name: "ct-vm", Image: "img2", VCPU: VMCpu{Count: 1}, Memory: VMMemory{SizeMB: 1024},
		}},
	}
	got, err := Merge(dst, patch)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(got.VirtualMachineFunctions) != 2 {
		t.Fatalf("应合并为 2 台 VM，实际 %d", len(got.VirtualMachineFunctions))
	}
	fw := got.VirtualMachineFunctions[0]
	if fw.Image != "img1" || fw.CloudInit == nil || len(fw.CloudInit.SSHKeys) != 1 || fw.CloudInit.SSHKeys[0] != "key2" {
		t.Errorf("fw-vm 合并结果不符: %+v", fw)
	}
	if got.VirtualMachineFunctions[1].Name != "ct-vm" {
		t.Errorf("新增 VM 缺失")
	}
}

func TestMergeSeqKeyedRules(t *testing.T) {
	dst := Config{Acls: []Acl{{Name: "acl-1", Rules: []AclRule{{Seq: 10, Action: "permit"}}}}}
	patch := Config{Acls: []Acl{{Name: "acl-1", Rules: []AclRule{{Seq: 20, Action: "deny"}}}}}
	got, err := Merge(dst, patch)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	rules := got.Acls[0].Rules
	if len(rules) != 2 || rules[0].Seq != 10 || rules[1].Seq != 20 {
		t.Errorf("seq 键控规则应合并追加: %+v", rules)
	}
}

func TestMergeNilSingleton(t *testing.T) {
	dst := Config{}
	patch := Config{Nat: &NatConfig{Static: []NatStatic{{InsideIP: "10.0.0.1", OutsideIP: "1.2.3.4"}}}}
	got, err := Merge(dst, patch)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got.Nat == nil || len(got.Nat.Static) != 1 {
		t.Errorf("nil 单例应被 patch 创建: %+v", got.Nat)
	}
}
