package api

// cli_bridge：CLI 专用执行端点（骨架 §2 internal/api/cli_bridge.go）。
// nfvis-cli 本地补全、远端执行——POST /api/v1/cli/execute。

import (
	"encoding/json"
	"net/http"
)

type cliExecuteRequest struct {
	Line   string `json:"line"`
	Source string `json:"source"` // ssh | console（默认 ssh，影响 FR-CFG-012 自锁判定）
}

type cliExecuteResponse struct {
	Output  string          `json:"output"`
	Mode    string          `json:"mode"`
	Path    []string        `json:"path"`
	Prompt  string          `json:"prompt"`
	Console *ConsoleRequest `json:"console,omitempty"` // M4-12：串口终端接管请求（FR-CMP-014）
}

// handleCLIExecute POST /api/v1/cli/execute：执行一行 CLI 命令。
// 逐命令权限在执行器内按 schema 节点判定（外层仅要求有效登录）。
func (s *Server) handleCLIExecute(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 单行命令请求，限制请求体防滥用
	var req cliExecuteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Line == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "需要 line 字段", nil)
		return
	}
	source := req.Source
	if source == "" {
		source = "ssh"
	}
	info, _ := Identity(r)
	res := s.cliExec.Execute(info.User, info.Class, source, req.Line)
	writeJSON(w, http.StatusOK, cliExecuteResponse{
		Output: res.Output, Mode: res.Mode, Path: res.Path, Prompt: res.Prompt,
		Console: res.Console,
	})
}
