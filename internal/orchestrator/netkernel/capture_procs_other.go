//go:build !linux

package netkernel

// 非 Linux 平台：抓包跨重启收敛所需的宿主进程操作不可用（产品只在 Linux 上运行；
// 这里仅为让本包在开发/测试平台上可编译，行为按「扫不到、杀不掉」如实退化）。

import (
	"errors"
	"syscall"
)

type procEntry struct {
	PID     int
	Cmdline string
}

func scanProcsOther() []procEntry { return nil }

func killPIDOther(pid int, sig syscall.Signal) error {
	return errors.New("仅 Linux 支持向宿主进程发信号")
}

func pidAliveOther(pid int) bool { return false }

var (
	defaultScanProcs = scanProcsOther
	defaultKillPID   = killPIDOther
	defaultPIDAlive  = pidAliveOther
)
