package api

// 审计查询与 CLI 动态候选端点。

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/schema"
)

// handleAuditLogs GET /api/v1/audit-logs：审计日志查询（FR-CFG-010/FR-OPS-031，
// limit/offset/user 过滤，FR-API-007 分页）。
func (s *Server) handleAuditLogs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	entries, err := s.engine.AuditTrail(limit, offset)
	if err != nil {
		mapEngineError(w, err)
		return
	}
	user := r.URL.Query().Get("user")
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		if user != "" && e.User != user {
			continue
		}
		out = append(out, map[string]any{
			"timestamp": e.Time,
			"user":      e.User,
			"action":    e.Action,
			"detail":    e.Detail,
			"result":    e.Result,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCLICandidates GET /api/v1/cli/candidates?tokens=a,b&partial=x：
// CLI ?/Tab 候选（§5.1~5.3）：schema 关键字 + 动态候选（实时读 committed 配置）。
func (s *Server) handleCLICandidates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// kind 模式：仅返回指定来源的动态候选清单（供 cliclient 缓存/补全使用）
	if kind := q.Get("kind"); kind != "" {
		writeJSON(w, http.StatusOK, s.dynamicValues(kind))
		return
	}
	var tokens []string
	if q.Get("tokens") != "" {
		tokens = strings.Split(q.Get("tokens"), ",")
	}
	partial := q.Get("partial")
	root := rootForQuery(tokens)
	cs := schema.Candidates(root, tokens, partial, s.dynamicValues)
	writeJSON(w, http.StatusOK, cs)
}

// rootForQuery 依据首个 token 选择补全根（与 nfvis-cli 本地逻辑一致）。
func rootForQuery(tokens []string) *schema.Node {
	if len(tokens) > 0 {
		switch tokens[0] {
		case "set", "delete", "edit":
			return schema.ConfigRoot()
		case "run":
			return schema.OperRoot()
		}
	}
	return schema.OperRoot()
}

// dynamicValues 动态候选来源（§5.3）。接口名一族取自**运行态端口清单**（决策 #83），
// 其余从 committed 配置实时推导。
func (s *Server) dynamicValues(kind string) []string {
	// 端口清单与配置无关：先处理，避免配置读取失败时连端口候选都拿不到。
	switch kind {
	case schema.DynVppIfnames:
		// 数据面端口（= 已被 DPDK 接管的口）：只能从 VPP 侧枚举（内核中已无 netdev）。
		return s.vppIfnames()
	case schema.DynKernelIfnames:
		// 内核网卡（未被接管）：管理口 / bind-dpdk / SR-IOV PF 的名字在内核侧才成立。
		return s.kernelIfnames()
	case schema.DynIfnames:
		// 并集：`request interfaces <n> enable|bind-dpdk|unbind-dpdk` 的动作混合，
		// 参数位置在动作之前、无法按动作区分来源，故两侧都给（决策 #83）。
		return s.allIfnames()
	}
	cfg, err := s.engine.Committed()
	if err != nil {
		return nil
	}
	switch kind {
	case schema.DynVSwitches:
		out := make([]string, 0, len(cfg.VirtualSwitches))
		for _, vs := range cfg.VirtualSwitches {
			out = append(out, vs.Name)
		}
		return out
	case schema.DynVrfs:
		out := make([]string, 0, len(cfg.Vrfs))
		for _, v := range cfg.Vrfs {
			out = append(out, v.Name)
		}
		return out
	case schema.DynVMs:
		out := make([]string, 0, len(cfg.VirtualMachineFunctions))
		for _, m := range cfg.VirtualMachineFunctions {
			out = append(out, m.Name)
		}
		return out
	case schema.DynContainers:
		out := make([]string, 0, len(cfg.ContainerFunctions))
		for _, ct := range cfg.ContainerFunctions {
			out = append(out, ct.Name)
		}
		return out
	case schema.DynImages:
		// M4-12（附录 A #51）：镜像不在配置模型中，候选取自镜像仓库运行态。
		if s.images == nil {
			return nil
		}
		metas := s.images.List()
		out := make([]string, 0, len(metas))
		for _, m := range metas {
			out = append(out, m.Name)
		}
		return out
	case schema.DynClasses:
		out := []string{aaa.ClassSuperUser, aaa.ClassOperator, aaa.ClassReadOnly}
		if cfg.System != nil && cfg.System.Login != nil {
			for _, c := range cfg.System.Login.Classes {
				out = append(out, c.Name)
			}
		}
		return out
	case schema.DynAcls:
		out := make([]string, 0, len(cfg.Acls))
		for _, a := range cfg.Acls {
			out = append(out, a.Name)
		}
		return out
	case schema.DynQos:
		out := make([]string, 0, len(cfg.QosPolicies))
		for _, qp := range cfg.QosPolicies {
			out = append(out, qp.Name)
		}
		return out
	case schema.DynRevisions:
		rev, err := s.engine.CurrentRevision()
		if err != nil {
			return nil
		}
		out := make([]string, 0, rev)
		for i := rev - 1; i >= 1 && len(out) < 50; i-- {
			out = append(out, strconv.Itoa(i))
		}
		return out
	}
	return nil
}
