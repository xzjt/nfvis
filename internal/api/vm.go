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
	// StartVMChecked 启动并在探测窗口内回读域状态（决策 #311）：正常启动返回零值 probe
	// （OK=true、无诊断字段），停在非预期态时返回诊断（域状态 + 原因 + 日志摘录 + 建议）。
	StartVMChecked(ctx context.Context, name string) (orchestrator.VMStartProbe, error)
	StopVM(ctx context.Context, name string) error
	RestartVM(ctx context.Context, name string) error
	// RefreshSeed 启动/重启前按当前配置重建 cloud-init seed（决策 #114）。
	RefreshSeed(ctx context.Context, vm model.VMFunction) error
	VMState(ctx context.Context, name string) (string, error)
}

// vmResponse VMFunction + 运行态 state（契约 GET 视图；state 为运行态字段）。
// statistics 只在详情端点附带（契约 VMFunction.statistics；列表不带，避免每台 VM 都去查计数）。
type vmResponse struct {
	model.VMFunction
	State      string `json:"state,omitempty"`
	Statistics []any  `json:"statistics,omitempty"`
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
	writeJSON(w, http.StatusOK, paginate(r, out))
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
	writeJSON(w, http.StatusOK, vmResponse{
		VMFunction: vm,
		State:      s.vmStateSafe(r.Context(), name),
		Statistics: s.vmStatistics(r.Context(), name),
	})
}

// vmStatistics 取该 VM 各 vhost-user vNIC 在 VPP 中的收发计数，与 CLI
// `show virtual-machine-functions <name> statistics` 同源（FR-CMP-011）。
// 运行态未接入、或该 VM 没有 vhost-user vNIC 时返回 nil——契约声明"非 vhost-user vNIC
// 不在其中"，故不发空数组占位（不编造）。
func (s *Server) vmStatistics(ctx context.Context, name string) []any {
	if s.state == nil {
		return nil
	}
	cfg, err := s.engine.Committed()
	if err != nil {
		return nil
	}
	vm, ok := findVM(cfg, name)
	if !ok {
		return nil
	}
	var out []any
	for _, nic := range vm.Interfaces {
		if nic.Type != "vhost-user" {
			continue
		}
		ifname := orchestrator.VnfIfaceName(name, nic.Name)
		row := map[string]any{"vnic": nic.Name, "interface": ifname}
		if c, ok := s.state.InterfaceCounters(ctx, ifname); ok {
			row["available"] = true
			row["rx_packets"], row["tx_packets"] = c.RxPackets, c.TxPackets
			row["rx_bytes"], row["tx_bytes"] = c.RxBytes, c.TxBytes
		} else {
			row["available"] = false
		}
		out = append(out, row)
	}
	return out
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
				fmt.Sprintf("VM %s 当前为 %s，vCPU/内存/vNIC 修改需先关机（热调整列 V2）", name, state), nil)
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

	user := "api"
	if info, ok := Identity(r); ok {
		user = info.User
	}
	ctx := r.Context()
	switch action {
	case "start":
		// 决策 #311：受理后回读域状态；非预期态用既有 Error 形状（message + detail[]）如实报出。
		probe, perr := s.vm.StartVMChecked(ctx, name)
		if perr != nil {
			err = perr
			break
		}
		if !probe.OK {
			s.engine.Audit(user, "vm.start", fmt.Sprintf("start VM %s: %s", name, probe.Reason), "failure")
			writeError(w, http.StatusInternalServerError, "VM_START_FAILED",
				vmStartFailureMessage(name, probe), vmStartFailureDetail(probe))
			return
		}
	case "stop":
		err = s.vm.StopVM(ctx, name)
	case "restart":
		err = s.vm.RestartVM(ctx, name)
	}
	if err != nil {
		result := "failure"
		detail := fmt.Sprintf("%s %s: %v", action, name, err)
		s.engine.Audit(user, "vm."+action, detail, result)
		if errors.Is(err, orchestrator.ErrVMNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error(), nil)
			return
		}
		// 决策 #314：数据面（VPP）不可用时的前置判定——既有 Error 形状 + 既有 503 UNAVAILABLE
		// （不新增 code、不新增响应字段）；message 已含「未启动虚拟机」与 request vpp restart 指引。
		if errors.Is(err, orchestrator.ErrDataPlaneUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", err.Error(), nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	s.engine.Audit(user, "vm."+action, fmt.Sprintf("%s VM %s", action, name), "success")
	s.publishVNFState("virtual-machine-functions", name, action+"ing") // M5-1 vnf-state-changed
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

// ---------- 启动结果回读的文案（决策 #311）----------
//
// REST 与 CLI 共用同一组装器，避免两处各写一份而漂移（CLI 不经 HTTP handler）。

// vmStartFailureMessage 一行摘要：VM 名 + 观测到的运行态 + libvirt 状态/reason + 首条建议。
func vmStartFailureMessage(name string, p orchestrator.VMStartProbe) string {
	state := p.State
	if state == "" {
		state = "未知"
	}
	msg := fmt.Sprintf("VM %s 启动后未达到运行态（%s）", name, state)
	if p.Reason != "" {
		msg += "：" + p.Reason
	}
	if len(p.Hints) > 0 {
		msg += "。" + p.Hints[0]
	}
	return msg
}

// vmStartFailureDetail 结构化明细：域状态 / libvirt 日志摘录（或「未取到」+ 路径）/ 恢复建议逐条。
func vmStartFailureDetail(p orchestrator.VMStartProbe) []ErrorDetail {
	var out []ErrorDetail
	if p.Reason != "" {
		out = append(out, ErrorDetail{Path: "vm_state", Message: p.Reason})
	}
	switch {
	case p.Detail != "":
		out = append(out, ErrorDetail{Path: "libvirt_log", Message: p.LogPath + "（末尾）：\n" + p.Detail})
	case p.LogPath != "":
		out = append(out, ErrorDetail{Path: "libvirt_log", Message: "未取到日志内容；请查看 " + p.LogPath})
	}
	for i, h := range p.Hints {
		out = append(out, ErrorDetail{Path: fmt.Sprintf("recovery_%d", i+1), Message: h})
	}
	return out
}

// vmStartFailureCLIText CLI 侧的多行失败文案（同一批事实，按终端可读排版）。
func vmStartFailureCLIText(name string, p orchestrator.VMStartProbe) string {
	var b strings.Builder
	state := p.State
	if state == "" {
		state = "未知"
	}
	fmt.Fprintf(&b, "VNF %s 启动后未达到运行态（%s）", name, state)
	if p.Reason != "" {
		b.WriteString("：" + p.Reason)
	}
	b.WriteString("\n")
	switch {
	case p.Detail != "":
		fmt.Fprintf(&b, "libvirt 日志 %s（末尾）：\n%s\n", p.LogPath, p.Detail)
	case p.LogPath != "":
		fmt.Fprintf(&b, "（未能读取 libvirt 日志 %s，请在该路径自查）\n", p.LogPath)
	}
	for _, h := range p.Hints {
		b.WriteString("建议：" + h + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
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
