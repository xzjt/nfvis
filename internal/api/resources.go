package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"

	"github.com/xzjt/nfvis/internal/model"
)

// 资源 CRUD handlers 第一组（OpenAPI 附录 B 映射：system/interfaces/
// virtual-switches/vrfs）。
//
// 语义对齐 CLI：GET 读 committed（show 视图，运行态字段待 state 模块接入）；
// 写操作按决策 #22 默认写 candidate，X-NFVIS-Auto-Commit: true 时直提；
// 响应为 ConfigAccepted（CommitResult + X-NFVIS-Committed 头）。

// conflictError 资源冲突（已存在/被引用/不存在 → 409，对齐 OpenAPI Error409）。
type conflictError string

func (e conflictError) Error() string { return string(e) }

func conflict(format string, args ...any) error { return conflictError(sprintf(format, args...)) }

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// mutateCandidate 在调用者会话的 candidate 上执行变更并按需直提。
// status 为写入成功状态码（POST 201 / PUT·DELETE 200）。
func (s *Server) mutateCandidate(w http.ResponseWriter, r *http.Request, status int, mutate func(*model.Config) error) {
	sess := sessionFromIdentity(r)
	if err := s.engine.Edit(sess); err != nil {
		mapEngineError(w, err)
		return
	}
	cfg, _, err := s.engine.Candidate()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	if err := mutate(&cfg); err != nil {
		var ce conflictError
		if errors.As(err, &ce) {
			writeError(w, http.StatusConflict, "CONFLICT", err.Error(), nil)
			return
		}
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if err := s.engine.UpdateCandidate(sess, cfg); err != nil {
		mapEngineError(w, err)
		return
	}
	if r.Header.Get("X-NFVIS-Auto-Commit") == "true" {
		res, err := s.engine.Commit(r.Context(), sess, CommitOptsFrom(r))
		if err != nil {
			mapEngineError(w, err)
			return
		}
		w.Header().Set("X-NFVIS-Committed", "true")
		writeJSON(w, status, commitResponseOf(res))
		return
	}
	rev, err := s.engine.CurrentRevision()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	w.Header().Set("X-NFVIS-Committed", "false")
	writeJSON(w, status, map[string]any{"committed": false, "revision": rev})
}

func decodeBody(r *http.Request, v any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return errors.New("读取请求体失败")
	}
	if err := json.Unmarshal(data, v); err != nil {
		return errors.New("JSON 不合法: " + err.Error())
	}
	return nil
}

// ---------- system（FR-SYS-001/006） ----------

// handleGetSystem GET /api/v1/system：读取系统配置（committed）。
//
// 与其它配置视图同口径脱敏（redactView；实现与规则自决策 #149 起在 internal/model）：
// `system.login.users[].password_hash` 永不回显（决策 #25）。此前这里直接序列化
// model.SystemConfig，成为**唯一**绕过脱敏的配置出口——2026-09-23 真机实测该响应
// 只有 166 字节却含 `"password_hash": "pbkdf2$…"`（全路径守护没覆盖这条路径，故长期未红）。
func (s *Server) handleGetSystem(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	out := model.SystemConfig{}
	if cfg.System != nil {
		out = *cfg.System
	}
	writeJSON(w, http.StatusOK, redactView(out))
}

// handlePutSystem PUT /api/v1/system：修改系统配置（整体替换 system 节）。
func (s *Server) handlePutSystem(w http.ResponseWriter, r *http.Request) {
	var in model.SystemConfig
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	s.mutateCandidate(w, r, http.StatusOK, func(cfg *model.Config) error {
		if cfg.System == nil {
			cfg.System = &model.SystemConfig{}
		}
		in.Login = cfg.System.Login // login 由专用端点管理，整体替换不误伤
		*cfg.System = in
		return nil
	})
}

// ---------- interfaces（FR-NET-003/004，CLI §2.3） ----------

// handleGetInterfaces GET /api/v1/interfaces：接口视图列表（配置 + 运行态，committed）。
func (s *Server) handleGetInterfaces(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, paginate(r, s.interfaceViews(cfg)))
}

// interfaceViews 接口列表视图：配置字段 + **运行态**字段（决策 #116）。
//
// 契约的 `Interface` 同时声明了配置字段（mtu/description/sriov）与运行态字段
// （kind/driver/enabled/link/speed_mbps/mac/numa_node），而此前实现只回配置对象——
// 照契约开发的客户端拿不到任何运行态。这里补运行态，**来源与 CLI `show interfaces physical`
// 同源**（`VppStateRuntime.InterfaceStates()`，决策 #84 定的运行态事实来源）；
// 取不到的字段（mac/numa_node 等）**不给**，不编造。
func (s *Server) interfaceViews(cfg model.Config) []map[string]any {
	out := make([]map[string]any, 0, len(cfg.Interfaces))
	states := map[string]InterfaceState{}
	if s.vppState != nil {
		if st, err := s.vppState.InterfaceStates(); err == nil {
			states = st
		}
	}
	for _, ifc := range cfg.Interfaces {
		// 先取配置对象的 JSON 形态再加运行态字段，避免字段名两处各写一份。
		b, _ := json.Marshal(ifc)
		m := map[string]any{}
		if json.Unmarshal(b, &m) != nil {
			m = map[string]any{}
		}
		if m["name"] == nil {
			m["name"] = ifc.Name
		}
		// cfg.Interfaces 就是产品模型里的**物理业务口**（与 `show interfaces physical` 同口径）。
		m["kind"] = "physical"
		if st, ok := states[ifc.Name]; ok {
			m["enabled"] = st.AdminUp
			m["link"] = "down"
			if st.LinkUp {
				m["link"] = "up"
			}
			if st.LinkSpeed > 0 { // kbps → Mbps；DPDK 口可能为 0，取不到就不给
				m["speed_mbps"] = st.LinkSpeed / 1000
			}
			if st.DevType != "" {
				m["driver"] = st.DevType
			}
		}
		out = append(out, m)
	}
	return out
}

// handleGetInterface GET /api/v1/interfaces/{name}。
func (s *Server) handleGetInterface(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	for _, i := range cfg.Interfaces {
		if i.Name == name {
			// 契约 Interface.statistics：运行态可用时附带（M3-7）
			if s.state != nil {
				if st, ok := s.state.InterfaceCounters(r.Context(), name); ok {
					b, _ := json.Marshal(i)
					var m map[string]any
					if json.Unmarshal(b, &m) == nil {
						m["statistics"] = st
						writeJSON(w, http.StatusOK, m)
						return
					}
				}
			}
			writeJSON(w, http.StatusOK, i)
			return
		}
	}
	writeError(w, http.StatusNotFound, "NOT_FOUND", "接口 "+name+" 未配置", nil)
}

// handlePutInterface PUT /api/v1/interfaces/{name}：upsert 接口配置。
func (s *Server) handlePutInterface(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var in model.InterfaceConfig
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	in.Name = name // 名称以路径为准
	s.mutateCandidate(w, r, http.StatusOK, func(cfg *model.Config) error {
		for i := range cfg.Interfaces {
			if cfg.Interfaces[i].Name == name {
				cfg.Interfaces[i] = in
				return nil
			}
		}
		cfg.Interfaces = append(cfg.Interfaces, in)
		return nil
	})
}

// ---------- virtual-switches（FR-NET-010~015，CLI §2.4） ----------

// handleGetVSwitches GET /api/v1/virtual-switches。
func (s *Server) handleGetVSwitches(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	out := cfg.VirtualSwitches
	if out == nil {
		out = []model.VirtualSwitch{}
	}
	writeJSON(w, http.StatusOK, paginate(r, out))
}

// handleGetVSwitch GET /api/v1/virtual-switches/{name}。
func (s *Server) handleGetVSwitch(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	for _, vs := range cfg.VirtualSwitches {
		if vs.Name == name {
			// 契约 VirtualSwitch.statistics：运行态可用且该交换机在数据面时附带（FR-NET-016）
			if st, ok := s.vswitchStatistics(r.Context(), name); ok {
				b, _ := json.Marshal(vs)
				var m map[string]any
				if json.Unmarshal(b, &m) == nil {
					m["statistics"] = st
					writeJSON(w, http.StatusOK, m)
					return
				}
			}
			writeJSON(w, http.StatusOK, vs)
			return
		}
	}
	writeError(w, http.StatusNotFound, "NOT_FOUND", "虚拟交换机 "+name+" 不存在", nil)
}

// vswitchStatistics 取该交换机在 VPP 中的成员口收发计数，与 CLI
// `show virtual-switches <name> statistics` 同源（BD 取 VppStateRuntime、计数取 state）。
// 运行态未接入、或该交换机不在数据面时返回 false——**不退回配置视图**（决策 #84 的口径）。
func (s *Server) vswitchStatistics(ctx context.Context, name string) (map[string]any, bool) {
	if s.vppState == nil {
		return nil, false
	}
	bds, err := s.vppState.BridgeDomains()
	if err != nil {
		return nil, false
	}
	var bd *BridgeDomainState
	for i := range bds {
		if bds[i].Name == name {
			bd = &bds[i]
			break
		}
	}
	if bd == nil {
		return nil, false
	}
	states, _ := s.vppState.InterfaceStates()
	ports := make([]any, 0, len(bd.Ports))
	for _, p := range bd.Ports {
		row := map[string]any{"port": p.Name, "sw_if_index": p.SwIfIndex}
		if st, ok := states[p.Name]; ok {
			row["admin"], row["link"] = st.AdminUp, st.LinkUp
		}
		if s.state != nil {
			if c, ok := s.state.InterfaceCounters(ctx, p.Name); ok {
				row["rx_packets"], row["tx_packets"] = c.RxPackets, c.TxPackets
			}
		}
		ports = append(ports, row)
	}
	return map[string]any{"bd_id": bd.ID, "ports": ports}, true
}

// handlePostVSwitch POST /api/v1/virtual-switches：创建（重名 409）。
func (s *Server) handlePostVSwitch(w http.ResponseWriter, r *http.Request) {
	var in model.VirtualSwitch
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if in.Name == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "name 必填", nil)
		return
	}
	s.mutateCandidate(w, r, http.StatusCreated, func(cfg *model.Config) error {
		for _, vs := range cfg.VirtualSwitches {
			if vs.Name == in.Name {
				return conflict("虚拟交换机 %s 已存在", in.Name)
			}
		}
		cfg.VirtualSwitches = append(cfg.VirtualSwitches, in)
		return nil
	})
}

// handleDeleteVSwitch DELETE /api/v1/virtual-switches/{name}：
// 被引用时 409（FR-NET-015 删除前校验）。
func (s *Server) handleDeleteVSwitch(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.mutateCandidate(w, r, http.StatusOK, func(cfg *model.Config) error {
		idx := slices.IndexFunc(cfg.VirtualSwitches, func(vs model.VirtualSwitch) bool { return vs.Name == name })
		if idx < 0 {
			return conflict("虚拟交换机 %s 不存在", name)
		}
		// FR-NET-015：VNF/容器 vNIC 引用检查
		for _, vm := range cfg.VirtualMachineFunctions {
			for _, nic := range vm.Interfaces {
				if nic.VirtualSwitch == name {
					return conflict("虚拟交换机 %s 被 VNF %s 的 vNIC %s 引用，先解除引用", name, vm.Name, nic.Name)
				}
			}
		}
		for _, ct := range cfg.ContainerFunctions {
			for _, nic := range ct.Interfaces {
				if nic.VirtualSwitch == name {
					return conflict("虚拟交换机 %s 被容器 %s 的 vNIC %s 引用，先解除引用", name, ct.Name, nic.Name)
				}
			}
		}
		// L3 交换机（type=l3）与同名 VRF 条目互为映射（附录 B），一并删除
		cfg.VirtualSwitches = slices.Delete(cfg.VirtualSwitches, idx, idx+1)
		cfg.Vrfs = slices.DeleteFunc(cfg.Vrfs, func(v model.Vrf) bool { return v.Name == name })
		return nil
	})
}

// handleGetVSwitchPorts GET /api/v1/virtual-switches/{name}/ports。
func (s *Server) handleGetVSwitchPorts(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	for _, vs := range cfg.VirtualSwitches {
		if vs.Name == name {
			ports := vs.Ports
			if ports == nil {
				ports = []model.VSwitchPort{}
			}
			writeJSON(w, http.StatusOK, ports)
			return
		}
	}
	writeError(w, http.StatusNotFound, "NOT_FOUND", "虚拟交换机 "+name+" 不存在", nil)
}

// handlePutVSwitchPorts PUT /api/v1/virtual-switches/{name}/ports：全量替换成员端口。
func (s *Server) handlePutVSwitchPorts(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var ports []model.VSwitchPort
	if err := decodeBody(r, &ports); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	s.mutateCandidate(w, r, http.StatusOK, func(cfg *model.Config) error {
		idx := slices.IndexFunc(cfg.VirtualSwitches, func(vs model.VirtualSwitch) bool { return vs.Name == name })
		if idx < 0 {
			return conflict("虚拟交换机 %s 不存在", name)
		}
		cfg.VirtualSwitches[idx].Ports = ports
		return nil
	})
}

// ---------- vrfs（FR-NET-013，L3 交换机配置数据，附录 B） ----------

// handleGetVrfs GET /api/v1/vrfs。
func (s *Server) handleGetVrfs(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	out := cfg.Vrfs
	if out == nil {
		out = []model.Vrf{}
	}
	writeJSON(w, http.StatusOK, paginate(r, out))
}

// handleGetVrf GET /api/v1/vrfs/{name}。
func (s *Server) handleGetVrf(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	for _, v := range cfg.Vrfs {
		if v.Name == name {
			writeJSON(w, http.StatusOK, v)
			return
		}
	}
	writeError(w, http.StatusNotFound, "NOT_FOUND", "VRF "+name+" 不存在", nil)
}

// handlePostVrf POST /api/v1/vrfs：创建（重名 409；同名 L3 交换机视为同一实体）。
func (s *Server) handlePostVrf(w http.ResponseWriter, r *http.Request) {
	var in model.Vrf
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if in.Name == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "name 必填", nil)
		return
	}
	s.mutateCandidate(w, r, http.StatusCreated, func(cfg *model.Config) error {
		for _, v := range cfg.Vrfs {
			if v.Name == in.Name {
				return conflict("VRF %s 已存在", in.Name)
			}
		}
		for _, vs := range cfg.VirtualSwitches {
			if vs.Name == in.Name {
				return conflict("VRF %s 与虚拟交换机重名（L3 虚拟交换机会映射为同名 VRF，请改用别的名字）", in.Name)
			}
		}
		cfg.Vrfs = append(cfg.Vrfs, in)
		return nil
	})
}

// handleDeleteVrf DELETE /api/v1/vrfs/{name}：被引用时 409。
func (s *Server) handleDeleteVrf(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.mutateCandidate(w, r, http.StatusOK, func(cfg *model.Config) error {
		idx := slices.IndexFunc(cfg.Vrfs, func(v model.Vrf) bool { return v.Name == name })
		if idx < 0 {
			return conflict("VRF %s 不存在", name)
		}
		// L3 交换机的同名校验（type=l3 交换机须有同名 VRF，决策 #24）
		for _, vs := range cfg.VirtualSwitches {
			if vs.Type == "l3" && vs.Name == name {
				return conflict("VRF %s 是 L3 交换机的配置数据，先删除对应交换机", name)
			}
		}
		// 引用检查：NAT 规则 / L2 网关显式 VRF
		if cfg.Nat != nil {
			for _, rule := range cfg.Nat.Rules {
				if rule.VirtualSwitch == name {
					return conflict("VRF %s 被 NAT 规则引用，先解除引用", name)
				}
			}
		}
		for _, vs := range cfg.VirtualSwitches {
			if vs.Gateway != nil && vs.Gateway.Vrf == name {
				return conflict("VRF %s 被虚拟交换机 %s 的网关引用，先解除引用", name, vs.Name)
			}
		}
		cfg.Vrfs = slices.Delete(cfg.Vrfs, idx, idx+1)
		return nil
	})
}

// handlePutVrfRoutes PUT /api/v1/vrfs/{name}/routes：全量替换静态路由（FR-NET-013）。
func (s *Server) handlePutVrfRoutes(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var routes []model.Route
	if err := decodeBody(r, &routes); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	s.mutateCandidate(w, r, http.StatusOK, func(cfg *model.Config) error {
		idx := slices.IndexFunc(cfg.Vrfs, func(v model.Vrf) bool { return v.Name == name })
		if idx < 0 {
			return conflict("VRF %s 不存在", name)
		}
		cfg.Vrfs[idx].Routes = routes
		return nil
	})
}
