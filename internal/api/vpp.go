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

// MACTableRow /virtual-switches/{name}/mac-table 一行（契约 mac/port/vlan）。
type MACTableRow struct {
	MAC  string `json:"mac"`
	Port string `json:"port"`
	VLAN int    `json:"vlan"`
}

// L2Runtime L2 运行态查询能力（编排器装配注入；nil = 503）。
type L2Runtime interface {
	MACTable(ctx context.Context, swName string) ([]MACTableRow, error)
}

// handleGetMacTable GET /api/v1/virtual-switches/{name}/mac-table（FR-NET-015）。
func (s *Server) handleGetMacTable(w http.ResponseWriter, r *http.Request) {
	if s.l2 == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "VPP 未接入（编排器未装配）", nil)
		return
	}
	rows, err := s.l2.MACTable(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	if rows == nil {
		rows = []MACTableRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// RouteRow /vrfs/{name}/routes 一行（契约 Route）。
type RouteRow struct {
	Prefix   string `json:"prefix"`
	NextHop  string `json:"next_hop"`
	Distance int    `json:"distance,omitempty"`
}

// L3Runtime L3 运行态查询能力（编排器装配注入；nil = 503）。
type L3Runtime interface {
	Routes(ctx context.Context, vrfName string) ([]RouteRow, error)
}

// LldpNeighborRow /protocols/lldp/neighbors 一行。
type LldpNeighborRow struct {
	Interface string  `json:"interface"`
	ChassisID string  `json:"chassis_id"`
	PortID    string  `json:"port_id"`
	TTL       int     `json:"ttl"`
	LastHeard float64 `json:"last_heard,omitempty"`
}

// LldpRuntime LLDP 运行态查询能力（编排器装配注入；nil = 503）。
type LldpRuntime interface {
	Neighbors(ctx context.Context) ([]LldpNeighborRow, error)
}

// handleGetLldpNeighbors GET /api/v1/protocols/lldp/neighbors（FR-NET-018 运行态）。
func (s *Server) handleGetLldpNeighbors(w http.ResponseWriter, r *http.Request) {
	if s.lldp == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "VPP 未接入（编排器未装配）", nil)
		return
	}
	rows, err := s.lldp.Neighbors(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	if rows == nil {
		rows = []LldpNeighborRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleGetVrfRoutes GET /api/v1/vrfs/{name}/routes：FIB 路由表（运行态，FR-NET-013）。
func (s *Server) handleGetVrfRoutes(w http.ResponseWriter, r *http.Request) {
	if s.l3 == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "VPP 未接入（编排器未装配）", nil)
		return
	}
	rows, err := s.l3.Routes(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	if rows == nil {
		rows = []RouteRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}
