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
	Version        string          `json:"version"`
	Connected      bool            `json:"connected"`
	PendingRestart bool            `json:"pending_restart"`
	LastError      string          `json:"last_error,omitempty"`
	Threads        []VppThread     `json:"threads,omitempty"`
	Buffers        []VppBufferPool `json:"buffers,omitempty"`
	// BuffersSource buffer 池统计来源（statsclient|vpp_get_stats，决策 #68）。
	BuffersSource string `json:"buffers_source,omitempty"`
	// BuffersUnavailable buffer 池统计不可用的原因（有数据时省略；决策 #68）。
	BuffersUnavailable string     `json:"buffers_unavailable,omitempty"`
	Memory             *VppMemory `json:"memory,omitempty"`
}

// VppBufferPool buffer 池用量（契约）。
type VppBufferPool struct {
	Name      string  `json:"name"`
	Used      float64 `json:"used"`
	Available float64 `json:"available"`
	Cached    float64 `json:"cached,omitempty"`
}

// VppMemory 数据面内存（main heap 合计）。
type VppMemory struct {
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
	Free  uint64 `json:"free"`
}

// VppThread /vpp/status 线程项（契约 components/schemas/VppThread）。
type VppThread struct {
	ID   uint32 `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	Core uint32 `json:"core"`
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
	view := s.vpp.Status(cfg.Vpp)
	if s.state != nil {
		for _, t := range s.state.Threads(r.Context()) {
			view.Threads = append(view.Threads, VppThread{ID: t.ID, Name: t.Name, Type: t.Type, Core: t.Core})
		}
		if bufs, ok := s.state.Buffers(r.Context()); ok {
			view.BuffersSource = bufs.Source
			for _, p := range bufs.Pools {
				view.Buffers = append(view.Buffers, VppBufferPool{Name: p.Name, Used: p.Used, Available: p.Available, Cached: p.Cached})
			}
		} else if bufs.Reason != "" {
			// 不可用必须给原因，不静默省略（决策 #68）
			view.BuffersUnavailable = bufs.Reason
		}
		if mem, ok := s.state.Memory(r.Context()); ok {
			view.Memory = &VppMemory{Total: mem.Total, Used: mem.Used, Free: mem.Free}
		}
	}
	writeJSON(w, http.StatusOK, view)
}

// SRIOVSetter 设置 PF 的 VF 数量（编排器装配注入）。
type SRIOVSetter interface {
	SetVFCount(ctx context.Context, ifname string, count int) error
}

// handlePutSRIOV PUT /api/v1/interfaces/{name}/sriov（FR-NET-004）。
func (s *Server) handlePutSRIOV(w http.ResponseWriter, r *http.Request) {
	if s.sriov == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "SR-IOV 未接入（编排器未装配）", nil)
		return
	}
	var in struct {
		VFCount *int `json:"vf_count"`
	}
	if err := decodeBody(r, &in); err != nil || in.VFCount == nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "vf_count 必填", nil)
		return
	}
	name := r.PathValue("name")
	if err := s.sriov.SetVFCount(r.Context(), name, *in.VFCount); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"interface": name, "vf_count": *in.VFCount})
}

// NatSessionRow /nat/sessions 一行。
type NatSessionRow struct {
	InsideIP    string `json:"inside_ip"`
	InsidePort  int    `json:"inside_port"`
	OutsideIP   string `json:"outside_ip"`
	OutsidePort int    `json:"outside_port"`
	Protocol    int    `json:"protocol"`
	Bytes       uint64 `json:"bytes"`
	Packets     uint32 `json:"packets"`
}

// NatSessionsRuntime NAT 会话运行态（编排器装配注入）。
type NatSessionsRuntime interface {
	Sessions(ctx context.Context) ([]NatSessionRow, error)
}

// handleGetNatSessions GET /api/v1/nat/sessions（FR §4.3 运行态）。
func (s *Server) handleGetNatSessions(w http.ResponseWriter, r *http.Request) {
	if s.natSessions == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "VPP 未接入（编排器未装配）", nil)
		return
	}
	rows, err := s.natSessions.Sessions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	if rows == nil {
		rows = []NatSessionRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleGetVppConfig GET /api/v1/vpp/config：committed vpp 段（startup.conf 生成源）。
func (s *Server) handleGetVppConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	if cfg.Vpp == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	writeJSON(w, http.StatusOK, cfg.Vpp)
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

// DPDKSetter 网卡 DPDK 驱动接管（FR-NET-001，决策 #72；编排器装配注入）。
type DPDKSetter interface {
	// SetDPDKBound 绑定/解绑；返回该网卡的 PCI 地址与操作后实际绑定的驱动名。
	// bound=true：driver 为目标 DPDK 驱动（空 = vfio-pci）；
	// bound=false：driver 为交还的内核驱动（空 = 交由内核自动探测，实测常需显式给出）。
	SetDPDKBound(ctx context.Context, ifname string, bound bool, driver string) (pci, curDriver string, err error)
}

// handlePutDPDK PUT /api/v1/interfaces/{name}/dpdk：网卡 DPDK 驱动绑定/解绑（FR-NET-001）。
//
// 会中断该网卡现有流量，故要求 confirm=true（与删除类动作同口径）。
func (s *Server) handlePutDPDK(w http.ResponseWriter, r *http.Request) {
	if s.dpdk == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "DPDK 接管未接入（编排器未装配）", nil)
		return
	}
	name := r.PathValue("name")
	if r.URL.Query().Get("confirm") != "true" {
		writeError(w, http.StatusBadRequest, "CONFIRM_REQUIRED",
			"DPDK 驱动绑定会中断该网卡流量，需 confirm=true", nil)
		return
	}
	var in struct {
		Bound     *bool  `json:"bound"`
		UIODriver string `json:"uio_driver"`
		ToDriver  string `json:"to_driver"` // 解绑语义：交还的内核驱动
	}
	if err := decodeBody(r, &in); err != nil || in.Bound == nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "bound 必填", nil)
		return
	}
	driver := in.UIODriver
	if !*in.Bound {
		driver = in.ToDriver // 解绑语义复用同一参数
	}
	pci, drv, err := s.dpdk.SetDPDKBound(r.Context(), name, *in.Bound, driver)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"interface": name, "pci": pci, "driver": drv})
}
