package api

// M4-5：VM 串口 console（FR-CMP-014/016、FR-OPS-032）。
//
// 流程：已认证的 `POST /virtual-machine-functions/{name}/console` 申请**一次性 ticket**
// （短时有效）——终端/浏览器无法在 WebSocket 握手上带 Bearer Token，故以 ticket 鉴权；
// `GET /virtual-machine-functions/{name}/console/ws?ticket=...` 完成 WebSocket 升级并与
// libvirt 串口双向桥接。console 打开/关闭写入审计（FR-OPS-032）。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// VMConsoleRuntime 串口 console 能力（libvirt 编排器注入；nil = 503）。
type VMConsoleRuntime interface {
	Console(ctx context.Context, name string) (io.ReadWriteCloser, error)
}

// consoleTicket 一次性 console 凭证。
type consoleTicket struct {
	vm      string
	user    string
	expires time.Time
}

// consoleTickets 进程内 ticket 表（一次性、短 TTL；M5 可换持久化/事件总线）。
type consoleTickets struct {
	mu  sync.Mutex
	m   map[string]consoleTicket
	now func() time.Time
	ttl time.Duration
}

func newConsoleTickets() *consoleTickets {
	return &consoleTickets{m: map[string]consoleTicket{}, now: time.Now, ttl: 60 * time.Second}
}

func ticketKey(vm, token string) string { return vm + "\x00" + token }

func (t *consoleTickets) issue(vm, user string) (string, int, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", 0, err
	}
	tok := hex.EncodeToString(b)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m[ticketKey(vm, tok)] = consoleTicket{vm: vm, user: user, expires: t.now().Add(t.ttl)}
	return tok, int(t.ttl.Seconds()), nil
}

// consume 校验并消费 ticket（一次性：无论成败均移除）。
func (t *consoleTickets) consume(vm, token string) (consoleTicket, bool) {
	if token == "" {
		return consoleTicket{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	k := ticketKey(vm, token)
	ct, ok := t.m[k]
	if !ok {
		return consoleTicket{}, false
	}
	delete(t.m, k)
	if t.now().After(ct.expires) {
		return consoleTicket{}, false
	}
	return ct, true
}

// handleConsoleTicket POST /api/v1/virtual-machine-functions/{name}/console
// 申请串口 console 访问凭证（契约 console 端点）。
func (s *Server) handleConsoleTicket(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.vmConsole == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "计算编排未接入（libvirt 未装配）", nil)
		return
	}
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	vm, ok := findVM(cfg, name)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("VM %s 不存在", name), nil)
		return
	}
	if vm.SerialConsole != nil && !*vm.SerialConsole {
		writeError(w, http.StatusConflict, "CONFLICT", fmt.Sprintf("VM %s 未启用串口（serial_console=false）", name), nil)
		return
	}
	user := "api"
	if info, ok := Identity(r); ok {
		user = info.User
	}
	tok, ttl, err := s.consoleTix.issue(name, user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "生成 console 凭证失败", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ws_url":     fmt.Sprintf("%s/virtual-machine-functions/%s/console/ws?ticket=%s", APIPrefix, name, tok),
		"expires_in": ttl,
	})
}

// handleConsoleWS GET /api/v1/virtual-machine-functions/{name}/console/ws?ticket=...
// 以一次性 ticket 鉴权（Bearer 不适用于 WebSocket 握手），升级后与串口双向桥接。
func (s *Server) handleConsoleWS(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.vmConsole == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "计算编排未接入（libvirt 未装配）", nil)
		return
	}
	ct, ok := s.consoleTix.consume(name, r.URL.Query().Get("ticket"))
	if !ok {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "console ticket 无效或已过期", nil)
		return
	}
	user := ct.user
	if user == "" {
		user = "api"
	}
	websocket.Handler(func(ws *websocket.Conn) {
		stream, err := s.vmConsole.Console(ws.Request().Context(), name)
		if err != nil {
			_, _ = fmt.Fprintf(ws, "\r\nconsole 打开失败: %v\r\n", err)
			s.engine.Audit(user, "vm.console", fmt.Sprintf("open console %s: %v", name, err), "failure")
			return
		}
		defer stream.Close()
		s.engine.Audit(user, "vm.console", fmt.Sprintf("open console %s", name), "success")
		defer s.engine.Audit(user, "vm.console", fmt.Sprintf("close console %s", name), "success")

		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(stream, ws); done <- struct{}{} }()
		go func() { _, _ = io.Copy(ws, stream); done <- struct{}{} }()
		<-done // 任一侧断开即结束会话（串口流 Close 由 defer 触发）
	}).ServeHTTP(w, r)
}
