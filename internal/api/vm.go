package api

// M4-3：VM VNF 配置 CRUD 与生命周期动作（FR-CMP-010~013）。
//
// 配置 CRUD 经事务引擎（candidate/commit，契约 ConfigAccepted）；
// start/stop/restart 为运行态动作（不改变 committed 配置），由 libvirt 编排器执行
// 并记入审计（FR-OPS-031）。FR-CMP-012：运行中修改 vCPU/内存/vNIC 返回 409。
// FR-CMP-013：删除需二次确认（API `confirm=true`，CLI 交互确认）。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// VMRuntime VM 生命周期运行态能力（libvirt 编排器注入；nil = 503）。
type VMRuntime interface {
	StartVM(ctx context.Context, name string) error
	StopVM(ctx context.Context, name string) error
	RestartVM(ctx context.Context, name string) error
	VMState(ctx context.Context, name string) (string, error)
}

// vmResponse VMFunction + 运行态 state（契约 GET 视图；state 为运行态字段）。
type vmResponse struct {
	model.VMFunction
	State string `json:"state,omitempty"`
}

// handleListVMs GET /api/v1/virtual-machine-functions（FR-CMP-011 状态实时查询）。
func (s *Server) handleListVMs(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	out := make([]vmResponse, 0, len(cfg.VirtualMachineFunctions))
	for _, vm := range cfg.VirtualMachineFunctions {
		out = append(out, vmResponse{VMFunction: vm, State: s.vmStateSafe(r.Context(), vm.Name)})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetVM GET /api/v1/virtual-machine-functions/{name}。
func (s *Server) handleGetVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
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
	writeJSON(w, http.StatusOK, vmResponse{VMFunction: vm, State: s.vmStateSafe(r.Context(), name)})
}

// handlePostVM POST /api/v1/virtual-machine-functions（FR-CMP-010：定义；autostart 时启动）。
func (s *Server) handlePostVM(w http.ResponseWriter, r *http.Request) {
	var in model.VMFunction
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if in.Name == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "name 必填", nil)
		return
	}
	s.mutateCandidate(w, r, http.StatusCreated, func(cfg *model.Config) error {
		if _, ok := findVM(*cfg, in.Name); ok {
			return conflict("VM %s 已存在", in.Name)
		}
		cfg.VirtualMachineFunctions = append(cfg.VirtualMachineFunctions, in)
		return nil
	})
}

// handlePutVM PUT /api/v1/virtual-machine-functions/{name}（FR-CMP-012：关机态生效）。
// 运行中（running/paused/crashed）拒绝，返回 409 并提示先关机。
func (s *Server) handlePutVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var in model.VMFunction
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if s.vm != nil {
		state, err := s.vm.VMState(r.Context(), name)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "查询 VM 状态失败: "+err.Error(), nil)
			return
		}
		switch state {
		case orchestrator.VMStateRunning, orchestrator.VMStatePaused, orchestrator.VMStateCrashed:
			writeError(w, http.StatusConflict, "CONFLICT",
				fmt.Sprintf("VM %s 当前为 %s，vCPU/内存/vNIC 修改需先关机（FR-CMP-012，热调整列 V2）", name, state), nil)
			return
		}
	}
	in.Name = name // 路径名为准，避免请求体改名
	s.mutateCandidate(w, r, http.StatusOK, func(cfg *model.Config) error {
		idx := slices.IndexFunc(cfg.VirtualMachineFunctions, func(x model.VMFunction) bool { return x.Name == name })
		if idx < 0 {
			return conflict("VM %s 不存在", name)
		}
		cfg.VirtualMachineFunctions[idx] = in
		return nil
	})
}

// handleDeleteVM DELETE /api/v1/virtual-machine-functions/{name}?confirm=true
// （FR-CMP-013：级联 vNIC/VPP 端口/快照 + 二次确认）。
func (s *Server) handleDeleteVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !confirmTrue(r) {
		writeError(w, http.StatusBadRequest, "CONFIRM_REQUIRED",
			fmt.Sprintf("删除 VM %s 需二次确认（confirm=true）", name), nil)
		return
	}
	s.mutateCandidate(w, r, http.StatusOK, func(cfg *model.Config) error {
		idx := slices.IndexFunc(cfg.VirtualMachineFunctions, func(x model.VMFunction) bool { return x.Name == name })
		if idx < 0 {
			return conflict("VM %s 不存在", name)
		}
		cfg.VirtualMachineFunctions = slices.Delete(cfg.VirtualMachineFunctions, idx, idx+1)
		return nil
	})
}

// dispatchVMPost 分发 `/virtual-machine-functions/{tail...}` 的 POST：
// `<name>:start|stop|restart`（冒号后缀不被 ServeMux 通配符支持，故手工分发）。
func (s *Server) dispatchVMPost(w http.ResponseWriter, r *http.Request) {
	tail := r.PathValue("tail")
	name, action, ok := strings.Cut(tail, ":")
	if !ok || name == "" || action == "" {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "未知的 VM 动作路径: "+tail, nil)
		return
	}
	switch action {
	case "start", "stop", "restart":
	default:
		writeError(w, http.StatusNotFound, "NOT_FOUND", "未知的 VM 动作: "+action, nil)
		return
	}
	s.vmAction(w, r, name, action)
}

func (s *Server) vmAction(w http.ResponseWriter, r *http.Request, name, action string) {
	if s.vm == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "计算编排未接入（libvirt 未装配）", nil)
		return
	}
	// 目标必须在 committed 配置中（防止对孤儿 domain 操作）。
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	if _, ok := findVM(cfg, name); !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("VM %s 不存在", name), nil)
		return
	}

	ctx := r.Context()
	switch action {
	case "start":
		err = s.vm.StartVM(ctx, name)
	case "stop":
		err = s.vm.StopVM(ctx, name)
	case "restart":
		err = s.vm.RestartVM(ctx, name)
	}
	user := "api"
	if info, ok := Identity(r); ok {
		user = info.User
	}
	if err != nil {
		result := "failure"
		detail := fmt.Sprintf("%s %s: %v", action, name, err)
		s.engine.Audit(user, "vm."+action, detail, result)
		if errors.Is(err, orchestrator.ErrVMNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error(), nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	s.engine.Audit(user, "vm."+action, fmt.Sprintf("%s VM %s", action, name), "success")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": action + "ing", "name": name})
}

// vmStateSafe 查询运行态，未装配/出错时返回空（列表仍可用，FR-CMP-011）。
func (s *Server) vmStateSafe(ctx context.Context, name string) string {
	if s.vm == nil {
		return ""
	}
	state, err := s.vm.VMState(ctx, name)
	if err != nil {
		return ""
	}
	return state
}

func findVM(cfg model.Config, name string) (model.VMFunction, bool) {
	for _, vm := range cfg.VirtualMachineFunctions {
		if vm.Name == name {
			return vm, true
		}
	}
	return model.VMFunction{}, false
}

// confirmTrue 读取二次确认参数（query `confirm=true|1`）。
func confirmTrue(r *http.Request) bool {
	v := r.URL.Query().Get("confirm")
	return v == "true" || v == "1"
}
