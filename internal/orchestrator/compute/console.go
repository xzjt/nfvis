package compute

// 串口 console 的纯解析（FR-CMP-014）。

import (
	"fmt"
	"regexp"
	"strings"
)

// serialSourceRe 匹配 serial 块的 pty 源路径（libvirt dumpxml 属性用单引号）。
var serialSourceRe = regexp.MustCompile(`<source\s+path='([^']+)'`)

// SerialPtyPath 从 domain XML 提取串口的 pty 路径（纯函数，单测覆盖）。
// 取第一个 `<serial ...>` 块的 `<source path=...>`；无串口/无 pty 时返回错误。
func SerialPtyPath(domainXML string) (string, error) {
	i := strings.Index(domainXML, "<serial")
	if i < 0 {
		return "", fmt.Errorf("域未配置串口（serial_console=false 或域未运行）")
	}
	rest := domainXML[i:]
	if j := strings.Index(rest, "</serial>"); j >= 0 {
		rest = rest[:j]
	}
	m := serialSourceRe.FindStringSubmatch(rest)
	if m == nil || m[1] == "" {
		return "", fmt.Errorf("串口未分配 pty（域未运行？）")
	}
	return m[1], nil
}
