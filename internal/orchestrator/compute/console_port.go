package compute

// telnet 控制台端口的启动期占用前置判定（决策 #485 复审）。
//
// 由来（真机实证）：有效控制台形态为 telnet 的 VM 会在 **127.0.0.1** 上绑定
// `model.ConsoleTelnetPort(vm.Name)`（5700~6699，由 VM 名确定性派生，见域组装的
// `<serial type='tcp'>`/`<source mode='bind' host='127.0.0.1' service=...>`）。
// 两台产品 VM 之间的端口撞车已在**提交期**由 model.checkConsoleTelnetPortCollisions
// 拦住；但若该端口被**本机无关进程**占用，start 会以 qemu 原始报错失败
// （`Failed to find an available port: Address already in use`）——如实但不面向产品，
// 也没给操作者可照做的出路。
//
// 本文件在**启动动作**之前做一次**有界、廉价**的探测：仅当有效控制台为 telnet 时，
// 试绑 `tcp 127.0.0.1:<port>` 并立即关闭；判为占用即**快速失败**并给出产品化文案
// （端口 + 「改 VNF 名 / 改控制台为 serial|vnc」两条出路）。serial(pty) 与 vnc（libvirt
// autoport）形态不受影响。探测的其它错误（如无法判定）**不阻断**合法启动——不因无关
// 探测失败误拒一台本该能起的 VM。

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"

	"github.com/xzjt/nfvis/internal/model"
)

// consoleLoopbackHost telnet 控制台的绑定地址（与域组装一致，仅回环）。
const consoleLoopbackHost = "127.0.0.1"

// consolePortInUse 探测 `127.0.0.1:<port>` 是否已被占用：试绑后立即关闭。
//
// 返回：
//   - (true, nil)：地址已被占用（唯一判为「占用」的情形）；
//   - (false, nil)：可绑定（空闲）；
//   - (false, err)：无法判定（非 EADDRINUSE 的其它错误）——调用方**不阻断**启动。
func consolePortInUse(port int) (bool, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(consoleLoopbackHost, strconv.Itoa(port)))
	if err == nil {
		_ = ln.Close()
		return false, nil
	}
	if isAddrInUse(err) {
		return true, nil
	}
	return false, err
}

// isAddrInUse 判定错误是否为「地址已被占用」（跨平台）。
//
// Go 在 Windows 上把 socket 错误返成 syscall.Errno(10048)（WSAEADDRINUSE），而
// syscall.EADDRINUSE 在 Windows 的数值与之不同（errors.Is 不命中）；故显式比对两个数值，
// 再以错误串兜底（不依赖它做主要判据）。
func isAddrInUse(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		if errno == syscall.EADDRINUSE || errno == 10048 /* WSAEADDRINUSE */ {
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "address already in use") ||
		strings.Contains(msg, "only one usage of each socket address")
}

// checkConsolePortForStart 启动路径的 telnet 控制台端口前置判定（决策 #485 复审）。
//
// 只在**启动动作**（start，以及 restart 的 off→start 分支）调用，与数据面前置判定
// （#314）落点相同：命中即**在进入会真正绑端口的 api.Start 之前**立即失败，不产生
// 半成品域，也不把 qemu 的原始报错原样抛给操作者。
//
// 保守边界（正常路径与既有语义不变）：
//   - 拿不到该 VM 的当前声明（lookupVMSpec 未命中，如单测裸启动）⇒ 不判；
//   - 有效控制台**非 telnet**（serial/vnc）⇒ 不判（这些形态不绑派生端口）；
//   - 探测本身无法判定（非地址占用错误）⇒ 不判（不误拒合法启动）。
func (p *Provider) checkConsolePortForStart(name string) error {
	spec, ok := p.lookupVMSpec(name)
	if !ok {
		return nil
	}
	form, _ := model.EffectiveConsole(spec.vm, p.effectiveProfiles())
	if !strings.EqualFold(form, "telnet") {
		return nil
	}
	port := model.ConsoleTelnetPort(spec.vm.Name)
	inUse, err := consolePortInUse(port)
	if err != nil || !inUse {
		return nil
	}
	return fmt.Errorf("VNF %s 的 telnet 控制台端口 %d 已被本机其它进程占用——端口由 VNF 名确定性派生；"+
		"请改用别的 VNF 名，或把该 VNF 的控制台改为 serial/vnc", name, port)
}
