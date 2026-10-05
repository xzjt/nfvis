package cliclient

// 串口 console 的 WebSocket 客户端（M4-12，FR-CMP-014）。
//
// 服务端（api/vm_console.go）在 `POST .../console` 签发一次性 ticket 后返回相对
// ws_url；CLI 前端据此连入并把本地终端接管为串口透传。WS 客户端放在客户端 SDK
// 内——CLI 前端（internal/cli）不得 import internal/api（archtest 守护）。

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/url"

	"golang.org/x/net/websocket"
)

// DialConsole 连接会话 WebSocket（VM 串口或容器终端），返回双向流（调用方负责 Close）。
// wsPath 为 CLIEResult.Console.WSURL（相对路径，如 /api/v1/...）。
// what 是调用方传入的**会话显示名**（如「串口」「容器终端」），用于失败文案——同一函数
// 现被 VM 串口与容器终端共用（决策 #358），写死「串口 console」会在容器终端上张冠李戴
// （决策 #375/R142 B10）。SDK 不新造 kind→文案映射，显示名由调用方给出。
// 鉴权经 URL 上的 ticket（Bearer 不适用于 WebSocket 握手），故不加 Authorization 头。
// TLS 口径与 REST **同源**（决策 #156）：证书固定 / -insecure 经 Client.tc 带入——
// 此前用默认 TLS 校验，自签 HTTPS 下 console 必挂（x509 unknown authority，真机实测）。
func (c *Client) DialConsole(wsPath, what string) (io.ReadWriteCloser, error) {
	u, err := c.wsURL(wsPath, what)
	if err != nil {
		return nil, err
	}
	cfg, err := websocket.NewConfig(u, c.wsOrigin(u))
	if err != nil {
		return nil, fmt.Errorf("连接%s失败: %w", what, err)
	}
	if c.tc != nil {
		cfg.TlsConfig = c.tlsConfigFor(hostOf(u))
	}
	ws, err := websocket.DialConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("连接%s失败: %w", what, err)
	}
	return ws, nil
}

// tlsConfigFor 克隆共享 TLS 口径并补 ServerName（x/net/websocket 的 tls.Client
// 不会从 URL 推导主机名，缺 ServerName 时校验直接报错）。
func (c *Client) tlsConfigFor(host string) *tls.Config {
	out := c.tc.Clone()
	if out.ServerName == "" {
		if h, _, err := net.SplitHostPort(host); err == nil {
			out.ServerName = h
		} else {
			out.ServerName = host
		}
	}
	return out
}

// hostOf 取 URL 的 host[:port]。
func hostOf(abs string) string {
	u, err := url.Parse(abs)
	if err != nil {
		return ""
	}
	return u.Host
}

// wsURL 把相对 ws 路径解析为绝对 ws://（或 wss://）URL。
// what 为会话显示名（用于空路径文案，见 DialConsole）。
func (c *Client) wsURL(p, what string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("%s ws 路径为空", what)
	}
	base, err := url.Parse(c.base)
	if err != nil {
		return "", fmt.Errorf("解析服务地址 %q: %w", c.base, err)
	}
	ref, err := url.Parse(p)
	if err != nil {
		return "", fmt.Errorf("解析 ws 路径 %q: %w", p, err)
	}
	abs := base.ResolveReference(ref)
	switch abs.Scheme {
	case "http":
		abs.Scheme = "ws"
	case "https":
		abs.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("不支持的 ws 协议: %s", abs.Scheme)
	}
	return abs.String(), nil
}

// wsOrigin WebSocket Origin 头（服务端不校验，但部分实现对缺省值敏感）。
func (c *Client) wsOrigin(abs string) string {
	u, err := url.Parse(abs)
	if err != nil {
		return ""
	}
	scheme := "http"
	if u.Scheme == "wss" {
		scheme = "https"
	}
	return scheme + "://" + u.Host
}
