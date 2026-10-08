package api

// R2-20：数据面判定按**装配事实**（VppController.Status().Mode，由装配处按数据面注入并
// 如实自报），不按 committed 配置。
//
// 窗口：`set system dataplane kernel` 已提交、服务还没重启——进程实际仍装着 VPP 控制器，
// 所有运行态读数（stats/buffer/路由…）事实上来自 VPP。若按 committed 判，`show vpp buffers`
// 会把不可用原因写成「当前数据面为 Linux 内核网络，无 VPP 运行态读数」——与读数的真实来路相反，
// 把排查引向一个不存在的方向。反向窗口（committed=vpp、装配=kernel）同理。
//
// 红-绿：修前 dpMode 读 committed → 第一段（committed=kernel、装配=vpp）报 kernel，
// `show vpp buffers` 打印内核原因 ⇒ 断言失败；修后按装配事实报 vpp ⇒ 通过。

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/state"
)

func TestCLIDpModeFollowsAssemblyFactNotCommitted(t *testing.T) {
	// ① committed=kernel（切换已提交）、装配=vpp（进程仍是 VPP 控制器）
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure", "set system dataplane kernel", "commit", "exit")
	x.setVppCtl(fakeVppCtl{st: VppStatus{Mode: model.DataPlaneVPP}})
	if got := x.dpMode(); got != model.DataPlaneVPP {
		t.Fatalf("committed=kernel 但装配=vpp：dpMode 应报 vpp（按装配事实），实得 %q", got)
	}
	x.setRuntime(nil, state.New(nil))
	out := x.Execute("admin", aaaClassSU, "ssh", "show vpp buffers").Output
	if strings.Contains(out, "当前数据面为 Linux 内核网络") {
		t.Fatalf("装配=vpp 时 buffer 不可用原因不得报内核数据面（读数实际来自 VPP stats）：%s", out)
	}

	// ② committed=vpp（缺省）、装配=kernel：反向窗口
	x2, _ := newCLIKit(t)
	x2.setVppCtl(fakeVppCtl{st: VppStatus{
		Mode:      model.DataPlaneKernel,
		LastError: "当前数据面为 Linux 内核网络，未使用 VPP",
	}})
	if got := x2.dpMode(); got != model.DataPlaneKernel {
		t.Fatalf("committed=vpp 但装配=kernel：dpMode 应报 kernel（按装配事实），实得 %q", got)
	}
	x2.setRuntime(nil, state.New(nil))
	out2 := x2.Execute("admin", aaaClassSU, "ssh", "show vpp buffers").Output
	if !strings.Contains(out2, "当前数据面为 Linux 内核网络") {
		t.Fatalf("装配=kernel 时 buffer 不可用原因应点名内核数据面：%s", out2)
	}

	// ③ 控制器未自报 Mode（旧装配/单测）：回落 committed，不 panic、不谎报
	x3, _ := newCLIKit(t)
	run(t, x3, "admin", aaaClassSU, "ssh",
		"configure", "set system dataplane kernel", "commit", "exit")
	x3.setVppCtl(fakeVppCtl{st: VppStatus{}})
	if got := x3.dpMode(); got != model.DataPlaneKernel {
		t.Fatalf("控制器未自报 Mode 时应回落 committed（kernel），实得 %q", got)
	}
}

// 结构化输出与文本同源：committed=vpp、装配=kernel 时不得给 `threads: 0` 假读数。
func TestCLIDpModeStructuredOutFollowsAssembly(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setVppCtl(fakeVppCtl{st: VppStatus{Mode: model.DataPlaneKernel}})
	x.setRuntime(nil, state.New(nil))
	out := x.Execute("admin", aaaClassSU, "ssh", "show vpp | display json").Output
	if strings.Contains(out, `"threads"`) {
		t.Fatalf("内核数据面结构化输出不得给 threads 假读数：%s", out)
	}
}
