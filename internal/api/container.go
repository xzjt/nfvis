package api

// M4-7：容器 VNF 配置 CRUD 与生命周期（FR-CMP-020~022）。
//
// 配置 CRUD 经事务引擎（candidate/commit）；生命周期动作与日志为运行态，
// 由 Docker 编排器执行；动作入审计（FR-OPS-031）。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// ContainerRuntime 容器生命周期与日志（Docker 编排器注入；nil = 503）。
type ContainerRuntime interface {
	StartContainer(ctx context.Context, name string) error
	StopContainer(ctx context.Context, name string) error
	RestartContainer(ctx context.Context, name string) error
	ContainerState(ctx context.Context, name string) (string, error)
	ContainerLogs(ctx context.Context, name string, tail int) (string, error)
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
	writeJSON(w, http.StatusOK, out)
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
	default:
		writeError(w, http.StatusNotFound, "NOT_FOUND", "未知的容器动作: "+action, nil)
		return
	}
	s.containerAction(w, r, name, action)
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
