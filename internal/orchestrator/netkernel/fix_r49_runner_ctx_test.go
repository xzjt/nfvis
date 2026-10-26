package netkernel

// round49 缺陷 #2 的第三层（runner.go）：宿主命令走 `exec.CommandContext` —— 上下文一旦取消，
// 命令**根本不执行**（或在执行中被 SIGKILL）。排障时「命令没执行」与「命令执行失败」是
// 完全不同的结论（前者什么都没发生、后者可能已部分生效），此前两者报出同一种文本，
// 于是在「补偿是否真的做了」这类判读上会得出错误结论。
//
// 本层把「已取消 ⇒ 未执行/被中断」如实点名。**注意**：这里是可读性纵深防御，
// 真正的修复在 config.Engine.Commit（下发上下文与请求取消解耦）与 apply.go（补偿上下文）。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestExecRunnerCancelledCtxReportsNotExecuted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// 用**不存在的工具名**：若上下文没被取消，这条命令会以 ErrToolMissing 失败——
	// 断言必须拿到「未执行」而不是「缺工具」，才能证明命令确实一步都没跑。
	_, err := NewExecRunner().Run(ctx, "nfvis-no-such-tool-round49")
	if err == nil {
		t.Fatalf("上下文已取消时应报错（不得静默成功）")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误应可判定为「已取消」: %v", err)
	}
	if errors.Is(err, ErrToolMissing) {
		t.Fatalf("取消不得被报成「宿主缺少工具」（两者排障结论不同）: %v", err)
	}
	if !strings.Contains(err.Error(), "未执行") {
		t.Fatalf("应说清命令没有执行: %v", err)
	}
	if !strings.Contains(err.Error(), "nfvis-no-such-tool-round49") {
		t.Fatalf("应点名是哪条命令: %v", err)
	}
}
