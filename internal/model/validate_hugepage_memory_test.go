package model

import "testing"

// 决策 #458（修 round13 R13-F）：guest RAM 由**整页大页**后备——`memory size-mb` 必须是
// 生效页尺寸的正整数倍，提交期拒绝。改前提交放行、到 `start` 才由 QEMU 以裸文案失败：
//
//	size-mb 512  + hugepage-size 1G ⇒ "memory size … must be equal to or larger than page size"
//	size-mb 1536 + hugepage-size 1G ⇒ "memory size must be multiple of 1 GiB"
func TestValidateVMMemoryMultipleOfHugepage(t *testing.T) {
	withPools := func(c Config) Config {
		// 1G 主池（validBase 的 VPP 页尺寸偏好也是 1G）+ 2M 池，便于覆盖两尺寸。
		c.ResourcePools.Hugepages = []HPool{{PageSize: "1G", Count: 32}, {PageSize: "2M", Count: 4096}}
		return c
	}

	cases := []struct {
		name    string
		sizeMB  int
		page    string // 显式 hugepage-size；空 = 按主池解析
		backing string
		wantErr bool
	}{
		{"512+1G 小于一页", 512, "1G", "", true},
		{"1536+1G 非整数倍", 1536, "1G", "", true},
		{"2048+1G 整数倍", 2048, "1G", "", false},
		{"1024+1G 恰一页", 1024, "1G", "", false},
		{"1025+2M 非整数倍", 1025, "2M", "", true},
		{"1024+2M 整数倍", 1024, "2M", "", false},
		{"未声明页尺寸按主池(1G)：512 拒", 512, "", "", true},
		{"未声明页尺寸按主池(1G)：1024 放行", 1024, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := withPools(validBase())
			c.VirtualMachineFunctions[0].Memory = VMMemory{SizeMB: tc.sizeMB, HugepageSize: tc.page, Backing: tc.backing}
			errs := Validate(c)
			if tc.wantErr {
				mustErrContaining(t, errs, "memory.size_mb", "整数倍")
			} else {
				mustNoErr(t, errs)
			}
		})
	}

	// backing=normal 用普通内存，不占大页池、不受整数倍约束（vhost-user vNIC 在 vpp 模式要求大页，
	// 故一并去掉 vNIC——本用例只问「内存尺寸是否受大页约束」）。
	c := withPools(validBase())
	c.VirtualMachineFunctions[0].Interfaces = nil
	c.VirtualMachineFunctions[0].Memory = VMMemory{SizeMB: 512, Backing: "normal"}
	mustNoErr(t, Validate(c))
}

// 页尺寸解析与账本/编排同源（vmHugepageSize）：显式声明优先，否则取资源池主池（首个页池）。
func TestVMHugepageSizeResolution(t *testing.T) {
	cfg := Config{ResourcePools: &ResourcePool{Hugepages: []HPool{{PageSize: "1G"}, {PageSize: "2M"}}}}
	if got := vmHugepageSize(cfg, VMFunction{}); got != "1G" {
		t.Fatalf("未声明时取主池：want 1G, got %q", got)
	}
	if got := vmHugepageSize(cfg, VMFunction{Memory: VMMemory{HugepageSize: "2M"}}); got != "2M" {
		t.Fatalf("显式声明优先：want 2M, got %q", got)
	}
	if got := vmHugepageSize(Config{}, VMFunction{}); got != "" {
		t.Fatalf("无池且未声明应为空：got %q", got)
	}
}
