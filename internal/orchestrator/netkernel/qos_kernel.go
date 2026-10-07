package netkernel

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// QoS 端口限速（model.QosPolicy，CIR/CBS）在内核数据面的实现。
//
// VPP 侧把策略实现为绑在接口 input/output feature 上的 1R2C policer；内核等价物是 tc：
// 设备上一个 clsact qdisc 提供 ingress/egress 两个 hook，各挂一条 matchall filter +
// police action（超出即丢，与 policer 同义）。CIR（bps）→ police 的 rate，CBS（字节）→ burst。
//
// 方向独立：两向是不同 hook 上的独立 filter，绑一个不动另一个。
// 本族在 clsact 上使用**专用 pref**（qosFilterPref），删除按 pref 精确摘除——同一设备的
// clsact 可能同时承载其它族的 filter（如端口镜像，见 span_kernel.go），整 hook 清空会误伤别人的绑定。
const qosFilterPref = tcPrefQoS

type qosManager struct{ run Runner }

func newQoSManager(run Runner) *qosManager {
	if run == nil {
		run = NewExecRunner()
	}
	return &qosManager{run: run}
}

// Bind 在某接口的某方向装上限速计量器（1R2C，超出即丢）。
//
// 幂等：先删本方向已有的 filter 再 add——tc 的 filter add 不报错但会**重复追加**（真机实测），
// 只 add 会叠出多条计量器；重复绑定必须收敛成一条。
func (m *qosManager) Bind(ctx context.Context, dev, dir string, cirBps, cbsBytes int) error {
	if dev == "" {
		return fmt.Errorf("限速接口名不能为空")
	}
	if !qosDirection(dir) {
		return fmt.Errorf("限速方向必须是 ingress 或 egress，得到 %q", dir)
	}
	if cirBps <= 0 || cbsBytes <= 0 {
		// 真机实测：tc 对 rate 0 / burst 0 直接报解析错误（文案对操作者没有指向性），
		// 这里提前给出可读的原因，而不是把内核的解析错误原样抛上去。
		return fmt.Errorf("限速速率与突发量必须为正（速率 %d bps、突发 %d 字节）", cirBps, cbsBytes)
	}
	if err := qosEnsureClsact(ctx, m.run, dev); err != nil {
		return err
	}
	if err := qosFilterDelete(ctx, m.run, dev, dir, qosFilterPref); err != nil {
		return err
	}
	return tcReq(ctx, m.run, "filter", "add", "dev", dev, dir,
		"pref", strconv.Itoa(qosFilterPref),
		"matchall", "action", "police",
		"rate", strconv.Itoa(cirBps)+"bit",
		"burst", strconv.Itoa(cbsBytes),
		"drop")
}

// Unbind 摘除某方向的限速计量器。
//
// 只摘本方向的 filter；clsact qdisc 只在设备上再无任何 filter 时回收——另一方向可能还绑着，
// 同一 clsact 也可能还挂着别的族的 filter（如端口镜像），早删会连带把别人正在用的 hook 撤掉。
func (m *qosManager) Unbind(ctx context.Context, dev, dir string) error {
	if dev == "" {
		return fmt.Errorf("限速接口名不能为空")
	}
	if !qosDirection(dir) {
		return fmt.Errorf("限速方向必须是 ingress 或 egress，得到 %q", dir)
	}
	if err := qosFilterDelete(ctx, m.run, dev, dir, qosFilterPref); err != nil {
		return err
	}
	return qosMaybeDeleteClsact(ctx, m.run, dev)
}

// Bound 某方向是否已装本族的限速计量器（读内核事实，不读进程内登记）。
func (m *qosManager) Bound(ctx context.Context, dev, dir string) (bool, error) {
	if dev == "" || !qosDirection(dir) {
		return false, nil
	}
	out, err := tcRun(ctx, m.run, "filter", "show", "dev", dev, dir)
	if err != nil {
		if qosNoQdisc(out) {
			return false, nil // 还没有 clsact，自然没有绑定
		}
		return false, fmt.Errorf("tc filter show dev %s %s: %w（%s）", dev, dir, err, trimOut(out))
	}
	return qosHasPref(out, qosFilterPref), nil
}

// ---------- 与 tc/clsact 打交道的共用底座（本包 QoS 与端口镜像族共用） ----------

// qosDirection 方向取值是否合法。
func qosDirection(dir string) bool { return dir == "ingress" || dir == "egress" }

// qosEnsureClsact 确保设备上有 clsact qdisc（幂等）。
func qosEnsureClsact(ctx context.Context, run Runner, dev string) error {
	out, err := tcRun(ctx, run, "qdisc", "add", "dev", dev, "clsact")
	if err == nil || alreadyExists(out, err) || qosQdiscExists(out) {
		return nil
	}
	return fmt.Errorf("tc qdisc add dev %s clsact: %w（%s）", dev, err, trimOut(out))
}

// qosQdiscExists 真机实测：clsact 已存在时 `tc qdisc add` 报
// "Exclusivity flag on, cannot modify."——alreadyExists 认的是 "File exists" 家族，不认这条。
func qosQdiscExists(out string) bool {
	return strings.Contains(strings.ToLower(out), "exclusivity")
}

// qosFilterDelete 按 pref 摘除某方向上的 filter（只动本族专用的那些）。
//
// 幂等：设备上还没有 clsact（"Parent Qdisc doesn't exists"）或本就没有该 pref 的 filter
// 都按已达成处理——重复解绑不应报错。
func qosFilterDelete(ctx context.Context, run Runner, dev, dir string, pref int) error {
	out, err := tcRun(ctx, run, "filter", "del", "dev", dev, dir, "pref", strconv.Itoa(pref))
	if err == nil || notFound(out, err) || qosNoQdisc(out) || qosNoFilterChain(out) {
		return nil
	}
	return fmt.Errorf("tc filter del dev %s %s pref %d: %w（%s）", dev, dir, pref, err, trimOut(out))
}

// qosMaybeDeleteClsact 设备上再无任何 filter 时回收 clsact qdisc（幂等）。
//
// 判据是「设备上确实一条 filter 都没有」，而非「本族两向都没绑」——这样另一族（端口镜像）
// 还挂在同一 clsact 上时不会被误删。
func qosMaybeDeleteClsact(ctx context.Context, run Runner, dev string) error {
	// 共享 clsact 的回收判据统一在 tc.go（只查自己的 pref 会误判空闲）。
	return tcDropClsactIfIdle(ctx, run, dev)
}

// qosHooksEmpty 设备 clsact 的 ingress/egress 两 hook 上是否都再无 filter。
func qosHooksEmpty(ctx context.Context, run Runner, dev string) (bool, error) {
	for _, dir := range []string{"ingress", "egress"} {
		out, err := tcRun(ctx, run, "filter", "show", "dev", dev, dir)
		if err != nil {
			if qosNoQdisc(out) {
				return true, nil // 没有 qdisc 就没有 filter
			}
			return false, fmt.Errorf("tc filter show dev %s %s: %w（%s）", dev, dir, err, trimOut(out))
		}
		if strings.TrimSpace(out) != "" {
			return false, nil
		}
	}
	return true, nil
}

// qosHasPref 输出里是否有挂在给定 pref 上的 filter。
//
// tc 输出行形如 `filter protocol all pref 10 matchall chain 0`；按字段比对 pref 值，
// 避免把别的族装在别的 pref 上的 filter 误算作本族。
func qosHasPref(out string, pref int) bool {
	want := strconv.Itoa(pref)
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "pref" && f[i+1] == want {
				return true
			}
		}
	}
	return false
}

// qosNoQdisc 设备上还没有 clsact 时，filter 操作报 "Parent Qdisc doesn't exists."。
func qosNoQdisc(out string) bool {
	return strings.Contains(strings.ToLower(out), "parent qdisc")
}

// qosNoFilterChain 该 hook 上一条 filter 都没有时，按 pref 删除报
// "Cannot find specified filter chain."——与「有 filter 但没有这个 pref」的
// "Filter with specified priority/protocol not found." 同义，都是「本就没绑」，按已达成处理。
func qosNoFilterChain(out string) bool {
	return strings.Contains(strings.ToLower(out), "filter chain")
}

// qosMissingQdisc 设备上没有 clsact 时 `tc qdisc del` 报
// "Cannot find specified qdisc on specified device."（notFound 认的 "cannot find device" 不覆盖它）。
func qosMissingQdisc(out string) bool {
	s := strings.ToLower(out)
	return strings.Contains(s, "cannot find specified qdisc") || qosNoQdisc(out)
}
