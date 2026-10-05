package api

// M4-7：容器 VNF 配置 CRUD 与生命周期（FR-CMP-020~022）。
//
// 配置 CRUD 经事务引擎（candidate/commit）；生命周期动作与日志为运行态，
// 由 Docker 编排器执行；动作入审计（FR-OPS-031）。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/orchestrator/container"
)

// ContainerRuntime 容器生命周期、日志与容器内执行（Docker 编排器注入；nil = 503）。
type ContainerRuntime interface {
	StartContainer(ctx context.Context, name string) error
	StopContainer(ctx context.Context, name string) error
	RestartContainer(ctx context.Context, name string) error
	ContainerState(ctx context.Context, name string) (string, error)
	ContainerLogs(ctx context.Context, name string, tail int) (string, error)
	// ContainerExec 在运行中的容器内执行命令（决策 #357，非交互）。
	ContainerExec(ctx context.Context, name, command string, timeout time.Duration) (container.ExecResult, error)
	// ContainerShell 打开容器内的交互式 TTY（决策 #358）；返回全双工流，Close 即关会话。
	ContainerShell(ctx context.Context, name string) (io.ReadWriteCloser, error)
}

// containerResponse ContainerFunction + 运行态 state（契约 GET 视图）。
type containerResponse struct {
	model.ContainerFunction
	State string `json:"state,omitempty"`
}

func (s *Server) ctStateSafe(ctx context.Context, name string) string {
	if s.containers == nil {
		return ""
	}
	st, err := s.containers.ContainerState(ctx, name)
	if err != nil {
		return ""
	}
	return st
}

// handleListContainers GET /api/v1/container-functions
func (s *Server) handleListContainers(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	out := make([]containerResponse, 0, len(cfg.ContainerFunctions))
	for _, ct := range cfg.ContainerFunctions {
		out = append(out, containerResponse{ContainerFunction: ct, State: s.ctStateSafe(r.Context(), ct.Name)})
	}
	writeJSON(w, http.StatusOK, paginate(r, out))
}

// handleGetContainer GET /api/v1/container-functions/{name}
func (s *Server) handleGetContainer(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	ct, ok := findContainer(cfg, name)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("容器 %s 不存在", name), nil)
		return
	}
	writeJSON(w, http.StatusOK, containerResponse{ContainerFunction: ct, State: s.ctStateSafe(r.Context(), name)})
}

// handlePostContainer POST /api/v1/container-functions（FR-CMP-020）。
func (s *Server) handlePostContainer(w http.ResponseWriter, r *http.Request) {
	var in model.ContainerFunction
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if in.Name == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "name 必填", nil)
		return
	}
	s.mutateCandidate(w, r, http.StatusCreated, func(cfg *model.Config) error {
		if _, ok := findContainer(*cfg, in.Name); ok {
			return conflict("容器 %s 已存在", in.Name)
		}
		cfg.ContainerFunctions = append(cfg.ContainerFunctions, in)
		return nil
	})
}

// handleDeleteContainer DELETE /api/v1/container-functions/{name}?confirm=true
func (s *Server) handleDeleteContainer(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !confirmTrue(r) {
		writeError(w, http.StatusBadRequest, "CONFIRM_REQUIRED",
			fmt.Sprintf("删除容器 %s 需二次确认（confirm=true）", name), nil)
		return
	}
	s.mutateCandidate(w, r, http.StatusOK, func(cfg *model.Config) error {
		idx := slices.IndexFunc(cfg.ContainerFunctions, func(x model.ContainerFunction) bool { return x.Name == name })
		if idx < 0 {
			return conflict("容器 %s 不存在", name)
		}
		cfg.ContainerFunctions = slices.Delete(cfg.ContainerFunctions, idx, idx+1)
		return nil
	})
}

// dispatchContainerPost 分发 `/container-functions/{tail...}` 的 POST：
// `<name>:start|stop|restart`（冒号后缀手工分发）。
func (s *Server) dispatchContainerPost(w http.ResponseWriter, r *http.Request) {
	tail := r.PathValue("tail")
	name, action, ok := strings.Cut(tail, ":")
	if !ok || name == "" || action == "" {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "未知的容器动作路径: "+tail, nil)
		return
	}
	switch action {
	case "start", "stop", "restart":
	case "exec":
		// 决策 #357：在运行中的容器内执行命令（非交互）。形态与生命周期动作不同
		// （带 body、回结果而非 202），单独走一条分支。
		s.containerExec(w, r, name)
		return
	default:
		writeError(w, http.StatusNotFound, "NOT_FOUND", "未知的容器动作: "+action, nil)
		return
	}
	s.containerAction(w, r, name, action)
}

// containerExec POST /container-functions/{name}:exec（决策 #357）。
//
// 权限：路由层已按 ClassSuperUser 鉴权（`request container-functions exec` 为 S 档）——
// 与 VM 串口 console 的关键差别是「console 进 guest 串口仍需 guest 凭据，而 exec 是
// **免凭据的容器内命令执行**（等价 root）」，operator 本不能创建容器，故不能经此绕过。
//
// 如实口径（决策 #366 收口 R142-10 的记账漂移）：命令跑完（哪怕非 0 退出码）⇒ 200，
// 退出码是**结果**不是失败；超时 ⇒ 504（决策原文「超时/流中断 ⇒ REST 非 2xx」，
// 不再以 200+timed_out 记成功；不带部分输出，容器内进程可能仍在运行）；
// 不存在/非运行 ⇒ 404/409。
func (s *Server) containerExec(w http.ResponseWriter, r *http.Request, name string) {
	if s.containers == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "容器编排未接入（Docker 未装配）", nil)
		return
	}
	var in struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if strings.TrimSpace(in.Command) == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "command 必填（要执行的命令）", nil)
		return
	}
	if in.TimeoutSeconds < 0 || in.TimeoutSeconds > 300 {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED",
			"timeout_seconds 须在 1..300 之间（缺省 30）", nil)
		return
	}
	timeout := 30 * time.Second
	if in.TimeoutSeconds > 0 {
		timeout = time.Duration(in.TimeoutSeconds) * time.Second
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
	res, err := s.containers.ContainerExec(r.Context(), name, in.Command, timeout)
	user := "api"
	if info, ok := Identity(r); ok {
		user = info.User
	}
	if err != nil {
		s.engine.Audit(user, "container.exec", fmt.Sprintf("exec %s: %v", name, err), "failure")
		switch {
		case errors.Is(err, orchestrator.ErrVMNotFound):
			writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error(), nil)
		case errors.Is(err, orchestrator.ErrContainerNotRunning):
			writeError(w, http.StatusConflict, "CONFLICT",
				err.Error()+"；先 request container-functions "+name+" start", nil)
		default:
			writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		}
		return
	}
	// 审计记**命令原文**（操作可追溯），不记输出（可能很大）。
	// 超时按失败记账（决策 #366）：决策 #357 原文「只有没跑完（超时/流中断/前置不满足）
	// 才按失败处理」——此前超时记 success 是实现漂移，三面（CLI/REST/审计）归一。
	if res.TimedOut {
		s.engine.Audit(user, "container.exec",
			fmt.Sprintf("exec 容器 %s: %s（超时 %s，已停止等待；容器内进程可能仍在运行）",
				name, in.Command, timeout.Round(time.Second)), "failure")
		writeError(w, http.StatusGatewayTimeout, "EXEC_TIMEOUT",
			fmt.Sprintf("命令在 %s 内未结束（已停止等待；容器内进程可能仍在运行，退出码未知）",
				timeout.Round(time.Second)), nil)
		return
	}
	exit := strconv.Itoa(res.ExitCode)
	if !res.HasExitCode {
		exit = "未知（未跑完）"
	}
	s.engine.Audit(user, "container.exec",
		fmt.Sprintf("exec 容器 %s: %s（退出码 %s）", name, in.Command, exit), "success")
	out := map[string]any{
		"stdout":      res.Stdout,
		"stderr":      res.Stderr,
		"duration_ms": res.Duration.Milliseconds(),
	}
	if res.HasExitCode {
		out["exit_code"] = res.ExitCode
	}
	if res.Truncated {
		out["truncated"] = true
	}
	// 决策 #370（R142 B9）：命令已跑完但退出码读不到 ⇒ 输出照常、如实说明（不谎报 0）。
	if res.ExitCodeNote != "" {
		out["exit_code_note"] = res.ExitCodeNote
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) containerAction(w http.ResponseWriter, r *http.Request, name, action string) {
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
	switch action {
	case "start":
		err = s.containers.StartContainer(r.Context(), name)
	case "stop":
		err = s.containers.StopContainer(r.Context(), name)
	case "restart":
		err = s.containers.RestartContainer(r.Context(), name)
	}
	user := "api"
	if info, ok := Identity(r); ok {
		user = info.User
	}
	if err != nil {
		s.engine.Audit(user, "container."+action, fmt.Sprintf("%s %s: %v", action, name, err), "failure")
		if errors.Is(err, orchestrator.ErrVMNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error(), nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	s.engine.Audit(user, "container."+action, fmt.Sprintf("%s 容器 %s", action, name), "success")
	s.publishVNFState("container-functions", name, action+"ing") // M5-1 vnf-state-changed
	writeJSON(w, http.StatusAccepted, map[string]string{"status": action + "ing", "name": name})
}

// handleContainerLogs GET /api/v1/container-functions/{name}/logs?last=N（text/plain）。
func (s *Server) handleContainerLogs(w http.ResponseWriter, r *http.Request) {
	if s.containers == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "容器编排未接入（Docker 未装配）", nil)
		return
	}
	name := r.PathValue("name")
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	if _, ok := findContainer(cfg, name); !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("容器 %s 不存在", name), nil)
		return
	}
	last := 100
	if v := r.URL.Query().Get("last"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil {
			last = n
		}
	}
	out, err := s.containers.ContainerLogs(r.Context(), name, last)
	if err != nil {
		if errors.Is(err, orchestrator.ErrVMNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error(), nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(out))
}

func findContainer(cfg model.Config, name string) (model.ContainerFunction, bool) {
	for _, ct := range cfg.ContainerFunctions {
		if ct.Name == name {
			return ct, true
		}
	}
	return model.ContainerFunction{}, false
}
