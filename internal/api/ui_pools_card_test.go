package api

// 决策 #480：Web「资源池」页**首表**大页数字改取内核实况（与下方「大页池」段同源），
// #403 的「两处口径不同、数字不必相等」说明随之取消。
//
// 判据与 TestUIAlarmClockMarkWiring 同风格——"源码里有没有这条线"：
// 断言首表列名与取数接线为内核口径，且旧 #403 说明已去掉。**真行为仍由浏览器验收**。

import (
	"strings"
	"testing"
)

func TestUIPoolsFirstTableKernelColumns(t *testing.T) {
	js := jsAll(t)

	// 首表列名（决策 #480）：页大小 / 声明 / 总数（内核实际） / 已分配（在用） / 可分配（VNF）。
	for _, want := range []string{
		"'页大小', '声明', '总数（内核实际）', '已分配（在用）', '可分配（VNF）'",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("首表列名应为内核口径（缺 %q）", want)
		}
	}
	// 取数接线：count 用声明、total/allocated/free 用内核视图（-1 经 hpNum 显示「取不到」）。
	for _, want := range []string{"dash(h.count)", "hpNum(h.total)", "hpNum(h.allocated)", "hpNum(h.free)"} {
		if !strings.Contains(js, want) {
			t.Errorf("首表应消费内核字段 %q", want)
		}
	}
	// 说明改为「与下方大页池同源」。
	if !strings.Contains(js, "与下方「大页池」段同源") {
		t.Errorf("首表说明应改为「与下方大页池同源」")
	}
	// 旧 #403 列名/说明不得残留。
	for _, bad := range []string{"'总数（声明）'", "'已分配（VNF）'", "本表是配置口径", "两处数字不必相等"} {
		if strings.Contains(js, bad) {
			t.Errorf("旧 #403 列名/说明应已取消（不该再含 %q）", bad)
		}
	}
}
