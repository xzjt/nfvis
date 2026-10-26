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

// VMConsoleRuntime 串口 console / VNC 图形控制台能力（libvirt 编排器注入；nil = 503）。
type VMConsoleRuntime interface {
	Console(ctx context.Context, name string) (io.ReadWriteCloser, error)
	// VNC 打开域 VNC 图形控制台的 TCP 流（决策 #485；Web 内置查看器经此接入，仅回环）。
	VNC(ctx context.Context, name string) (io.ReadWriteCloser, error)
}

// consoleTicket 一次性终端凭证（VM 串口 console 与容器 shell **共用同一张表**，
// 靠 resource 键前缀区分，决策 #358）。
type consoleTicket struct {
	resource string
	user     string
	// tokenID 签发该 ticket 的会话稳定 ID（决策 #507）：握手期据此复核凭据是否仍然有效。
	// 空串 = 签发路径拿不到稳定 ID（CLI 侧签发，无 Bearer 上下文）——按 aaa 的保守口径放行。
	tokenID string
	expires time.Time
}

// consoleTickets 进程内 ticket 表（一次性、短 TTL；M5 可换持久化/事件总线）。
//
// sessions 一并挂在这里（决策 #507）：它需要与 ticket 表**同一生命周期**（一个 Server 一份），
// 而本结构就是装配处已按 Server 建好的那个单例（newConsoleTickets 由 api.New 调用），
// 故不再新增装配字段。
type consoleTickets struct {
	mu       sync.Mutex
	m        map[string]consoleTicket
	now      func() time.Time
	ttl      time.Duration
	sessions *consoleSessionTable
}

func newConsoleTickets() *consoleTickets {
	return &consoleTickets{m: map[string]consoleTicket{}, now: time.Now, ttl: 60 * time.Second,
		sessions: newConsoleSessionTable()}
}

// 资源键：同类会话共享一张表、不同类互不通用（一张表的 ticket 开不了另一类会话）。
func vmConsoleResource(name string) string { return "vm/" + name }
func ctShellResource(name string) string   { return "ct/" + name }

// vncConsoleResource VNC 图形查看器的资源键（决策 #485）：与串口 console（vm/）**互不通用**
// ——vnc ticket 开不了串口 WS，反之亦然（沿用 #358 的资源键前缀隔离）。
func vncConsoleResource(name string) string { return "vnc/" + name }

func ticketKey(resource, token string) string { return resource + "\x00" + token }

// issue 签发一次性 ticket（决策 #507：tokenID 为可选入参——签发它的会话稳定 ID）。
// 带 Bearer 身份的签发路径（REST 三处端点）一律传入；拿不到稳定 ID 的路径给空串，
// 消费侧对空 ID 保守放行（见 aaa.Service.ActiveTokenID 的口径与 console_sessions.go 的说明）。
func (t *consoleTickets) issue(resource, user string, tokenID ...string) (string, int, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", 0, err
	}
	tok := hex.EncodeToString(b)
	t.mu.Lock()
	defer t.mu.Unlock()
	// 决策 #374（R142 B4/E5）：随访问清扫已过期条目——未消费的一次性 ticket（如 `-c` 脚本模式
	// 申请了却不接管终端）此前会**永久驻留**（TTL 只在消费时校验）；本仓既有 idiom：
	// 随访问清扫、不新造定时器（与 DHCP 去重窗 rt.recent 同法）。
	now := t.now()
	for k, ct := range t.m {
		if now.After(ct.expires) {
			delete(t.m, k)
		}
	}
	ct := consoleTicket{resource: resource, user: user, expires: now.Add(t.ttl)}
	if len(tokenID) > 0 {
		ct.tokenID = tokenID[0]
	}
	t.m[ticketKey(resource, tok)] = ct
	return tok, int(t.ttl.Seconds()), nil
}

// consume 校验并消费 ticket（一次性：无论成败均移除）。
func (t *consoleTickets) consume(resource, token string) (consoleTicket, bool) {
	if token == "" {
		return consoleTicket{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	k := ticketKey(resource, token)
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

// ---------- 活动终端会话登记（决策 #507 的薄封装；表本体见 console_sessions.go） ----------

// registerSession 登记一条活动终端会话，返回注销函数（桥接结束 defer 调用）。
func (t *consoleTickets) registerSession(tokenID, user string, closeFn func()) func() {
	if t == nil {
		return func() {}
	}
	return t.sessions.register(tokenID, user, closeFn)
}

// closeSessionsByToken 关闭该会话稳定 ID 的全部活动终端，返回关闭条数。
func (t *consoleTickets) closeSessionsByToken(tokenID string) int {
	if t == nil {
		return 0
	}
	return t.sessions.closeByToken(tokenID)
}

// closeSessionsByUser 关闭该用户的全部活动终端（含无稳定 ID 的登记），返回关闭条数。
func (t *consoleTickets) closeSessionsByUser(user string) int {
	if t == nil {
		return 0
	}
	return t.sessions.closeByUser(user)
}

// ticketTokenID 取签发 ticket 的会话稳定 ID（无身份/无稳定 ID 时为空串 → 消费侧保守放行）。
func ticketTokenID(r *http.Request) string {
	if info, ok := Identity(r); ok {
		return info.ID
	}
	return ""
}

// ticketCredentialActive 复核 ticket 绑定会话的凭据是否仍然有效（决策 #507）。
// 空 tokenID（CLI 侧签发）保守放行：那条路径没有 Bearer 上下文，不能凭一个空串推断「已吊销」。
func (s *Server) ticketCredentialActive(tokenID string) bool {
	if s.aaa == nil {
		return true
	}
	return s.aaa.ActiveTokenID(tokenID)
}

// handleConsoleTicket POST /api/v1/virtual-machine-functions/{name}/console
// 申请串口 console 访问凭证（契约 console 端点）。
//
// 控制台形态分派（决策 #485）：响应里的 `kind` 告知前端该 VM 的有效控制台形态
// （serial|telnet|vnc，见 consoleFormOf）。**vnc 不签发串口 ticket**——VNC 是图形控制台，
// 没有文本串口可接管；此处如实回 `kind=vnc`（不带 ws_url），前端据此改走 Web 内置 noVNC
// 查看器（`POST .../console/vnc` 换一次性 ticket，见 handleVNCTicket）。VNC 仅监听 127.0.0.1，
// 宿主也可经 virsh vncdisplay 查端口后用任意 VNC 客户端接入。
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
	form := consoleFormOf(vm, cfg.VMImageProfiles)
	if form == ConsoleKindVNC {
		// VNC 无文本串口可接管：此处不签发 ticket，只如实告知形态；前端据此改走
		// POST .../console/vnc 换 VNC 查看器的一次性 ticket（handleVNCTicket）。
		writeJSON(w, http.StatusOK, map[string]any{"kind": ConsoleKindVNC})
		return
	}
	// 只有「域内确实没有串口」才拒绝（未显式设定 console 且 serial_console=false）；
	// 显式设定的形态独立于该开关（契约 §4.3），此时域内有串口、可正常签发 ticket。
	if form == "" {
		writeError(w, http.StatusConflict, "CONFLICT", fmt.Sprintf("VM %s 未启用串口（serial_console=false）", name), nil)
		return
	}
	user := "api"
	if info, ok := Identity(r); ok {
		user = info.User
	}
	// 决策 #507：ticket 绑定签发它的会话稳定 ID——吊销/登出后握手期即复核失败。
	tok, ttl, err := s.consoleTix.issue(vmConsoleResource(name), user, ticketTokenID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "生成 console 凭证失败", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ws_url":     fmt.Sprintf("%s/virtual-machine-functions/%s/console/ws?ticket=%s", APIPrefix, name, tok),
		"expires_in": ttl,
		"kind":       form,
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
	ct, ok := s.consoleTix.consume(vmConsoleResource(name), r.URL.Query().Get("ticket"))
	if !ok {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "console ticket 无效或已过期", nil)
		return
	}
	// 决策 #507：ticket 本身有效（一次性、60s 内），但签发它的会话可能已被吊销/登出/用户被删
	// ——升级前复核一次，否则「吊销凭据后仍能开终端」（round49 第 2 条）依旧成立。
	if !s.ticketCredentialActive(ct.tokenID) {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "console ticket 无效或已过期（签发它的会话已失效）", nil)
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
		// 决策 #507：登记为活动终端会话——吊销/登出/用户变更时按会话 ID 或用户关闭它
		// （关闭 = 关底层流，桥接两端随即返回）。
		defer s.consoleTix.registerSession(ct.tokenID, user, streamCloser(stream))()
		s.engine.Audit(user, "vm.console", fmt.Sprintf("open console %s", name), "success")
		defer s.engine.Audit(user, "vm.console", fmt.Sprintf("close console %s", name), "success")

		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(stream, ws); done <- struct{}{} }()
		go func() { _, _ = io.Copy(ws, stream); done <- struct{}{} }()
		<-done // 任一侧断开即结束会话（串口流 Close 由 defer 触发）
	}).ServeHTTP(w, r)
}

// handleVNCTicket POST /api/v1/virtual-machine-functions/{name}/console/vnc
// 申请 VNC 图形查看器访问凭证（一次性 ticket；决策 #485）。
//
// 与串口 console ticket 端点**同权限档**（ClassSuperUser）。仅当该 VM 的**有效控制台形态**
// 确为 vnc 时签发——否则 409 并点名实际形态（不给一个连上即断的凭证）。Web 控制台拿到
// `ws_url` 后交给内置 noVNC 查看器（vncview.js）连入；ticket 与串口/容器**资源键隔离**
// （vnc/），一个 vnc ticket 开不了串口 WS，反之亦然。
func (s *Server) handleVNCTicket(w http.ResponseWriter, r *http.Request) {
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
	form := consoleFormOf(vm, cfg.VMImageProfiles)
	if form != ConsoleKindVNC {
		writeError(w, http.StatusConflict, "CONFLICT",
			fmt.Sprintf("VM %s 的控制台形态是 %s，不是 VNC（内置 VNC 查看器仅适用于 VNC 形态）",
				name, consoleFormLabel(form)), nil)
		return
	}
	user := "api"
	if info, ok := Identity(r); ok {
		user = info.User
	}
	// 决策 #507：VNC ticket 同样绑定签发会话的稳定 ID（与串口同口径）。
	tok, ttl, err := s.consoleTix.issue(vncConsoleResource(name), user, ticketTokenID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "生成 VNC 凭证失败", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ws_url":     fmt.Sprintf("%s/virtual-machine-functions/%s/console/vnc/ws?ticket=%s", APIPrefix, name, tok),
		"expires_in": ttl,
		"kind":       ConsoleKindVNC,
	})
}

// handleVNCWS GET /api/v1/virtual-machine-functions/{name}/console/vnc/ws?ticket=...
// 以一次性 ticket 鉴权（Bearer 不适用于 WebSocket 握手），升级后与 VM 的 VNC 服务
// （127.0.0.1:<port>，端口取自域 XML）双向桥接——字节原样透传，noVNC 在前端解析 RFB 协议。
func (s *Server) handleVNCWS(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.vmConsole == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "计算编排未接入（libvirt 未装配）", nil)
		return
	}
	ct, ok := s.consoleTix.consume(vncConsoleResource(name), r.URL.Query().Get("ticket"))
	if !ok {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "VNC ticket 无效或已过期", nil)
		return
	}
	// 决策 #507：与串口 console 同口径——升级前复核签发会话是否仍然有效。
	if !s.ticketCredentialActive(ct.tokenID) {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "VNC ticket 无效或已过期（签发它的会话已失效）", nil)
		return
	}
	user := ct.user
	if user == "" {
		user = "api"
	}
	websocket.Handler(func(ws *websocket.Conn) {
		// **必须发二进制帧**：x/net/websocket 默认 PayloadType=TextFrame，
		// 会把服务端→客户端的字节当 UTF-8 文本帧发。noVNC 对每个消息做
		// `new Uint8Array(e.data)`（binaryType="arraybuffer"）——文本帧在 JS 里是字符串，
		// 转 Uint8Array 只得空数组，RFB 握手永远拿不到字节；非 UTF-8 的 VNC 字节更会触发
		// 浏览器的 1007（非法 UTF-8）关帧。RFB 是二进制协议，故此处把帧类型设为二进制
		// （字段是导出的、每次 Write 现读，见 x/net/websocket Conn.Write/PayloadType）。
		ws.PayloadType = websocket.BinaryFrame
		stream, err := s.vmConsole.VNC(ws.Request().Context(), name)
		if err != nil {
			_, _ = fmt.Fprintf(ws, "\r\nVNC 连接失败: %v\r\n", err)
			s.engine.Audit(user, "vm.vnc", fmt.Sprintf("open vnc %s: %v", name, err), "failure")
			return
		}
		defer stream.Close()
		// 决策 #507：登记为活动终端会话（资源键隔离不变，登记只按会话 ID / 用户索引）。
		defer s.consoleTix.registerSession(ct.tokenID, user, streamCloser(stream))()
		s.engine.Audit(user, "vm.vnc", fmt.Sprintf("open vnc %s", name), "success")
		defer s.engine.Audit(user, "vm.vnc", fmt.Sprintf("close vnc %s", name), "success")

		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(stream, ws); done <- struct{}{} }()
		go func() { _, _ = io.Copy(ws, stream); done <- struct{}{} }()
		<-done // 任一侧断开即结束会话（VNC 流 Close 由 defer 触发）
	}).ServeHTTP(w, r)
}

// consoleFormLabel 形态的中文名（错误文案用；空串 = 该 VM 未启用串口）。
func consoleFormLabel(form string) string {
	switch form {
	case ConsoleKindVNC:
		return "VNC 图形控制台"
	case ConsoleKindTelnet:
		return "telnet 串口"
	case ConsoleKindSerial:
		return "serial 串口"
	default:
		return "无（serial_console 已禁用）"
	}
}
