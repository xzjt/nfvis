package config

// 决策 #308（round84 R84-18）：cross-connect 交换机端口数不足（<2）时提交必须**如实提示**
// 「本次提交不会建立直通」。
//
// 背景：点对点直通必须两端，applier 在 cross_connect=true 且端口数 <2 时**有意什么都不挂载**
// （合法无操作，旧测试 TestL2CrossConnectDetach 即把「缩减到 1 端口」判为合法），但提交照常
// 成功、操作者看不到任何提示。触发路径是「先建两点直通、再删掉一个端口」。本决策不改合法性，
// 只在提交期既有结构化警告通道（CommitResult.Warnings）把「没生效」说清楚。

import (
	"context"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// crossConnectConfig 造一台 cross-connect 交换机，端口数由 nports 决定（1 或 2）。
func crossConnectConfig(nports int) model.Config {
	on := true
	cfg := baseCommitted()
	cfg.Interfaces = append(cfg.Interfaces, model.InterfaceConfig{Name: "ens2f1", Enabled: &on})
	ports := []model.VSwitchPort{{Seq: 1, Interface: "ens2f0"}}
	if nports >= 2 {
		ports = append(ports, model.VSwitchPort{Seq: 2, Interface: "ens2f1"})
	}
	cfg.VirtualSwitches = []model.VirtualSwitch{
		{Name: "vs-xc", Type: "l2", CrossConnect: true, Ports: ports},
	}
	return cfg
}

// 两点直通建好后缩减到一个端口：提交成功，但必须提示「不会建立直通」。
func TestCommitWarnsOnCrossConnectSinglePort(t *testing.T) {
	k := newEngineKit(t)
	sess := Session{User: "admin", Source: "console"}

	// 前置：把两点直通提交为 committed
	k.edit(t, "admin", "console")
	if err := k.engine.UpdateCandidate(sess, crossConnectConfig(2)); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	if _, err := k.engine.Commit(context.Background(), sess, CommitOpts{}); err != nil {
		t.Fatalf("前置提交: %v", err)
	}

	// 缩减到一个端口（真机 R84-18 的触发路径）
	k.edit(t, "admin", "console")
	if err := k.engine.UpdateCandidate(sess, crossConnectConfig(1)); err != nil {
		t.Fatalf("UpdateCandidate2: %v", err)
	}
	res, err := k.engine.Commit(context.Background(), sess, CommitOpts{})
	if err != nil {
		t.Fatalf("缩减提交应成功（合法性不变）: %v", err)
	}
	if !warningsContain(res.Warnings, "vs-xc") || !warningsContain(res.Warnings, "不会建立直通") {
		t.Fatalf("cross-connect 单端口应如实提示「本次提交不会建立直通」: %+v", res.Warnings)
	}
}

// 端口数正常（恰两个）的 cross-connect 交换机不得出现该提示（不制造无谓文本）。
func TestCommitNoWarningOnCrossConnectTwoPorts(t *testing.T) {
	k := newEngineKit(t)
	sess := Session{User: "admin", Source: "console"}
	k.edit(t, "admin", "console")
	if err := k.engine.UpdateCandidate(sess, crossConnectConfig(2)); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	res, err := k.engine.Commit(context.Background(), sess, CommitOpts{})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if warningsContain(res.Warnings, "不会建立直通") {
		t.Fatalf("两端就绪的 cross-connect 不应提示「不会建立直通」: %+v", res.Warnings)
	}
}
