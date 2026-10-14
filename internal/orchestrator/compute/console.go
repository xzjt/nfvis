package compute

// 串口 console 的纯解析（FR-CMP-014）。

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
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

// VNC 图形控制台（`<graphics type='vnc'>`，决策 #485/Task 8）的 TCP 目标提取。
// libvirt dumpxml 属性一律单引号：`<graphics type='vnc' port='5900' autoport='yes' listen='127.0.0.1'/>`
var (
	graphicsTypeRe   = regexp.MustCompile(`type='([^']*)'`)
	graphicsPortRe   = regexp.MustCompile(`port='([^']*)'`)
	graphicsListenRe = regexp.MustCompile(`listen='([^']*)'`)
	graphicsAddrRe   = regexp.MustCompile(`address='([^']*)'`)
)

// VNCTCPAddr 从 domain XML 提取 **VNC 图形控制台**的 TCP 目标地址（纯函数，单测覆盖）。
//
// 控制台形态 vnc 时（决策 #485/Task 8），域里挂的是
// `<graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1'/>`——`port='-1'` 是
// 定义期占位，域运行后 libvirt 会把它规范化成**真实端口**（如 `port='5900'`）。本函数从
// **运行中的域 XML** 把「实际监听在哪」读出来交给访问路径去拨——**以域 XML 为单一事实源**，
// 与 SerialTCPAddr 同一手法。
//
// 返回：
//   - 无 vnc 图形设备：("", false, nil) —— 该 VM 不是 VNC 形态；
//   - vnc 图形设备：("<host>:<port>", true, nil)（host 缺省 127.0.0.1）；
//   - vnc 图形设备但端口未分配（域未运行，port=-1/缺）：("", true, err) —— 如实报错，不猜端口。
func VNCTCPAddr(domainXML string) (string, bool, error) {
	idx := 0
	for {
		i := strings.Index(domainXML[idx:], "<graphics")
		if i < 0 {
			return "", false, nil
		}
		start := idx + i
		rest := domainXML[start:]
		end := strings.Index(rest, ">")
		if end < 0 {
			return "", false, nil
		}
		// 界定「以 start 起始的这个 <graphics> 元素」：
		//   - 自闭合（`<graphics .../>`）→ 只到其起始标签的 `>`；
		//   - 带子元素（如 <listen .../>）→ 扩到**本元素自己的** `</graphics>`。
		// 关键：不能无条件扩到「start 之后出现的第一个 `</graphics>`」——若前面有一个自闭合的
		// 图形元素（如 spice），那个 `</graphics>` 属于**后面的** VNC 元素，会把 VNC 元素一并
		// 吞进 block 并让 idx 越过它，之后再也找不到（复现：
		// `<graphics type='spice' .../><graphics type='vnc' ...></graphics>`）。
		block := rest[:end+1]
		if !strings.HasSuffix(strings.TrimSpace(rest[:end]), "/") {
			if j := strings.Index(rest, "</graphics>"); j >= 0 {
				block = rest[:j]
			}
		}
		idx = start + len(block)
		mt := graphicsTypeRe.FindStringSubmatch(block)
		if mt == nil || !strings.EqualFold(strings.TrimSpace(mt[1]), "vnc") {
			continue
		}
		host := "127.0.0.1"
		if m := graphicsListenRe.FindStringSubmatch(block); m != nil && strings.TrimSpace(m[1]) != "" {
			host = strings.TrimSpace(m[1])
		} else if m := graphicsAddrRe.FindStringSubmatch(block); m != nil && strings.TrimSpace(m[1]) != "" {
			host = strings.TrimSpace(m[1])
		}
		mp := graphicsPortRe.FindStringSubmatch(block)
		if mp == nil || strings.TrimSpace(mp[1]) == "" {
			return "", true, fmt.Errorf("VNC 图形未给出端口（域未运行？）")
		}
		port, err := strconv.Atoi(strings.TrimSpace(mp[1]))
		if err != nil || port <= 0 {
			return "", true, fmt.Errorf("VNC 端口尚未分配（域未运行？）")
		}
		return net.JoinHostPort(host, strconv.Itoa(port)), true, nil
	}
}

// IsLoopbackAddr 判断 "host:port"（如 net.JoinHostPort 的产物）的 host 是否为回环地址。
// 仅回环（127.0.0.0/8、::1、localhost）为真——VNC 图形服务只应监听本机，访问路径据此
// 拒绝把字节流送到任何非回环目标（纵深防御，见 OpenVNC）。
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
