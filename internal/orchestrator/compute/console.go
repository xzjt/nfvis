package compute

// 串口 console 的纯解析（FR-CMP-014）。

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

// serialSourceRe 匹配 serial 块的 pty 源路径（libvirt dumpxml 属性用单引号）。
var serialSourceRe = regexp.MustCompile(`<source\s+path='([^']+)'`)

// telnet 串口（`<serial type='tcp'>`，决策 #485/Task 8）的 host/service 属性提取。
var (
	serialTCPHostRe    = regexp.MustCompile(`host='([^']*)'`)
	serialTCPServiceRe = regexp.MustCompile(`service='([^']*)'`)
)

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

// SerialTCPAddr 从 domain XML 提取 **telnet 控制台**的 TCP 目标地址（纯函数，单测覆盖）。
//
// 控制台形态 telnet 时（决策 #485/Task 8），域里的串口是 `<serial type='tcp'>` +
// `<source mode='bind' host='127.0.0.1' service='NNNN'/>`（仅回环）。本函数把该「绑在哪」
// 读出来交给访问路径去拨——**以域 XML 为单一事实源**，端口改了（如显式 ConsolePort）
// 也无需产品另存一份。
//
// 返回：
//   - 非 tcp 串口（pty 或缺串口）：("", false, nil) —— 交给 SerialPtyPath 的既有路径；
//   - tcp 串口：("<host>:<port>", true, nil)；
//   - tcp 串口但缺端口：("", false, err) —— 如实报错，不猜端口。
func SerialTCPAddr(domainXML string) (string, bool, error) {
	i := strings.Index(domainXML, "<serial")
	if i < 0 {
		return "", false, nil
	}
	rest := domainXML[i:]
	if j := strings.Index(rest, "</serial>"); j >= 0 {
		rest = rest[:j]
	}
	// 只认 `<serial ...>` 起始标签上的 type='tcp'（协议块 `<protocol type='telnet'/>` 不算）。
	open := rest
	if j := strings.Index(open, ">"); j >= 0 {
		open = open[:j]
	}
	if !strings.Contains(open, "type='tcp'") {
		return "", false, nil
	}
	host := "127.0.0.1"
	if m := serialTCPHostRe.FindStringSubmatch(rest); m != nil && strings.TrimSpace(m[1]) != "" {
		host = m[1]
	}
	m := serialTCPServiceRe.FindStringSubmatch(rest)
	if m == nil || strings.TrimSpace(m[1]) == "" {
		return "", false, fmt.Errorf("telnet 串口未给出绑定端口（service）")
	}
	return net.JoinHostPort(host, m[1]), true, nil
}
