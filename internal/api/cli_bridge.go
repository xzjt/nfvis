package api

// cli_bridge：CLI 专用执行端点（骨架 §2 internal/api/cli_bridge.go）。
// nfvis-cli 本地补全、远端执行——POST /api/v1/cli/execute。

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
)

type cliExecuteRequest struct {
	Line   string `json:"line"`
	Source string `json:"source"` // ssh | console（默认 ssh；console 声明仅回环采信，见 cliSessionSource）
}

// cliSessionSource 本会话的接入口来源（FR-CFG-012 自锁判定与 `start shell` 权限用）。
//
// 决策 #369（收口 R142-11）：`console` 自述**只在连接源自本机回环时采信**——物理串口 console
// 会话与 SSH 会话都经由「本机 nfvis-cli → 127.0.0.1」到达服务端，服务端能验证的事实是
// 「连接是否来自本机」；而远程连接**物理上不可能是本地串口会话**，其 `console` 声明一律按
// 网络会话（ssh）处理。效果：远程 REST/Web/CLI 无法再伪造 console 绕过管理口自锁保护；
// 本机来源（物理 console 与 SSH 登录里运行的 nfvis-cli）行为与今日一致——SSH 仍按 FR 原文
// 受守卫（本机来源里**声称** console 才豁免，非削弱）。
func cliSessionSource(r *http.Request, claimed string) string {
	source := strings.TrimSpace(claimed)
	if source == "" {
		source = "ssh"
	}
	if source == "console" && !loopbackOrigin(r) {
		return "ssh"
	}
	return source
}

// loopbackOrigin 该请求是否源自本机回环（IPv4/IPv6 回环同判；取不到地址时保守按非回环）。
func loopbackOrigin(r *http.Request) bool {
	if r == nil {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	return ip != nil && ip.IsLoopback()
}

type cliExecuteResponse struct {
	Output  string          `json:"output"`
	Mode    string          `json:"mode"`
	Path    []string        `json:"path"`
	Prompt  string          `json:"prompt"`
	Console *ConsoleRequest `json:"console,omitempty"` // M4-12：串口终端接管请求（FR-CMP-014）
	// Warning 输出为**提示**而非失败（当前唯一来源：语句未产生配置变更，round86 R86-8）。
	// nfvis-cli 脚本模式（`-c`）据此继续执行，而不是猜输出文本前缀。
	Warning bool `json:"warning,omitempty"`
}

// handleCLIExecute POST /api/v1/cli/execute：执行一行 CLI 命令。
// 逐命令权限在执行器内按 schema 节点判定（外层仅要求有效登录）。
func (s *Server) handleCLIExecute(w http.ResponseWriter, r *http.Request) {
	var req cliExecuteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Line == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "需要 line 字段", nil)
		return
	}
	// 决策 #369：接入口来源事实化（console 声明仅回环采信，见 cliSessionSource）。
	source := cliSessionSource(r, req.Source)
	info, _ := Identity(r)
	// 决策 #301：把会话稳定 ID 传入执行器——`show system api tokens` 的「当前会话」标记
	// 与吊销自己的会话时的提示都以它为判据。
	res := s.cliExec.ExecuteAs(info.User, info.Class, source, info.ID, req.Line)
	writeJSON(w, http.StatusOK, cliExecuteResponse{
		Output: res.Output, Mode: res.Mode, Path: res.Path, Prompt: res.Prompt,
		Console: res.Console, Warning: res.Warning,
	})
}
