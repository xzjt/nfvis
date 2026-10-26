package main

// round49 装配守护（静态源码级）。
//
// 由来：`config.Options.OnEvent` 自引入起**从未接线**，于是超时回滚失败、空闲锁释放失败、
// 审计写入失败（round49 才发现）在生产里只有测试能看到——「失败发生了但没人看得到」正是
// 本轮反复修的一类缺陷。告警码与钩子接线漏了不会编译失败、也不会被任何单测抓到（各包单测
// 都自己注入假件），故按本仓既有先例（`TestConfigSourceWiringOrderInMain`）用静态断言钉住：
//   ① `OnEvent` 与 `OnAuditError` 都必须在 `config.NewEngine` **之前**赋值；
//   ② 两个告警码（`CONFIRMED_ROLLBACK_FAILED` / `AUDIT_WRITE_FAILED`）必须被真正引用；
//   ③ DPDK 绑定失败的回滚告警落点（`dpdkBinder.Alarms`）必须在装配里存在。

import (
	"os"
	"strings"
	"testing"
)

func TestRound49EngineHooksWiringInMain(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)

	for _, want := range []string{
		"engineOpts.OnEvent = func(ev config.Event) {",
		"engineOpts.OnAuditError = func(en config.AuditEntry, err error) {",
		"network.AlarmConfirmedRollbackFailed",
		"network.AlarmAuditWriteFailed",
		"config.EventConfirmedRollbackFailed",
		"dpdkBinder.Alarms = alarms",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("main.go 缺少 %q（round49 的引擎钩子/告警接线）", want)
		}
	}

	// 钩子必须在引擎构造之前赋值（构造之后赋值对已创建的引擎无效）。
	iEvent := strings.Index(src, "engineOpts.OnEvent = func(")
	iAudit := strings.Index(src, "engineOpts.OnAuditError = func(")
	iEngine := strings.Index(src, "engine, err := config.NewEngine(")
	if iEvent < 0 || iAudit < 0 || iEngine < 0 {
		t.Fatalf("main.go 结构变了（找不到钩子赋值或引擎构造点），请人工核对接线")
	}
	if !(iEvent < iEngine && iAudit < iEngine) {
		t.Fatalf("钩子接线位置不对：OnEvent@%d OnAuditError@%d 引擎构造@%d（都应在构造之前）",
			iEvent, iAudit, iEngine)
	}
}

// 静态守护：`softwareController` 必须转发 `RollbackTo`（round49 真机验证抓到过一次漏转发）。
//
// 那次漏转发只在真机上暴露：`rollback to <版本>` 恒报「当前装配不支持指定版本回退」，而所有单测
// 注入的假件都自带该方法、全绿（与 `PackageState` 同族）。RollbackTo 现已升为 api.SoftwareRuntime
// 的成员（漏实现编译失败），本守护再钉一层：适配器方法存在且真的把参数转给底层管理器。
func TestSoftwareControllerForwardsRollbackTo(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	sig := "func (c *softwareController) RollbackTo(ctx context.Context, version string) (system.SoftwareResult, error) {"
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatalf("main.go 缺少 %q——api 的 rollback to <版本> 在真机上会报「装配不支持」", sig)
	}
	// 方法体里必须真的调用底层管理器（转发不能是空实现）
	rest := src[i:]
	end := strings.Index(rest, "\n}")
	if end < 0 || !strings.Contains(rest[:end], "c.m.RollbackTo(ctx, version)") {
		t.Fatalf("softwareController.RollbackTo 必须转发 c.m.RollbackTo(ctx, version)，实际：%q", rest[:min(len(rest), 200)])
	}
}
