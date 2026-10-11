//go:build linux

package netkernel

// 决策 #466：跨 nfvisd 重启收敛要做的宿主进程操作——扫 /proc 找到重启前遗留的 tcpdump
// （记录里有 pid，但仍要按命令行核对身份，防 pid 复用），以及按 pid 有界终止。
// 平台相关内容隔离在本文件（非 Linux 下走 capture_procs_other.go 的空实现）。

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// procEntry 一个进程（pid + 完整命令行拼接）。
type procEntry struct {
	PID     int
	Cmdline string
}

// scanProcsLinux 扫描 /proc 各进程的 cmdline；读不到的（短命进程/内核线程）跳过。
func scanProcsLinux() []procEntry {
	pids, err := filepath.Glob("/proc/[0-9]*")
	if err != nil {
		return nil
	}
	out := make([]procEntry, 0, len(pids))
	for _, p := range pids {
		pid, err := strconv.Atoi(filepath.Base(p))
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(p, "cmdline"))
		if err != nil || len(data) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
		out = append(out, procEntry{PID: pid, Cmdline: strings.Join(args, " ")})
	}
	return out
}

func killPIDLinux(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }

func pidAliveLinux(pid int) bool { return syscall.Kill(pid, 0) == nil }

var (
	defaultScanProcs = scanProcsLinux
	defaultKillPID   = killPIDLinux
	defaultPIDAlive  = pidAliveLinux
)
