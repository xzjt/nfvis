package system

// 宿主时钟的 NTP 同步状态（NFR-006：审计/告警时间戳依赖 NTP，未同步时事件带标记）。
//
// 判据取 systemd 的 `NTPSynchronized`——它反映**内核**的同步标志，也就是「时间戳能不能信」
// 这件事本身；取不到再退一步看 chronyc 的 Leap status。
//
// **不可知时按「未同步」返回**：这个探针的结论会作为「时间戳是否可信」的标记写进审计记录，
// 故宁可保守标记，也不能让一个查不出来源的时钟看起来是同步的。

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// clockProbeTimeout 单次探测上限——探针在审计写入路径上被调用，不能因 dbus/工具卡住而拖住写审计。
const clockProbeTimeout = 2 * time.Second

// ClockSynced 报告宿主时钟当前是否已与 NTP 同步。
func ClockSynced() bool {
	if out, err := runProbe("timedatectl", "show", "-p", "NTPSynchronized", "--value"); err == nil {
		return strings.TrimSpace(out) == "yes"
	}
	if out, err := runProbe("chronyc", "tracking"); err == nil {
		// chronyc 输出形如 "Leap status     : Normal"
		for _, line := range strings.Split(out, "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok && strings.Contains(k, "Leap status") {
				return strings.TrimSpace(v) == "Normal"
			}
		}
	}
	return false
}

func runProbe(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), clockProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}
