//go:build linux

// SIGHUP 免疫（决策 #183）。
//
// 为什么守护进程要专门处理 SIGHUP：POSIX 会话的**控制终端**一挂断，内核就向前台进程组
// 发 SIGHUP，默认动作是**终止进程**。nfvisd 是长驻服务，任何「意外获得控制终端」的路径
// （例如以非 root 之外的 fd 打开某个 pty）都可能让它因为一次终端挂断而消失——而它消失时
// 请求方只看到连接 EOF，故障现象与真实原因相距甚远：
//
//	真机实测（round86）：`request virtual-machine-functions <vm> console` 打开的 VM 串口 pty
//	曾是控制终端（打开时未带 O_NOCTTY），此后 stop 该 VM → QEMU 关闭 pty master → nfvisd
//	收到 SIGHUP 并退出；systemd 按 Restart=always 把它拉起来，排障时只能看到「一条 stop
//	请求 EOF + 服务重启计数 +1」，从现象推不到根因。
//
// 根因已由 O_NOCTTY 修掉（internal/orchestrator/compute/console_libvirt.go）；这里是
// **纵深防御**：无论未来哪条路径让进程沾上终端语义，SIGHUP 都不再能杀死它。
package systemd

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// WatchHangup 让本进程对 SIGHUP 免疫：每次收到只回调一次 onHangup（用于记日志），进程继续运行。
// onHangup 为空时静默忽略。返回停止函数（幂等；测试/退出时用）。
//
// 只接管 SIGHUP——SIGTERM/SIGINT 仍由调用方（main 的 NotifyContext）掌控，优雅停机语义不变。
func WatchHangup(onHangup func()) (stop func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-ch:
				if onHangup != nil {
					onHangup()
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			signal.Stop(ch)
			close(done)
		})
	}
}
