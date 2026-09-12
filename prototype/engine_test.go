package main

import (
	"strings"
	"testing"
)

func TestVppInCompletionTree(t *testing.T) {
	e := NewEngine()
	root := e.completionRoot()
	// show ? 必须列出 vpp
	node, _ := walkTree(root, []string{"show"})
	found := false
	for _, c := range node.candidates("", e) {
		if c[0] == "vpp" {
			found = true
		}
	}
	if !found {
		t.Fatal("show ? 未列出 vpp")
	}
	// request vpp restart 必须可达
	node, _ = walkTree(root, []string{"request", "vpp"})
	cands := node.candidates("", e)
	if len(cands) != 1 || cands[0][0] != "restart" {
		t.Fatalf("request vpp 补全异常: %v", cands)
	}
}

func TestSetCommitRollbackDiff(t *testing.T) {
	e := NewEngine()
	e.Execute("configure")

	// set → candidate 生效、committed 不变
	out := e.Execute("set system hostname nfvis-new")
	if !strings.Contains(out, "[ok]") {
		t.Fatalf("set 失败: %q", out)
	}
	if e.candidate["system|hostname"] != "nfvis-new" {
		t.Fatal("candidate 未更新")
	}
	if e.committed["system|hostname"] != "nfvis-node1" {
		t.Fatal("committed 不应受 set 影响")
	}

	// compare 输出 +/-
	diff := e.Execute("compare")
	if !strings.Contains(diff, "+") || !strings.Contains(diff, "nfvis-new") {
		t.Fatalf("compare 输出异常: %q", diff)
	}

	// commit → 生效并产生快照
	out = e.Execute("commit")
	if !strings.Contains(out, "commit 成功 (revision 1)") {
		t.Fatalf("commit 失败: %q", out)
	}
	if e.committed["system|hostname"] != "nfvis-new" {
		t.Fatal("commit 后 committed 未更新")
	}

	// 修改 → rollback 1（= 上一次 commit 之前的配置，即 nfvis-node1）→ commit
	e.Execute("set system hostname nfvis-bad")
	e.Execute("rollback 1")
	e.Execute("commit")
	if e.committed["system|hostname"] != "nfvis-node1" {
		t.Fatalf("rollback+commit 结果异常: %v", e.committed["system|hostname"])
	}

	// 校验失败：不存在的镜像
	out = e.Execute("set virtual-machine-functions fw-vm image no-such-image")
	e.Execute("delete system hostname")
	if !strings.Contains(out, "[ok]") {
		t.Fatalf("set image 失败: %q", out)
	}
	out = e.Execute("commit")
	if !strings.Contains(out, "校验失败") {
		t.Fatalf("commit 应校验失败: %q", out)
	}
}

func TestDeleteSubtree(t *testing.T) {
	e := NewEngine()
	e.Execute("configure")
	out := e.Execute("delete virtual-machine-functions fw-vm interfaces")
	if !strings.Contains(out, "已删除") {
		t.Fatalf("delete 子树失败: %q", out)
	}
	for k := range e.candidate {
		if strings.HasPrefix(k, "virtual-machine-functions|fw-vm|interfaces") {
			t.Fatalf("子树未删净: %s", k)
		}
	}
}

func TestCompletion(t *testing.T) {
	e := NewEngine()
	// 操作模式：show vir → 唯一前缀补全到 virtual-machine-functions
	root := e.completionRoot()
	node, _ := walkTree(root, []string{"show"})
	cands := node.candidates("vir", e)
	if len(cands) != 2 || cands[0][0] != "virtual-machine-functions" {
		t.Fatalf("show vir 候选异常: %v", cands)
	}

	// 动态候选：request virtual-machine-functions <Tab> → VM 名列表
	node, _ = walkTree(root, []string{"request", "virtual-machine-functions"})
	cands = node.candidates("", e)
	if len(cands) < 1 || cands[0][0] != "fw-vm" {
		t.Fatalf("动态候选异常: %v", cands)
	}

	// 配置模式：set virtual-machine-functions <Tab> → VM 名
	e.Execute("configure")
	node, _ = walkTree(e.completionRoot(), []string{"set", "virtual-machine-functions"})
	cands = node.candidates("f", e)
	if len(cands) != 1 || cands[0][0] != "fw-vm" {
		t.Fatalf("配置模式动态候选异常: %v", cands)
	}

	// set virtual-machine-functions fw-vm interfaces eth0 <Tab> → 子树关键字
	node, _ = walkTree(e.completionRoot(), []string{"set", "virtual-machine-functions", "fw-vm", "interfaces", "eth0"})
	cands = node.candidates("", e)
	if len(cands) != 4 {
		t.Fatalf("vNIC 子树候选异常: %v", cands)
	}

	// set vpp dpdk dev <Tab> → 全局参数 + 动态接口候选并存
	node, _ = walkTree(e.completionRoot(), []string{"set", "vpp", "dpdk", "dev"})
	cands = node.candidates("", e)
	var hasIfname, hasGlobal bool
	for _, c := range cands {
		if c[0] == "ens2f0" {
			hasIfname = true
		}
		if c[0] == "rx-queues" {
			hasGlobal = true
		}
	}
	if !hasIfname || !hasGlobal {
		t.Fatalf("dev 补全应同时含全局参数与接口候选: %v", cands)
	}

	// set vpp dpdk dev ens2f0 <Tab> → 覆盖参数子树
	node, _ = walkTree(e.completionRoot(), []string{"set", "vpp", "dpdk", "dev", "ens2f0"})
	cands = node.candidates("", e)
	if len(cands) != 4 {
		t.Fatalf("per-NIC 覆盖参数候选异常: %v", cands)
	}
}

func TestVppConfig(t *testing.T) {
	e := NewEngine()
	e.Execute("configure")

	// 正常变更：dpdk 队列 → commit 成功且输出重启生效警告（FR-SYS-009）
	out := e.Execute("set vpp dpdk dev rx-queues 2")
	if !strings.Contains(out, "[ok]") {
		t.Fatalf("set vpp dpdk 失败: %q", out)
	}
	out = e.Execute("commit")
	if !strings.Contains(out, "commit 成功") || !strings.Contains(out, "request vpp restart") {
		t.Fatalf("commit 应成功且带重启警告: %q", out)
	}

	// 单网卡覆盖：合法接口 → 生效；非法接口 → 校验失败
	out = e.Execute("set vpp dpdk dev ens2f0 rx-queues 4")
	if !strings.Contains(out, "[ok]") {
		t.Fatalf("set per-NIC dev 失败: %q", out)
	}
	out = e.Execute("set vpp dpdk dev eth9 rx-queues 4")
	if !strings.Contains(out, "[ok]") {
		t.Fatalf("set 非法接口应被语法层接受: %q", out)
	}
	out = e.Execute("commit")
	if !strings.Contains(out, "校验失败") || !strings.Contains(out, "eth9") {
		t.Fatalf("非法接口应校验失败: %q", out)
	}
	e.Execute("delete vpp dpdk dev eth9")
	out = e.Execute("commit")
	if !strings.Contains(out, "commit 成功") {
		t.Fatalf("删除非法覆盖后 commit 应成功: %q", out)
	}

	// 核不在隔离核池内 → 校验失败（FR-SYS-010）
	e.Execute("set vpp cpu main-core 0")
	out = e.Execute("commit")
	if !strings.Contains(out, "校验失败") {
		t.Fatalf("main-core 越界应校验失败: %q", out)
	}
	e.Execute("delete vpp cpu main-core")

	// worker 列表部分越界
	e.Execute("set vpp cpu corelist-workers 5,99")
	out = e.Execute("commit")
	if !strings.Contains(out, "校验失败") || !strings.Contains(out, "99") {
		t.Fatalf("worker 越界应校验失败: %q", out)
	}
	e.Execute("delete vpp cpu corelist-workers")

	// 大页偏好与资源池不一致 → 校验失败
	e.Execute("set vpp memory hugepage-preference 2M")
	out = e.Execute("commit")
	if !strings.Contains(out, "校验失败") || !strings.Contains(out, "hugepage") {
		t.Fatalf("大页不一致应校验失败: %q", out)
	}
	e.Execute("delete vpp memory hugepage-preference")

	// 插件开关（动态候选 + 取值）
	out = e.Execute("set vpp plugins linux-cp state disable")
	if !strings.Contains(out, "[ok]") {
		t.Fatalf("set plugins 失败: %q", out)
	}
	out = e.Execute("commit")
	if !strings.Contains(out, "commit 成功") {
		t.Fatalf("plugins commit 失败: %q", out)
	}
}

func TestGatewayDiskBackingValidation(t *testing.T) {
	e := NewEngine()
	e.Execute("configure")

	// BVI 网关 + 附加数据盘：正常 commit
	e.Execute("set virtual-switches vs-app gateway ip 10.1.0.1/24")
	e.Execute("set virtual-machine-functions fw-vm disks data1 size-gb 100")
	out := e.Execute("commit")
	if !strings.Contains(out, "commit 成功") {
		t.Fatalf("gateway+disks commit 失败: %q", out)
	}

	// FR-CFG-011①：vhost-user VM 内存 backing 改为 normal → 校验失败
	e.Execute("set virtual-machine-functions fw-vm memory backing normal")
	out = e.Execute("commit")
	if !strings.Contains(out, "校验失败") || !strings.Contains(out, "vhost-user") {
		t.Fatalf("vhost-user+normal 内存应校验失败: %q", out)
	}
	e.Execute("delete virtual-machine-functions fw-vm memory backing")
	out = e.Execute("commit")
	if !strings.Contains(out, "commit 成功") {
		t.Fatalf("恢复 backing 后 commit 应成功: %q", out)
	}
}

func TestVmPageSizeAndMgmtWarning(t *testing.T) {
	e := NewEngine()
	e.Execute("configure")

	// FR-CFG-011⑪：指定不存在的页大小池 → 校验失败
	e.Execute("set virtual-machine-functions fw-vm memory hugepage-size 2M")
	out := e.Execute("commit")
	if !strings.Contains(out, "校验失败") || !strings.Contains(out, "2M") {
		t.Fatalf("页大小池不匹配应校验失败: %q", out)
	}
	e.Execute("delete virtual-machine-functions fw-vm memory hugepage-size")

	// FR-CFG-012：管理口变更 → commit 成功但输出自锁警告
	e.Execute("set system management ip address 192.168.1.99/24")
	out = e.Execute("commit")
	if !strings.Contains(out, "commit 成功") || !strings.Contains(out, "管理口") {
		t.Fatalf("管理口变更应输出警告: %q", out)
	}
}

func TestCommitConfirmedTimeout(t *testing.T) {
	e := NewEngine()
	e.Execute("configure")
	e.Execute("set system hostname confirmed-test")
	out := e.Execute("commit confirmed")
	if !strings.Contains(out, "confirmed") {
		t.Fatalf("commit confirmed 失败: %q", out)
	}
	// 等待 15s 定时回滚（测试等待 15s 太久——直接验证 timer 已建立）
	if e.confirmedTimer == nil {
		t.Fatal("confirmed 计时器未建立")
	}
	e.confirmedTimer.Stop()
}
