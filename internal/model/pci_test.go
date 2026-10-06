package model

import (
	"strings"
	"testing"
)

// FR-CMP-023：通用 PCI 直通设备（BDF）的语法/归一——合法、非法、省略 domain 与短写。
func TestNormalizeBDF(t *testing.T) {
	valid := []struct {
		in   string
		want string
	}{
		{"0000:03:00.0", "0000:03:00.0"},
		{"0000:0b:10.1", "0000:0b:10.1"},       // 短写 bus/slot（单字符十六进制）
		{"03:00.0", "0000:03:00.0"},            // 省略 domain → 0000
		{"3:0.0", "0000:03:00.0"},              // 省略 domain + 短写
		{"0000:FF:1F.7", "0000:ff:1f.7"},       // 大小写不敏感，输出小写
		{"0x0000:0x03:0x00.0", "0000:03:00.0"}, // 0x 前缀（与 compute.ParsePCI 接受形态一致）
		{"0000:00:0f.0", "0000:00:0f.0"},       // 真机 VMware guest 的形态
	}
	for _, tc := range valid {
		got, err := NormalizeBDF(tc.in)
		if err != nil {
			t.Errorf("NormalizeBDF(%q) 应通过，实际报错: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeBDF(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}

	invalid := []string{
		"",               // 空
		"0000:03:00",     // 缺 function
		"0000:03",        // 只有两段但没有 slot.function
		"0000:03:00.0:1", // 段数过多
		"zzzz:03:00.0",   // domain 非十六进制
		"0000:xx:00.0",   // bus 非十六进制
		"0000:03:yy.0",   // slot 非十六进制
		"0000:03:00.z",   // function 非十六进制
		"00001:03:00.0",  // domain 超 4 位
		"0000:003:00.0",  // bus 超 2 位
		"0000:03:20.0",   // slot 超 0x1f
		"0000:03:00.8",   // function 超 7
		"0000-03-00.0",   // 分隔符错误
		"0000:03:00.",    // function 为空
		"0000:03:.0",     // slot 为空
	}
	for _, in := range invalid {
		if got, err := NormalizeBDF(in); err == nil {
			t.Errorf("NormalizeBDF(%q) 应报错，实际返回 %q", in, got)
		}
	}
}

func vmWithPCI(name string, bdfs ...string) VMFunction {
	return VMFunction{
		Name: name, Image: "img", VCPU: VMCpu{Count: 1},
		Memory:     VMMemory{SizeMB: 1024, HugepageSize: "1G"},
		PCIDevices: bdfs,
	}
}

// 合法 BDF（含省略 domain 与短写）通过校验。
func TestValidatePCIDevicesAccepted(t *testing.T) {
	c := validBase()
	c.VirtualMachineFunctions[0].PCIDevices = []string{"0000:03:00.0", "05:10.1"}
	if errs := Validate(c); len(errs) != 0 {
		t.Fatalf("合法 PCI 设备不应报错: %v", errs)
	}
}

// 同一 VM 内同一设备（归一后）重复声明被拒。
func TestValidatePCIDeviceDuplicateInVM(t *testing.T) {
	c := validBase()
	c.VirtualMachineFunctions[0].PCIDevices = []string{"0000:03:00.0", "03:00.0"}
	errs := Validate(c)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Path, "pci_devices[1]") && strings.Contains(e.Message, "重复声明") {
			found = true
		}
	}
	if !found {
		t.Fatalf("同 VM 内重复 BDF 应报「重复声明」: %v", errs)
	}
}

// 同一设备同时声明给两台 VM 被拒，报错点名两台的名称与设备。
func TestValidatePCIDeviceCrossVMConflict(t *testing.T) {
	c := validBase()
	c.VirtualMachineFunctions = append(c.VirtualMachineFunctions, vmWithPCI("probe-vm", "0000:03:00.0"))
	c.VirtualMachineFunctions[0].PCIDevices = []string{"03:00.0"} // 与 probe-vm 同一设备（省略 domain 写法）
	errs := Validate(c)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Path, "virtual-machine-functions[probe-vm].pci_devices[0]") &&
			strings.Contains(e.Message, "0000:03:00.0") &&
			strings.Contains(e.Message, "fw-vm") && strings.Contains(e.Message, "probe-vm") {
			found = true
		}
	}
	if !found {
		t.Fatalf("跨 VM 冲突应点名两台 VM 与设备: %v", errs)
	}
}

// 非法 BDF 在提交校验被拒（路径带下标与原始值）。
func TestValidatePCIDeviceSyntaxRejected(t *testing.T) {
	c := validBase()
	c.VirtualMachineFunctions[0].PCIDevices = []string{"0000:03:00.9"}
	errs := Validate(c)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Path, "pci_devices[0]") && strings.Contains(e.Message, "function") {
			found = true
		}
	}
	if !found {
		t.Fatalf("非法 BDF 应报语法错误: %v", errs)
	}
}
