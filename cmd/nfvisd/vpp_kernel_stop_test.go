package main

// 决策 #426①：数据面切到内核后的启动收尾——停掉不再被产品使用的 vpp.service。
//
// 口径（附录 A #426，与安装器 stop_vpp_for_kernel 同源）：只停不卸、不改启用状态；
// VPP 未装/未运行安静跳过；停完回读确认；失败如实 Warn 并给自查路径（systemctl status vpp）。

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/system"
)

// vppStopRunner 假 systemctl：按子命令应答并记录调用序列。
type vppStopRunner struct {
	calls      []string
	unitExists bool
	// active 依次应答 is-active（用尽后重复最后一个；空 = 一律 inactive）。
	active  []bool
	stopErr error
	// stopStillActive：stop 之后 is-active 仍报 active（写成功 ≠ 收敛）。
	stopStillActive bool
}

func (r *vppStopRunner) run(_ context.Context, name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, line)
	switch {
	case strings.HasPrefix(line, "systemctl list-unit-files"):
		if !r.unitExists {
			return "0 unit files listed.\n", errors.New("exit status 1")
		}
		return "vpp.service enabled\n", nil
	case strings.HasPrefix(line, "systemctl is-active"):
		n := 0
		for _, c := range r.calls[:len(r.calls)-1] {
			if strings.HasPrefix(c, "systemctl is-active") {
				n++
			}
		}
		act := false
		switch {
		case r.stopStillActive:
			act = true // 回读永远 active（含 stop 之后）
		case len(r.active) > 0:
			if n < len(r.active) {
				act = r.active[n]
			} else {
				act = r.active[len(r.active)-1]
			}
		}
		if act {
			return "active\n", nil
		}
		return "inactive\n", errors.New("exit status 3")
	case strings.HasPrefix(line, "systemctl stop"):
		if r.stopErr != nil {
			return "Failed to stop vpp.service: Operation refused.\n", r.stopErr
		}
		return "", nil
	}
	return "", nil
}

func (r *vppStopRunner) count(prefix string) int {
	n := 0
	for _, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func captureLog() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buf, nil)), buf
}

// 正常路径：已装 + 运行中 → stop 恰一次并**回读确认**；日志如实记录「已停、只停不卸」。
func TestStopVPPForKernelDataPlaneStopsAndConfirms(t *testing.T) {
	r := &vppStopRunner{unitExists: true, active: []bool{true, false}}
	o := stopVPPForKernelDataplane(context.Background(), model.DataPlaneKernel, r.run)
	if !o.Stopped || o.Err != nil {
		t.Fatalf("应停成：%+v", o)
	}
	if !o.UnitExists || !o.WasActive {
		t.Fatalf("应记录单元存在与停止前 active：%+v", o)
	}
	// 调用序列固定：先查单元 → 查运行态 → stop → 回读（顺序即「先看再动、动完回读」）。
	want := []string{
		"systemctl list-unit-files vpp.service",
		"systemctl is-active vpp.service",
		"systemctl stop vpp.service",
		"systemctl is-active vpp.service",
	}
	if strings.Join(r.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("调用序列不符：\n%v", r.calls)
	}
	// 只停不卸：任何 disable/mask/purge 都是越权改动（口径与安装器一致）。
	for _, c := range r.calls {
		for _, bad := range []string{"disable", "mask", "purge", "rm "} {
			if strings.Contains(c, bad) {
				t.Fatalf("只停不卸，不应出现 %q：%s", bad, c)
			}
		}
	}
	log, buf := captureLog()
	logVPPKernelStop(log, o)
	for _, wantMsg := range []string{"数据面已切到内核", "已停 vpp.service", "只停不卸"} {
		if !strings.Contains(buf.String(), wantMsg) {
			t.Fatalf("INFO 应含 %q：%s", wantMsg, buf.String())
		}
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("正常路径不应有 WARN：%s", buf.String())
	}
}

// VPP 未装：安静跳过——只查一次单元，不做任何 stop。
func TestStopVPPForKernelDataPlaneSkipsWhenUnitMissing(t *testing.T) {
	r := &vppStopRunner{unitExists: false, active: []bool{true}}
	o := stopVPPForKernelDataplane(context.Background(), model.DataPlaneKernel, r.run)
	if !o.skipped() || o.Stopped {
		t.Fatalf("未装应安静跳过：%+v", o)
	}
	if n := r.count("systemctl stop"); n != 0 {
		t.Fatalf("未装不得调 stop（%d 次）", n)
	}
	if n := r.count("systemctl is-active"); n != 0 {
		t.Fatalf("未装无需查运行态（%d 次）", n)
	}
	log, buf := captureLog()
	logVPPKernelStop(log, o)
	if !strings.Contains(buf.String(), "未找到 vpp.service") || strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("应一行 INFO 跳过：%s", buf.String())
	}
}

// VPP 未运行：零 stop 调用（安静跳过）。
func TestStopVPPForKernelDataPlaneSkipsWhenInactive(t *testing.T) {
	r := &vppStopRunner{unitExists: true, active: []bool{false}}
	o := stopVPPForKernelDataplane(context.Background(), model.DataPlaneKernel, r.run)
	if !o.skipped() || o.Stopped {
		t.Fatalf("未运行应安静跳过：%+v", o)
	}
	if n := r.count("systemctl stop"); n != 0 {
		t.Fatalf("未运行不得调 stop（%d 次），调用序列：%v", n, r.calls)
	}
	log, buf := captureLog()
	logVPPKernelStop(log, o)
	if !strings.Contains(buf.String(), "VPP 未在运行") || strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("应一行 INFO 跳过：%s", buf.String())
	}
}

// stop 动作失败且回读仍 active → 如实 Warn + 自查路径（不谎报已停）。
func TestStopVPPForKernelDataPlaneStopFailureWarns(t *testing.T) {
	r := &vppStopRunner{unitExists: true, active: []bool{true}, stopErr: errors.New("exit status 1"), stopStillActive: true}
	o := stopVPPForKernelDataplane(context.Background(), model.DataPlaneKernel, r.run)
	if o.Stopped || o.Err == nil {
		t.Fatalf("未停成应带错误：%+v", o)
	}
	log, buf := captureLog()
	logVPPKernelStop(log, o)
	for _, wantMsg := range []string{"level=WARN", "systemctl status vpp", "手工执行 systemctl stop vpp.service", "vfio-pci"} {
		if !strings.Contains(buf.String(), wantMsg) {
			t.Fatalf("WARN 应含 %q：%s", wantMsg, buf.String())
		}
	}
}

// stop 命令报错但回读确认已不在运行 → 按「已停成」记（命令退出码 ≠ 目标态），不误报失败。
func TestStopVPPForKernelDataPlaneStopErrorButInactiveIsSuccess(t *testing.T) {
	r := &vppStopRunner{unitExists: true, active: []bool{true, false}, stopErr: errors.New("exit status 1")}
	o := stopVPPForKernelDataplane(context.Background(), model.DataPlaneKernel, r.run)
	if !o.Stopped || o.Err != nil {
		t.Fatalf("回读已停应算成功：%+v", o)
	}
}

// stop 执行成功但回读仍 active → 如实按失败报（写成功 ≠ 收敛，与全仓口径一致）。
func TestStopVPPForKernelDataPlaneCommandOKButStillActive(t *testing.T) {
	r := &vppStopRunner{unitExists: true, active: []bool{true}, stopStillActive: true}
	o := stopVPPForKernelDataplane(context.Background(), model.DataPlaneKernel, r.run)
	if o.Stopped || o.Err == nil || !strings.Contains(o.Err.Error(), "仍为 active") {
		t.Fatalf("命令成功但未收敛应如实报错：%+v", o)
	}
}

// VPP 数据面：一个调用都不发（本收尾只属于内核方向）。
func TestStopVPPForKernelDataPlaneNoopForVPP(t *testing.T) {
	r := &vppStopRunner{unitExists: true, active: []bool{true}}
	o := stopVPPForKernelDataplane(context.Background(), model.DataPlaneVPP, r.run)
	if len(r.calls) != 0 {
		t.Fatalf("VPP 数据面不应有任何 systemctl 调用：%v", r.calls)
	}
	log, buf := captureLog()
	logVPPKernelStop(log, o)
	if buf.Len() != 0 {
		t.Fatalf("VPP 数据面不应落日志：%s", buf.String())
	}
	// 装配层只会对 kernel 调用它，但 double-check 一次「未执行」档不误导日志
	logVPPKernelStop(log, vppKernelStopOutcome{Mode: model.DataPlaneKernel, UnitExists: true, WasActive: true, Stopped: true})
	if !strings.Contains(buf.String(), "已停 vpp.service") {
		t.Fatalf("kernel 档应正常落 INFO：%s", buf.String())
	}
}

// 编译期钉住装配层用的 Runner 类型（system.Runner）——签名漂移会让启动接线直接编译失败。
var _ system.Runner = (&vppStopRunner{}).run
