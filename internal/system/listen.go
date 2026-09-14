package system

// FR-SEC-001（决策 #72）：管理面仅监听管理网卡。
//
// 规格要求「管理面仅监听管理网卡；业务网卡上不得出现管理服务端口」，而 systemd 单元
// 长期硬编码 NFVIS_LISTEN=:443（监听**全部**网卡）。此处按 committed 管理口地址推导
// 实际绑定地址：通配监听 → 收敛到管理口 IP；显式指定了别的地址 → 给出告警（不阻断）。

import (
	"fmt"
	"net"
	"strings"
)

// ResolveListenAddr 由 listen（host:port）与管理口地址推导实际绑定地址。
//
// 返回 (addr, note)：note 非空时表示发生了收敛或存在告警，由调用方记录。
// isLocal 判定某个 IP 是否已配置在本机（推导前须确认，否则 net.Listen 会因
// "cannot assign requested address" 直接启动失败——配置了尚未生效的管理口地址时很常见）。
func ResolveListenAddr(listen, mgmtAddress string, isLocal func(ip string) bool) (string, string) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen, "" // 无端口/格式特殊：原样返回
	}
	mgmtIP := mgmtAddrIP(mgmtAddress)
	if mgmtIP == "" {
		return listen, "" // 未配置管理口：无从推导，保持既有行为
	}
	wildcard := host == "" || host == "0.0.0.0" || host == "::" || host == "[::]"
	switch {
	case wildcard:
		if isLocal != nil && !isLocal(mgmtIP) {
			return listen, fmt.Sprintf("监听地址 %s 为通配，但管理口地址 %s 未配置在本机，暂不收敛（FR-SEC-001）", listen, mgmtIP)
		}
		return net.JoinHostPort(mgmtIP, port),
			fmt.Sprintf("监听地址 %s 为通配，已收敛为管理口地址 %s（FR-SEC-001：管理面仅监听管理网卡）", listen, mgmtIP)
	case host != mgmtIP:
		return listen, fmt.Sprintf("警告：监听地址 %s 与管理口地址 %s 不一致（FR-SEC-001：管理面应仅监听管理网卡）", host, mgmtIP)
	default:
		return listen, ""
	}
}

// mgmtAddrIP 从管理口 CIDR 取出 IP 字符串（无 CIDR 时按裸 IP 解析）。
func mgmtAddrIP(mgmtAddress string) string {
	s := strings.TrimSpace(mgmtAddress)
	if s == "" {
		return ""
	}
	if ip, _, err := net.ParseCIDR(s); err == nil {
		return ip.String()
	}
	if ip := net.ParseIP(s); ip != nil {
		return ip.String()
	}
	return ""
}

// LocalAddrChecker 返回判定「IP 是否配置在本机」的函数（FR-SEC-001 推导前的安全检查）。
func LocalAddrChecker() func(string) bool {
	return func(ip string) bool {
		want := net.ParseIP(strings.TrimSpace(ip))
		if want == nil {
			return false
		}
		addrs, err := net.InterfaceAddrs()
		if err != nil {
			return false
		}
		for _, a := range addrs {
			var got net.IP
			switch v := a.(type) {
			case *net.IPNet:
				got = v.IP
			case *net.IPAddr:
				got = v.IP
			}
			if got != nil && got.Equal(want) {
				return true
			}
		}
		return false
	}
}

// ListenSANs 由监听地址推导自签证书的 SAN IP 列表（FR-SEC-004）。
// 始终包含回环地址；解析不出 IP（如主机名）时仅返回回环。
func ListenSANs(listen string) []string {
	out := []string{"127.0.0.1", "::1"}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return out
	}
	host = strings.Trim(host, "[]")
	if host == "" || host == "0.0.0.0" || host == "::" {
		return out
	}
	if ip := net.ParseIP(host); ip != nil {
		out = append(out, ip.String())
	}
	return out
}
