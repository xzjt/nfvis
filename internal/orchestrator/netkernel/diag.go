package netkernel

import (
	"context"
	"errors"
	"fmt"
	"strconv"
)

// ErrStatsClearUnsupported 内核数据面不支持清零接口统计。
var ErrStatsClearUnsupported = errors.New("内核数据面不支持清零接口统计")

// Diag 内核数据面的诊断命令实现（与 api.DiagRuntime 同形）。
//
// 与 VPP 实现的差别：不用数据面自身的 ping（VPP 的 ping 走 FIB），而是用宿主网络栈的
// ping/traceroute；目标在某个 VRF 内时经 `ip vrf exec <vrf>` 进入该表发起（等价于在域内发起）。
type Diag struct{ run Runner }

// NewDiag 构造内核诊断实现。
func NewDiag(run Runner) *Diag {
	if run == nil {
		run = NewExecRunner()
	}
	return &Diag{run: run}
}

// Ping 执行 ping；未通（命令非零退出）时如实返回错误，输出原样带回供排查。
func (d *Diag) Ping(ctx context.Context, host, source, vrf string, count int, ipv6 bool) (string, error) {
	if host == "" {
		return "", errors.New("缺少 ping 目标")
	}
	if count <= 0 {
		count = 5
	}
	args := []string{"ping", "-c", strconv.Itoa(count)}
	if ipv6 {
		args = append(args, "-6")
	}
	if source != "" {
		args = append(args, "-I", source)
	}
	args = append(args, host)
	out, err := d.runVrf(ctx, vrf, args...)
	if err != nil {
		return out, fmt.Errorf("ping %s 未通: %w", host, err)
	}
	return out, nil
}

// Traceroute 执行 traceroute；VRF 作用域经 `ip vrf exec` 进入。
func (d *Diag) Traceroute(ctx context.Context, host, vrf string, ipv6 bool) (string, error) {
	if host == "" {
		return "", errors.New("缺少 traceroute 目标")
	}
	args := []string{"traceroute"}
	if ipv6 {
		args = append(args, "-6")
	}
	args = append(args, host)
	out, err := d.runVrf(ctx, vrf, args...)
	if err != nil {
		return out, fmt.Errorf("traceroute %s 未通: %w", host, err)
	}
	return out, nil
}

// ClearInterfaceStats 内核数据面不支持（清零内核计数需要重建接口，代价与影响面过大）。
func (d *Diag) ClearInterfaceStats(context.Context, string) error {
	return ErrStatsClearUnsupported
}

// runVrf 在指定 VRF 内执行命令（vrf 为空时直接在默认表执行）。
func (d *Diag) runVrf(ctx context.Context, vrf string, args ...string) (string, error) {
	if vrf == "" {
		return d.run.Run(ctx, args[0], args[1:]...)
	}
	full := append([]string{"vrf", "exec", LinkName(vrf), args[0]}, args[1:]...)
	return d.run.Run(ctx, "ip", full...)
}
