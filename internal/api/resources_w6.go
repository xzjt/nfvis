package api

// W6：网络配置层 API handlers 第二组（M2 收尾任务清单 W6）。
// acls / nat / qos / port-mirroring / bonds / protocols-lldp 的配置 CRUD
//（写 candidate → commit 落库，决策 #22）。运行态与底座生效属 M3——本文件
// 不 import internal/orchestrator 实现，下发经既有 Provider 接口（mock 可运行）。

import (
	"context"
	"net/http"
	"slices"

	"github.com/xzjt/nfvis/internal/model"
)

// ---------- acls（FR：ACL 绑定校验见 FR-CFG-011 前置引用） ----------

// ACLCountersRuntime ACL 逐规则命中来源（决策 #339；编排器装配注入，nil = 运行态未接入）。
// 错误**如实上抛**——调用方呈现原因，不静默当作「无命中」。
type ACLCountersRuntime interface {
	ACLHitCounters(ctx context.Context) (map[string]map[uint32]uint64, error)
}

// aclHitsFor 取某 ACL 的逐规则命中（键 = 规则下发下标，与配置 rules 顺序同源）。
//
// 返回 (nil, nil) 表示运行态未接入（无命中来源，读视图如实不带 hits）；
// 非 nil error 表示取数失败（调用方呈现原因，不吞）。
func aclHitsFor(ctx context.Context, src ACLCountersRuntime, name string) (map[uint32]uint64, error) {
	if src == nil {
		return nil, nil
	}
	all, err := src.ACLHitCounters(ctx)
	if err != nil {
		return nil, err
	}
	return all[name], nil
}

// aclDetailView ACL 详情视图：配置对象 + 每条规则附命中数 `hits`（运行态，决策 #339）。
//
// 用 anyToTree 复用模型序列化，保证除 hits 外与既有详情逐字段一致。hits 为 nil
// （运行态未接入 / 该 ACL 无计数）时不加该字段——契约里 hits 是可选只读字段。
func aclDetailView(a model.Acl, hits map[uint32]uint64) map[string]any {
	m, _ := anyToTree(a).(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	if hits == nil {
		return m
	}
	rules, _ := m["rules"].([]any)
	for i, r := range rules {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		rm["hits"] = hits[uint32(i)]
	}
	return m
}

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
// 自决策 #339 起每条规则附运行态命中数 `hits`（来源与 CLI `show acls <name> detail` 同一
// ACLCountersRuntime）；取数失败不拖垮配置读视图，改为在 `hits_unavailable` 里如实说明原因。
func (s *Server) handleGetAcl(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	for _, a := range cfg.Acls {
		if a.Name != name {
			continue
		}
		hits, herr := aclHitsFor(r.Context(), s.aclCounters, name)
		view := aclDetailView(a, hits)
		if herr != nil {
			view["hits_unavailable"] = herr.Error()
		}
		writeJSON(w, http.StatusOK, view)
		return
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

func (s *Server) handleGetLldp(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	out := model.LldpConfig{}
	if cfg.Protocols != nil && cfg.Protocols.LLDP != nil {
		out = *cfg.Protocols.LLDP
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
