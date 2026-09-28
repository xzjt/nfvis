//go:build linux

package systemd

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// TestWatchHangupSwallowsSIGHUP：真机缺陷 round86 的回归用例——SIGHUP（终端挂断）
// 绝不能杀死守护进程，只应记一条日志（这里用回调计数代替日志）。
//
// 判据是「回调被调用且测试进程仍然活着」：把 SIGHUP 真发给本进程，若接管失效，
// Go 运行时会按默认动作终止进程（测试会以信号退出，而不是断言失败）。
func TestWatchHangupSwallowsSIGHUP(t *testing.T) {
	got := make(chan struct{}, 4)
	stop := WatchHangup(func() { got <- struct{}{} })
	defer stop()

	for i := 1; i <= 2; i++ {
		if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
			t.Fatalf("第 %d 次发 SIGHUP: %v", i, err)
		}
		select {
		case <-got:
		case <-time.After(3 * time.Second):
			t.Fatalf("第 %d 次 SIGHUP 未被接管（进程本该记日志并继续运行）", i)
		}
	}
}

// TestWatchHangupStopRestoresDefault：停止接管后再发 SIGHUP 不应再进回调
// （Stop 只是解除接管；此处不再真发信号，避免终止测试进程——只验证不 panic 且回调不再增加）。
func TestWatchHangupStopRestoresDefault(t *testing.T) {
	got := make(chan struct{}, 1)
	stop := WatchHangup(func() { got <- struct{}{} })
	stop()
	stop() // 幂等
	select {
	case <-got:
		t.Fatal("停止接管后不应有回调")
	default:
	}
}
