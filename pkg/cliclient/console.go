package cliclient

// 串口 console 的 WebSocket 客户端（M4-12，FR-CMP-014）。
//
// 服务端（api/vm_console.go）在 `POST .../console` 签发一次性 ticket 后返回相对
// ws_url；CLI 前端据此连入并把本地终端接管为串口透传。WS 客户端放在客户端 SDK
// 内——CLI 前端（internal/cli）不得 import internal/api（archtest 守护）。

import (
	"fmt"
	"io"
	"net/url"

	"golang.org/x/net/websocket"
)

// DialConsole 连接串口 WebSocket，返回双向流（调用方负责 Close）。
// wsPath 为 CLIEResult.Console.WSURL（相对路径，如 /api/v1/...）。
// 鉴权经 URL 上的 ticket（Bearer 不适用于 WebSocket 握手），故不加 Authorization 头。
func (c *Client) DialConsole(wsPath string) (io.ReadWriteCloser, error) {
	u, err := c.wsURL(wsPath)
	if err != nil {
		return nil, err
	}
	ws, err := websocket.Dial(u, "", c.wsOrigin(u))
	if err != nil {
		return nil, fmt.Errorf("连接串口 console 失败: %w", err)
	}
	return ws, nil
}

// wsURL 把相对 ws 路径解析为绝对 ws://（或 wss://）URL。
func (c *Client) wsURL(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("console ws 路径为空")
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
