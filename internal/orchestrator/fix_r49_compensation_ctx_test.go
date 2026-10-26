package orchestrator

// round49 缺陷 #2 的**纵深防御层**（位于 apply.go）：`Apply` 的失败补偿此前用**同一个**
// 上下文执行 undo，而这个上下文可能已被调用方取消（REST 提交的 `r.Context()` 随连接关闭取消）。
// undo 链路末端的宿主命令走 `exec.CommandContext` —— 对已取消的上下文**不执行**，补偿于是
// 静默变成空操作，数据面停在半途（比不补偿更糟：看起来走了补偿分支，实际什么都没做）。
//
// 修法：进入补偿分支时若调用方上下文已取消，改用与取消解耦、自带超时的上下文执行 undo。
// 本测试用「下发过程中取消」复现：run 阶段把 ctx 取消，失败后仍必须执行全部 undo，
// 且 undo 收到的上下文**不得**是已取消的那个。

import (
	"context"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

func TestApplyCompensationRunsAfterCallerCancel(t *testing.T) {
	calls := &[]string{}
	ctxErrs := &[]error{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ap := NewApplier(
		// bd 下发成功的那一刻取消调用方上下文（模拟客户端断开）；vm 随后失败 ⇒ 进入补偿分支。
		recNet{calls: calls, failOn: "vm:vm-1", ctxErrs: ctxErrs, cancelOn: "bd:vs-1", cancel: cancel},
		recCompute{calls: calls, failOn: "vm:vm-1"},
		recContainer{calls: calls},
	)
	old := model.Config{}
	newCfg := model.Config{
		Acls:                    []model.Acl{{Name: "acl-1", Rules: []model.AclRule{{Seq: 10, Action: "permit"}}}},
		VirtualSwitches:         []model.VirtualSwitch{{Name: "vs-1", Type: "l2"}},
		VirtualMachineFunctions: []model.VMFunction{vmOf("vm-1")},
	}

	err := ap.Apply(ctx, old, newCfg)
	if err == nil || !hasCall(*calls, "vm:vm-1") {
		t.Fatalf("应返回 vm 下发失败: %v（calls=%v）", err, *calls)
	}
	if ctx.Err() == nil {
		t.Fatalf("前提不成立：调用方上下文应在下发期间被取消")
	}
	if !hasCall(*calls, "del-bd:vs-1") || !hasCall(*calls, "del-acl:acl-1") {
		t.Fatalf("补偿必须照常执行全部 undo: %v", *calls)
	}
	if len(*ctxErrs) != 2 {
		t.Fatalf("应记录两次补偿调用的上下文状态，实际 %d 次", len(*ctxErrs))
	}
	for i, e := range *ctxErrs {
		if e != nil {
			t.Fatalf("第 %d 次补偿收到的上下文已被取消（%v）：宿主命令不会执行，补偿等于空操作", i+1, e)
		}
	}
}

// TestApplyCompensationKeepsCallerCtxWhenAlive：调用方上下文仍有效时行为**不变**——
// 补偿仍用原上下文（不新造上下文、不改变既有语义）。
func TestApplyCompensationKeepsCallerCtxWhenAlive(t *testing.T) {
	calls := &[]string{}
	ctxErrs := &[]error{}
	ap := NewApplier(
		recNet{calls: calls, failOn: "vm:vm-1", ctxErrs: ctxErrs},
		recCompute{calls: calls, failOn: "vm:vm-1"},
		recContainer{calls: calls},
	)
	newCfg := model.Config{
		Acls:                    []model.Acl{{Name: "acl-1", Rules: []model.AclRule{{Seq: 10, Action: "permit"}}}},
		VirtualSwitches:         []model.VirtualSwitch{{Name: "vs-1", Type: "l2"}},
		VirtualMachineFunctions: []model.VMFunction{vmOf("vm-1")},
	}
	if err := ap.Apply(context.Background(), model.Config{}, newCfg); err == nil {
		t.Fatalf("应返回下发失败错误")
	}
	if !hasCall(*calls, "del-bd:vs-1") {
		t.Fatalf("补偿应执行: %v", *calls)
	}
	for i, e := range *ctxErrs {
		if e != nil {
			t.Fatalf("第 %d 次补偿上下文不该被取消: %v", i+1, e)
		}
	}
}
