package api

// M3-2：VPP 数据面状态与重启（FR-SYS-007/009）。
// 具体连接/启动配置由编排器实现，经 Options.VPP 注入，本层不 import orchestrator。

import (
	"context"
	"net/http"

	"github.com/xzjt/nfvis/internal/model"
)

// VppStatus /vpp/status 响应（契约 components/schemas/VppStatus）。
type VppStatus struct {
	Version        string `json:"version"`
	Connected      bool   `json:"connected"`
	PendingRestart bool   `json:"pending_restart"`
	LastError      string `json:"last_error,omitempty"`
}

// VppController VPP 数据面控制能力（编排器装配注入）。
type VppController interface {
	Status(vpp *model.VppConfig) VppStatus
	Restart(ctx context.Context, vpp *model.VppConfig) error
}

// handleGetVppStatus GET /api/v1/vpp/status：连接状态与 pending_restart。
func (s *Server) handleGetVppStatus(w http.ResponseWriter, r *http.Request) {
	if s.vpp == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "VPP 未接入（编排器未装配）", nil)
		return
	}
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.vpp.Status(cfg.Vpp))
}

// handlePostVppRestart POST /api/v1/vpp/restart：按 committed 配置重建并重启（FR-SYS-009）。
func (s *Server) handlePostVppRestart(w http.ResponseWriter, r *http.Request) {
	if s.vpp == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "VPP 未接入（编排器未装配）", nil)
		return
	}
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	if err := s.vpp.Restart(r.Context(), cfg.Vpp); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "restarting"})
}
