package api

// W6：网络配置层 API handlers 第二组（M2 收尾任务清单 W6）。
// acls / nat / qos / port-mirroring / bonds / protocols-lldp 的配置 CRUD
//（写 candidate → commit 落库，决策 #22）。运行态与底座生效属 M3——本文件
// 不 import internal/orchestrator 实现，下发经既有 Provider 接口（mock 可运行）。

import (
	"net/http"
	"slices"

	"github.com/xzjt/nfvis/internal/model"
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
	writeJSON(w, http.StatusOK, out)
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

func (s *Server) handleGetQosPolicies(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	out := cfg.QosPolicies
	if out == nil {
		out = []model.QosPolicy{}
	}
	writeJSON(w, http.StatusOK, out)
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
				return conflict("限速策略 %s 被接口 %s 绑定，先解除引用", name, i.Name)
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
	writeJSON(w, http.StatusOK, out)
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
	writeJSON(w, http.StatusOK, out)
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
