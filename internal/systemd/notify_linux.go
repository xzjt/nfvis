//go:build linux

// Package systemd 与 systemd 的轻量交互（FR-OPS-013）：sd_notify 就绪与看门狗。
//
// 说明：不引入 go-systemd 依赖，直接向 $NOTIFY_SOCKET（AF_UNIX datagram）写
// systemd 约定的状态行。未由 systemd 启动（无 NOTIFY_SOCKET）时静默空操作。
package systemd

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

// Notify 向 NOTIFY_SOCKET 发送状态（如 "READY=1"、"WATCHDOG=1"、"STOPPING=1"）。
// 未由 systemd 管理时返回 nil（不视为错误）。
func Notify(state string) error {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return nil
	}
	// 抽象套接字以 @ 开头，需转为 NUL 前缀
	addr := sock
	if addr[0] == '@' {
		addr = "\x00" + addr[1:]
	}
	conn, err := net.Dial("unixgram", addr)
	if err != nil {
		return fmt.Errorf("连接 NOTIFY_SOCKET: %w", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(state)); err != nil {
		return fmt.Errorf("发送 sd_notify(%s): %w", state, err)
	}
	return nil
}

// WatchdogInterval 返回 systemd 配置的看门狗周期（WATCHDOG_USEC 的一半，
// 惯例：在超时前一半处心跳）。未启用返回 ok=false。
func WatchdogInterval() (time.Duration, bool) {
	v := os.Getenv("WATCHDOG_USEC")
	if v == "" {
		return 0, false
	}
	usec, err := strconv.ParseInt(v, 10, 64)
	if err != nil || usec <= 0 {
		return 0, false
	}
	return time.Duration(usec) * time.Microsecond / 2, true
}
