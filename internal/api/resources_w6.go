package api

// W6：网络配置层 API handlers 第二组（M2 收尾任务清单 W6）。
// acls / nat / qos / port-mirroring / bonds / protocols-lldp 的配置 CRUD
//（写 candidate → commit 落库，决策 #22）。运行态与底座生效属 M3——本文件
// 不 import internal/orchestrator 实现，下发经既有 Provider 接口（mock 可运行）。

import (
	"net/http"
	"slices"
	"sort"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// ---------- acls（FR：ACL 绑定校验见 FR-CFG-011 前置引用） ----------

func (s *Server) handleGetAcls(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	out := cfg.Acls
	if out == nil {
		out = []model.Acl{}
	}
	writeJSON(w, http.StatusOK, paginate(r, out))
}

// handleGetAcl GET /api/v1/acls/{name}：ACL 详情（T0-1；契约 /acls/{name} get）。
func (s *Server) handleGetAcl(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	for _, a := range cfg.Acls {
		if a.Name == name {
			writeJSON(w, http.StatusOK, a)
			return
		}
	}
	writeError(w, http.StatusNotFound, "NOT_FOUND", "ACL "+name+" 不存在", nil)
}

func (s *Server) handlePostAcl(w http.ResponseWriter, r *http.Request) {
	var in model.Acl
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if in.Name == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "name 必填", nil)
		return
	}
	s.mutateCandidate(w, r, http.StatusCreated, func(cfg *model.Config) error {
		if slices.ContainsFunc(cfg.Acls, func(a model.Acl) bool { return a.Name == in.Name }) {
			return conflict("ACL %s 已存在", in.Name)
		}
		cfg.Acls = append(cfg.Acls, in)
		return nil
	})
}

func (s *Server) handleDeleteAcl(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.mutateCandidate(w, r, http.StatusOK, func(cfg *model.Config) error {
		idx := slices.IndexFunc(cfg.Acls, func(a model.Acl) bool { return a.Name == name })
		if idx < 0 {
			return conflict("ACL %s 不存在", name)
		}
		// 绑定引用检查（端口/网关/L3 接口的 acl_in/acl_out）
		for _, vs := range cfg.VirtualSwitches {
			if vs.Gateway != nil && (vs.Gateway.AclIn == name || vs.Gateway.AclOut == name) {
				return conflict("ACL %s 被交换机 %s 的网关引用，先解除引用", name, vs.Name)
			}
			for _, p := range vs.Ports {
				if p.AclIn == name || p.AclOut == name {
					return conflict("ACL %s 被交换机 %s 的端口引用，先解除引用", name, vs.Name)
				}
			}
		}
		for _, v := range cfg.Vrfs {
			for _, li := range v.L3Interfaces {
				if li.AclIn == name {
					return conflict("ACL %s 被 VRF %s 的 L3 接口引用，先解除引用", name, v.Name)
				}
			}
		}
		cfg.Acls = slices.Delete(cfg.Acls, idx, idx+1)
		return nil
	})
}

// vrfOf 取同名 Vrf（附录 B 映射；无则零值）。
func vrfOf(cfg *model.Config, name string) model.Vrf {
	for _, v := range cfg.Vrfs {
		if v.Name == name {
			return v
		}
	}
	return model.Vrf{}
}

// ---------- nat（整体替换，FR：NAT44 仅作用于 L3 交换机） ----------

func (s *Server) handleGetNat(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	out := model.NatConfig{}
	if cfg.Nat != nil {
		out = *cfg.Nat
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePutNat(w http.ResponseWriter, r *http.Request) {
	var in model.NatConfig
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	s.mutateCandidate(w, r, http.StatusOK, func(cfg *model.Config) error {
		nat := in
		cfg.Nat = &nat
		return nil
	})
}

// ---------- qos/policies ----------

// qosBinding 一条方向绑定（决策 #331）：策略被哪个接口、以哪个方向引用。
type qosBinding struct {
	Interface string `json:"interface"`
	Direction string `json:"direction"`
}

// qosPolicyViews 限速策略读视图：配置字段 + **绑定关系**（决策 #331）。
// 绑定取自 committed 的接口声明（`interfaces[].ingress_policy|egress_policy`），
// 两向一并呈现——单看策略对象看不出方向绑定，这正是出向落地后要补的读视图。
// `bound_interfaces` 给去重后的接口名（兼容既有字段），`bindings` 逐条带方向。
func qosPolicyViews(cfg model.Config) []map[string]any {
	out := make([]map[string]any, 0, len(cfg.QosPolicies))
	for _, q := range cfg.QosPolicies {
		names := []string{}
		bindings := []qosBinding{}
		seen := map[string]bool{}
		for _, i := range cfg.Interfaces {
			for _, b := range []struct{ pol, dir string }{
				{i.IngressPolicy, "ingress"}, {i.EgressPolicy, "egress"},
			} {
				if b.pol != q.Name {
					continue
				}
				bindings = append(bindings, qosBinding{Interface: i.Name, Direction: b.dir})
				if !seen[i.Name] {
					seen[i.Name] = true
					names = append(names, i.Name)
				}
			}
		}
		m := map[string]any{"name": q.Name, "cir": q.Cir, "cbs": q.Cbs,
			"bound_interfaces": names, "bindings": bindings}
		out = append(out, m)
	}
	return out
}

func (s *Server) handleGetQosPolicies(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, paginate(r, qosPolicyViews(cfg)))
}

func (s *Server) handlePostQosPolicy(w http.ResponseWriter, r *http.Request) {
	var in model.QosPolicy
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if in.Name == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "name 必填", nil)
		return
	}
	s.mutateCandidate(w, r, http.StatusCreated, func(cfg *model.Config) error {
		if slices.ContainsFunc(cfg.QosPolicies, func(q model.QosPolicy) bool { return q.Name == in.Name }) {
			return conflict("限速策略 %s 已存在", in.Name)
		}
		cfg.QosPolicies = append(cfg.QosPolicies, in)
		return nil
	})
}

func (s *Server) handleDeleteQosPolicy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.mutateCandidate(w, r, http.StatusOK, func(cfg *model.Config) error {
		idx := slices.IndexFunc(cfg.QosPolicies, func(q model.QosPolicy) bool { return q.Name == name })
		if idx < 0 {
			return conflict("限速策略 %s 不存在", name)
		}
		for _, i := range cfg.Interfaces {
			if i.IngressPolicy == name {
				return conflict("限速策略 %s 被接口 %s 的入向绑定，先解除引用", name, i.Name)
			}
			if i.EgressPolicy == name {
				return conflict("限速策略 %s 被接口 %s 的出向绑定，先解除引用", name, i.Name)
			}
		}
		cfg.QosPolicies = slices.Delete(cfg.QosPolicies, idx, idx+1)
		return nil
	})
}

// ---------- port-mirroring ----------

func (s *Server) handleGetPMs(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	out := cfg.PortMirroring
	if out == nil {
		out = []model.PortMirroring{}
	}
	writeJSON(w, http.StatusOK, paginate(r, out))
}

func (s *Server) handlePostPM(w http.ResponseWriter, r *http.Request) {
	var in model.PortMirroring
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if in.Name == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "name 必填", nil)
		return
	}
	s.mutateCandidate(w, r, http.StatusCreated, func(cfg *model.Config) error {
		if slices.ContainsFunc(cfg.PortMirroring, func(p model.PortMirroring) bool { return p.Name == in.Name }) {
			return conflict("镜像会话 %s 已存在", in.Name)
		}
		cfg.PortMirroring = append(cfg.PortMirroring, in)
		return nil
	})
}

func (s *Server) handleDeletePM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.mutateCandidate(w, r, http.StatusOK, func(cfg *model.Config) error {
		idx := slices.IndexFunc(cfg.PortMirroring, func(p model.PortMirroring) bool { return p.Name == name })
		if idx < 0 {
			return conflict("镜像会话 %s 不存在", name)
		}
		cfg.PortMirroring = slices.Delete(cfg.PortMirroring, idx, idx+1)
		return nil
	})
}

// ---------- bonds（FR-NET-017） ----------

func (s *Server) handleGetBonds(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	out := cfg.Bonds
	if out == nil {
		out = []model.Bond{}
	}
	writeJSON(w, http.StatusOK, paginate(r, out))
}

// handleGetBond GET /api/v1/bonds/{name}：bond 详情（T0-1；契约 /bonds/{name} get）。
// 运行态 LACP actor/partner 属 M3，此处先返回配置视图。
func (s *Server) handleGetBond(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	for _, b := range cfg.Bonds {
		if b.Name == name {
			writeJSON(w, http.StatusOK, b)
			return
		}
	}
	writeError(w, http.StatusNotFound, "NOT_FOUND", "bond "+name+" 不存在", nil)
}

func (s *Server) handlePostBond(w http.ResponseWriter, r *http.Request) {
	var in model.Bond
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if in.Name == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "name 必填", nil)
		return
	}
	s.mutateCandidate(w, r, http.StatusCreated, func(cfg *model.Config) error {
		if slices.ContainsFunc(cfg.Bonds, func(b model.Bond) bool { return b.Name == in.Name }) {
			return conflict("bond %s 已存在", in.Name)
		}
		cfg.Bonds = append(cfg.Bonds, in)
		return nil
	})
}

func (s *Server) handleDeleteBond(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.mutateCandidate(w, r, http.StatusOK, func(cfg *model.Config) error {
		idx := slices.IndexFunc(cfg.Bonds, func(b model.Bond) bool { return b.Name == name })
		if idx < 0 {
			return conflict("bond %s 不存在", name)
		}
		// bond 名可在一切接受接口名处引用（FR-NET-017）：交换机端口/L3 接口
		for _, vs := range cfg.VirtualSwitches {
			for _, p := range vs.Ports {
				if p.Interface == name {
					return conflict("bond %s 被交换机 %s 的端口引用，先解除引用", name, vs.Name)
				}
			}
		}
		for _, v := range cfg.Vrfs {
			for _, li := range v.L3Interfaces {
				if li.Interface == name {
					return conflict("bond %s 被 VRF %s 的 L3 接口引用，先解除引用", name, v.Name)
				}
			}
		}
		cfg.Bonds = slices.Delete(cfg.Bonds, idx, idx+1)
		return nil
	})
}

// ---------- protocols/lldp（FR-NET-018，附录 B：/protocols/lldp） ----------

// LLDPStateRuntime 内核数据面 LLDP 收发代理的运行态读物（Options.LLDPState；nil = 读视图
// 不出现运行态块，VPP 数据面恒如此）。ok=false = 没有可报的运行态（从未装配），读视图据此
// 省略运行态块——不编造（与 DNSProxyRuntime 同形，决策 #451）。
type LLDPStateRuntime interface {
	LLDPState() (network.LLDPState, bool)
}

// lldpView LLDP 读视图（GET /protocols/lldp）：配置声明 + **实际在发广告的口** + （内核侧）运行态。
//
// advertising 如实列出「实际在发广告的接口」：全局 `enable true` 时是**全部已声明物理口**按
// 逐口覆盖后的结果（决策 #451）；全局 false 时只有逐口声明 true 的口。VPP 侧为配置派生值；
// 内核侧另附 runtime（每口的就绪/未收敛原因，不谎报）。
type lldpView struct {
	model.LldpConfig
	Advertising []string           `json:"advertising"`
	Runtime     *network.LLDPState `json:"runtime,omitempty"`
}

// lldpAdvertising 实际在发广告的接口清单（配置派生；与数据面启用集同口径）。
func lldpAdvertising(cfg model.Config) []string {
	on := map[string]bool{}
	var lldp *model.LldpConfig
	if cfg.Protocols != nil {
		lldp = cfg.Protocols.LLDP
	}
	if lldp != nil && lldp.Enabled {
		for _, ifc := range cfg.Interfaces {
			if ifc.Name != "" {
				on[ifc.Name] = true
			}
		}
	}
	if lldp != nil {
		for _, li := range lldp.Interfaces {
			if li.Interface == "" {
				continue
			}
			if li.Enabled {
				on[li.Interface] = true
			} else {
				delete(on, li.Interface) // 逐口 false 覆盖全局默认
			}
		}
	}
	out := make([]string, 0, len(on))
	for n := range on {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (s *Server) handleGetLldp(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	out := lldpView{Advertising: lldpAdvertising(cfg)}
	if cfg.Protocols != nil && cfg.Protocols.LLDP != nil {
		out.LldpConfig = *cfg.Protocols.LLDP
	}
	// 运行态块（决策 #451）：内核数据面下把「每个在发广告的口」的就绪/未收敛原因如实给出
	// （读物注入、可读时出现；VPP 数据面/读不到 ⇒ 字段缺席，不发 null）。运行态可读时
	// **以运行态为准**给出 advertising（只算已收敛的口）——不谎报「在发广告」。
	if s.lldpState != nil {
		if st, ok := s.lldpState.LLDPState(); ok {
			out.Runtime = &st
			adv := make([]string, 0, len(st.Interface))
			for _, i := range st.Interface {
				if i.Converged {
					adv = append(adv, i.Name)
				}
			}
			sort.Strings(adv)
			out.Advertising = adv
		}
	}
	// 注意：这是 LLDP **配置对象**（非列表），不加分页；邻居列表在 handleGetLldpNeighbors。
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePutLldp(w http.ResponseWriter, r *http.Request) {
	var in model.LldpConfig
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	s.mutateCandidate(w, r, http.StatusOK, func(cfg *model.Config) error {
		lldp := in
		if cfg.Protocols == nil {
			cfg.Protocols = &model.ProtocolsConfig{}
		}
		cfg.Protocols.LLDP = &lldp
		return nil
	})
}
