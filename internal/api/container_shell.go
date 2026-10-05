package api

// 容器交互式终端（决策 #358）：与 VM 串口 console **同一套管线**——已认证端点签发一次性
// ticket（WS 握手带不了 Bearer），WS 以 ticket 鉴权后与容器 TTY 双向桥接；CLI 前端复用
// raw 接管与 Ctrl-] 退出。底座是 Docker exec 的 TTY 形态（`Tty:true` + `Upgrade: tcp`
// ⇒ `101 UPGRADED` 全双工裸流，round139 真机实证）。
//
// 与 exec（#357）同一类边界：**断开只关产品侧桥接**——WS 断开 ⇒ 关流，但容器内的 shell 进程
// **可能仍在运行**（Docker 不提供 exec 进程的中止接口；round139 对照实验：一次会话结束后容器内
// 仍有 /bin/sh）。需要清理时用 exec 杀进程或重启容器——不谎称已释放。
// 窗口尺寸同步本期不做（如实登记，见决策 #358）。
//
// ticket 与 VM 串口**共用一张表**（consoleTickets），靠资源键前缀（ct/ 与 vm/）隔离：
// 一类会话的 ticket 开不了另一类会话。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/net/websocket"

	"github.com/xzjt/nfvis/internal/orchestrator"
)

// shellStateCheckTimeout shell ticket 端点前置状态检查的硬上界（决策 #366，R142-12）：
// dockerd 假死时不应让申请挂到客户端超时。包级 var 仅为测试可注入；生产代码不得改写。
var shellStateCheckTimeout = 10 * time.Second

// handleContainerShellTicket POST /api/v1/container-functions/{name}/shell
// 申请容器交互式终端凭证（一次性 ticket）。
func (s *Server) handleContainerShellTicket(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.containers == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "容器编排未接入（Docker 未装配）", nil)
		return
	}
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	if _, ok := findContainer(cfg, name); !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("容器 %s 不存在", name), nil)
		return
	}
	// 前置：容器须运行中。在这里就判（而不是等 WS 升级后才发现）——操作者拿到的是一句
	// 能照做的错误，而不是一个「连上了但立刻断开」的终端。编排层还会再判一次（纵深防御）。
	// 状态检查有界（决策 #366，R142-12）：dockerd 假死时不应让申请挂到客户端超时（90s）。
	sctx, cancel := context.WithTimeout(r.Context(), shellStateCheckTimeout)
	defer cancel()
	st, serr := s.containers.ContainerState(sctx, name)
	switch {
	case serr == nil && st != orchestrator.CTStateRunning:
		writeError(w, http.StatusConflict, "CONFLICT",
			fmt.Sprintf("容器 %s 未处于运行态（当前 %s）；先 request container-functions %s start", name, st, name), nil)
		return
	case errors.Is(serr, context.DeadlineExceeded):
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE",
			"Docker 未在 10s 内响应（已中止等待；请确认 docker 服务状态）", nil)
		return
	}
	user := "api"
	if info, ok := Identity(r); ok {
		user = info.User
	}
	tok, ttl, err := s.consoleTix.issue(ctShellResource(name), user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "生成终端凭证失败", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ws_url":     fmt.Sprintf("%s/container-functions/%s/shell/ws?ticket=%s", APIPrefix, name, tok),
		"expires_in": ttl,
	})
}

// handleContainerShellWS GET /api/v1/container-functions/{name}/shell/ws?ticket=...
// 以一次性 ticket 鉴权（Bearer 不适用于 WebSocket 握手，与 console 同因），升级后与容器 TTY 双向桥接。
func (s *Server) handleContainerShellWS(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.containers == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "容器编排未接入（Docker 未装配）", nil)
		return
	}
	ct, ok := s.consoleTix.consume(ctShellResource(name), r.URL.Query().Get("ticket"))
	if !ok {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "容器终端凭证无效或已过期", nil)
		return
	}
	user := ct.user
	if user == "" {
		user = "api"
	}
	websocket.Handler(func(ws *websocket.Conn) {
		stream, err := s.containers.ContainerShell(ws.Request().Context(), name)
		if err != nil {
			_, _ = fmt.Fprintf(ws, "\r\nshell 打开失败: %v\r\n", err)
			s.engine.Audit(user, "container.shell", fmt.Sprintf("open shell %s: %v", name, err), "failure")
			return
		}
		defer stream.Close()
		s.engine.Audit(user, "container.shell", fmt.Sprintf("open shell %s", name), "success")
		defer s.engine.Audit(user, "container.shell", fmt.Sprintf("close shell %s", name), "success")

		// 任一侧断开即结束**桥接**（关流由 defer 触发）；容器内的 shell 进程可能仍在运行（见文件头注释）。
		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(stream, ws); done <- struct{}{} }()
		go func() { _, _ = io.Copy(ws, stream); done <- struct{}{} }()
		<-done
	}).ServeHTTP(w, r)
}
