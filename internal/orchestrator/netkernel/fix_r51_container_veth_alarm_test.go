package netkernel

// round51 P5：容器 veth 的恢复告警要能被「修好它的那条巡检」消解。
//
// 缺陷形态（静态评审定位）：同一件事（容器 vNIC 宿主端的收敛）两条路径用了**两个不同的告警键**——
// 恢复重放（EnsureConsistent 的容器 veth 段）按逐对象来源 `container-functions/<属主>/interfaces/<vNIC>`
// 落告警，而 15s 巡检 ReconcileContainerVeth 用族级 source `container-veth` 建/消。于是重放失败
// （暂时性：桥还没收敛、底座一时不可用）之后，即便事实愈合，那条逐对象告警也无人消解：
//   - 巡检消的是族级键（另一个键）；
//   - ReconcileRecoveryAlarms 只按「对象已不在声明里」复核逐对象来源（对象还在声明里就保守保留）。
// 它要挂到下一次全量重放（nfvisd 重启/数据面切换）才消失——期间 `show alarms` 与现场相反，
// 而巡检日志同一时刻报的是「一切正常」。
//
// 处置（本用例钉住的约定）：**两条路径用同一个键**——逐对象来源串逐字相同（族级键只留给
// 「无对象可归属」的残渣清理失败）。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

func fixR51AlarmActive(store *network.AlarmStore, code, source string) bool {
	for _, a := range store.List(network.AlarmActive) {
		if a.Code == code && a.Source == source {
			return true
		}
	}
	return false
}

func TestContainerVethReplayAndReconcileShareOneAlarmSource(t *testing.T) {
	cfg := ctCfgOneContainer("ct-web")
	f := newCtVethFixture(t, cfg)
	ctx := context.Background()
	f.io.ensureErr = errors.New("注入：veth 建不出")

	// ① 巡检自身失败：落在**逐对象**来源上（与恢复重放同一串）。
	if errs := f.p.ReconcileContainerVeth(ctx, cfg); len(errs) == 0 {
		t.Fatal("宿主端建不出应如实进未收敛项")
	}
	const src = "container-functions/ct-web/interfaces/eth0"
	if !fixR51AlarmActive(f.alarms, network.AlarmUnconverged, src) {
		t.Fatalf("巡检失败应落 source=%s：%+v", src, f.alarms.List(network.AlarmActive))
	}
	if fixR51AlarmActive(f.alarms, network.AlarmUnconverged, ctVethResidueSource) {
		t.Fatalf("逐对象失败不得再落族级键（两个键正是本缺陷的成因）：%+v", f.alarms.List(network.AlarmActive))
	}

	// ② 恢复重放失败：落在**同一个**键上（不新增第二个键）。
	if errs := f.p.EnsureConsistent(ctx, cfg); len(errs) == 0 {
		t.Fatal("恢复重放应把建不出的宿主端记为未收敛")
	}
	if !fixR51AlarmActive(f.alarms, network.AlarmUnconverged, src) {
		t.Fatalf("重放失败应落同一个 source=%s：%+v", src, f.alarms.List(network.AlarmActive))
	}
	for _, a := range f.alarms.List(network.AlarmActive) {
		if strings.HasPrefix(a.Source, "container-functions/") && a.Source != src {
			t.Fatalf("同一件事实只应有一个键，多余：%+v", a)
		}
	}

	// ③ 事实愈合 + 15s 巡检一轮：**重放留下的那条告警必须当场消解**（缺陷形态：它挂到下次重启）。
	f.io.ensureErr = nil
	f.io.addBridge(LinkName(ctVethTestSwitch))
	if errs := f.p.ReconcileContainerVeth(ctx, cfg); len(errs) != 0 {
		t.Fatalf("愈合后巡检应通过：%v", errs)
	}
	if fixR51AlarmActive(f.alarms, network.AlarmUnconverged, src) {
		t.Fatalf("巡检一轮成功即应消解重放留下的告警（否则它与现场相反地长期挂着）：%+v",
			f.alarms.List(network.AlarmActive))
	}
}
