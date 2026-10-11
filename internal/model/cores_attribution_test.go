package model

import (
	"strings"
	"testing"
)

// 决策 #463（收口 round12/13 观察②）：绑核不足的**归因可读化**。
//
// 账本按 **VM 名序**依次分配绑核，故「用户刚扩大的那台」未必是报错这台——真机现场：两台各 1 vcpu，
// 把 vnf-a 扩到 2 后报的是 vnf-b「隔离核不足」。Path 仍指向真正分不到的那台（那是正确的事实），
// 但文案必须点明**分配顺序**与**照做路径**，免得操作者跑去查一台自己没动过的 VM。
func TestAllocateCoresShortfallAttributionHint(t *testing.T) {
	cfg := Config{
		ResourcePools: &ResourcePool{
			Hugepages: []HPool{{PageSize: "1G", Count: 8}},
			CPU:       &CPUSetup{IsolatedCores: []int{4, 5}}, // 空闲核只有 2 枚
		},
		VirtualMachineFunctions: []VMFunction{
			{Name: "vnf-a", Image: "x", VCPU: VMCpu{Count: 2}, Memory: VMMemory{SizeMB: 1024, HugepageSize: "1G"}},
			{Name: "vnf-b", Image: "x", VCPU: VMCpu{Count: 1}, Memory: VMMemory{SizeMB: 1024, HugepageSize: "1G"}},
		},
	}
	l := NewPoolLedger(cfg)
	errs := l.Allocate(cfg)

	var hit *ValidateError
	for i := range errs {
		if strings.Contains(errs[i].Path, "vnf-b") && strings.Contains(errs[i].Message, "隔离核不足") {
			hit = &errs[i]
			break
		}
	}
	if hit == nil {
		t.Fatalf("名序下应由 vnf-b 报「隔离核不足」：%v", errs)
	}
	// 归因可读化（#463）：点明顺序 + 给照做路径。
	if !strings.Contains(hit.Message, "按 VM 名序") {
		t.Fatalf("文案应点明「按 VM 名序依次分配」：%s", hit.Message)
	}
	if !strings.Contains(hit.Message, "isolated-cores") {
		t.Fatalf("文案应给照做路径（扩充隔离核池）：%s", hit.Message)
	}
	// 事实本身不变：vnf-a（名序在前）仍分到 2 枚，缺口数字照旧。
	if got := l.CPU.VMCores["vnf-a"]; len(got) != 2 {
		t.Fatalf("名序在前者应分到 2 核：%v", got)
	}
	if !strings.Contains(hit.Message, "需要 1，可用 0") {
		t.Fatalf("缺口数字应保持如实：%s", hit.Message)
	}
}
