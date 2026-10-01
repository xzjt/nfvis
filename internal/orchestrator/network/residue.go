package network

// 残渣对账（决策 #321）：把「数据面存在、配置未声明」的对象当成一条可复查的事实。
//
// 由来（round87 §8 登记）：提交失败后的补偿若也失败，残渣此前只进当次提交输出与提交期告警
// （COMMIT_COMPENSATION_FAILED，进程内、重启即丢）；#192 只为 **IP 表**这一种残渣加了
// 「按数据面事实对账」的可见化与自动消解——非 VRF 表类（ACL、bridge-domain）没有任何
// 消解路径，重启后也无从查证。
//
// 本文件把 ACL 与 bridge-domain 一并纳入 #192 的**同一份**对账视野（不新造第二套巡检）：
//   - 事实源 = 数据面（ip_table_dump / acl_dump / bridge_domain_dump），不是进程内记忆，
//     故**跨 nfvisd 重启仍然可见**；
//   - 每次对账（恢复收敛 + 15s 巡检）按数据面实况重建告警，对象消失即自动消警；
//   - 提交期 COMMIT_COMPENSATION_FAILED 若其对象已对得上配置（已声明、或确实不在数据面），
//     在本轮对账中一并消解；不属于可核对类别的（如 VM/容器）保持原样、不猜测。
//
// 诚实性（本仓库硬要求）：对账重建的告警文案明确写清「由启动/巡检对账重建、原始提交不可回溯」，
// 取不到的（是哪次提交、是否 NAT 引用）绝不编造；查询失败一律上抛、不当作「没有残渣」。

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// 残渣种类（ResidueItem.Kind，同时是告警 source 前缀）。
const (
	ResidueKindIPTables = "ip-table"
	ResidueKindACL      = "acl"
	ResidueKindBD       = "bridge-domain"
)

// ResidueItem 一条「数据面存在、配置未声明」的残渣事实。
type ResidueItem struct {
	Kind  string
	Ident string // IP 表 ID / ACL 名 / BD 名（BD 无名时用其 ID）
}

// Code 该残渣对应的告警码（与决策 #192 的 VRF_TABLE_LEFTOVER 同族）。
func (r ResidueItem) Code() string {
	switch r.Kind {
	case ResidueKindIPTables:
		return AlarmTableLeftover
	case ResidueKindACL:
		return AlarmACLLeftover
	default:
		return AlarmBDLeftover
	}
}

// Source 告警 source（形如 ip-table/123、acl/web、bridge-domain/vs-a）。
func (r ResidueItem) Source() string { return r.Kind + "/" + r.Ident }

// Message 告警文案：说明它是什么、下一步能做什么，并如实声明这是对账重建、原始提交不可回溯。
func (r ResidueItem) Message() string {
	return fmt.Sprintf("数据面存在配置未声明的 %s %s：多来自「提交补偿未完成」或「删表延后」留下的"+
		"残渣，也可能是手工创建的对象。它不被任何配置引用、不影响转发；本项由启动/巡检对账按数据面"+
		"事实重建（原始提交不可回溯），执行 request vpp restart 或手工清理后自动消警",
		r.kindLabel(), r.Ident)
}

func (r ResidueItem) kindLabel() string {
	switch r.Kind {
	case ResidueKindIPTables:
		return "IP 表"
	case ResidueKindACL:
		return "ACL"
	default:
		return "bridge-domain"
	}
}

// residueScan 一次对账的完整结果：残渣项、查询错误，以及各类型「数据面全部对象」的快照
// （用于判定提交期告警对象是否已复原）。*OK 表示该类型的查询成功——只有成功时才允许消警，
// 「问不出来」不得当成「已复原」（与 #192 同一诚实口径）。
type residueScan struct {
	items []ResidueItem
	errs  []error

	tableOK      bool
	presentTable map[uint32]bool
	aclOK        bool
	presentACL   map[string]bool
	bdOK         bool
	presentBD    map[string]bool
}

// scanResidue 按数据面实况扫描残渣（IP 表 ∪ ACL ∪ bridge-domain，声明集由配置给出）。
func (n *L2Network) scanResidue(cfg model.Config) residueScan {
	var s residueScan
	if n.l3 != nil {
		all, err := n.l3.AllTables()
		if err != nil {
			s.errs = append(s.errs, fmt.Errorf("对账数据面 IP 表: %w", err))
		} else {
			s.tableOK, s.presentTable = true, all
			declared := DeclaredTables(cfg)
			var ids []uint32
			for id := range all {
				if _, ok := declared[id]; !ok {
					ids = append(ids, id)
				}
			}
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			for _, id := range ids {
				s.items = append(s.items, ResidueItem{Kind: ResidueKindIPTables,
					Ident: strconv.FormatUint(uint64(id), 10)})
			}
		}
	}
	if n.acl != nil {
		tags, err := n.acl.ACLTagsInVPP()
		if err != nil {
			s.errs = append(s.errs, fmt.Errorf("对账数据面 ACL: %w", err))
		} else {
			s.aclOK, s.presentACL = true, make(map[string]bool, len(tags))
			declared := make(map[string]bool, len(cfg.Acls))
			for _, a := range cfg.Acls {
				declared[a.Name] = true
			}
			for _, t := range tags {
				s.presentACL[t] = true
				if !declared[t] {
					s.items = append(s.items, ResidueItem{Kind: ResidueKindACL, Ident: t})
				}
			}
		}
	}
	if n.l2 != nil {
		bds, err := n.l2.BDsInVPP()
		if err != nil {
			s.errs = append(s.errs, fmt.Errorf("对账数据面 bridge-domain: %w", err))
		} else {
			s.bdOK, s.presentBD = true, make(map[string]bool, len(bds))
			declared := map[string]bool{}
			for _, vs := range cfg.VirtualSwitches {
				if vs.Type == "l2" {
					declared[vs.Name] = true
				}
			}
			for _, bd := range bds {
				// 无名 BD 不参与对账：BD-Tag 是产品建的标识，空 tag 的 BD 无法与 VPP 内部/
				// 手工对象区分，报出来只会是无从处置的噪声（如实登记为对账边界）。
				if bd.Name == "" {
					continue
				}
				s.presentBD[bd.Name] = true
				if !declared[bd.Name] {
					s.items = append(s.items, ResidueItem{Kind: ResidueKindBD, Ident: bd.Name})
				}
			}
		}
	}
	sort.SliceStable(s.items, func(i, j int) bool {
		if s.items[i].Kind == s.items[j].Kind {
			return s.items[i].Ident < s.items[j].Ident
		}
		return s.items[i].Kind < s.items[j].Kind
	})
	return s
}

// applyResidueAlarms 按残渣项重建/消解**残渣码**告警（只动残渣码，不碰恢复收敛的其它告警）。
//
// 与 EnsureConsistent 里 Sync 的区别：Sync 覆盖整个 recovery 作用域（会把不在本轮失败清单里的
// 项当陈旧清掉），而本方法只认残渣码，可安全地供 15s 巡检单独调用。本次对账某类型查询失败时
// 保守留警（不消解该类型），避免「问不出来」被当成「已复原」。
func (n *L2Network) applyResidueAlarms(items []ResidueItem, s residueScan) {
	if n.alarms == nil {
		return
	}
	expect := make(map[string]bool, len(items))
	for _, it := range items {
		expect[it.Source()] = true
		n.alarms.Raise(recoveryScope, SeverityWarning, it.Code(), it.Message(), it.Source())
	}
	for _, a := range n.alarms.ActiveOf(recoveryScope) {
		if !IsResidueCode(a.Code) || expect[a.Source] {
			continue
		}
		if n.residueKindScanned(a.Code, s) {
			n.alarms.Resolve(recoveryScope, a.Code, a.Source)
		}
	}
}

// residueKindScanned 该告警码对应的类型本轮是否查询成功（成功才允许消警）。
func (n *L2Network) residueKindScanned(code string, s residueScan) bool {
	switch code {
	case AlarmTableLeftover:
		return s.tableOK
	case AlarmACLLeftover:
		return s.aclOK
	case AlarmBDLeftover:
		return s.bdOK
	}
	return false
}

// resolveCompensationAlarms 把**提交期补偿告警**中对得上配置的项消解。
//
// 判定（只对能按数据面核对的类别做，其余保持原样、不猜测）：该对象已由配置声明（配置说它该在，
// 重放负责把它收敛），或它确实不在数据面（残渣已消失）⇒ 该对象已不再是「数据面与配置不一致」，
// 消解其 COMMIT_COMPENSATION_FAILED。查询失败的类别跳过（问不出来 ≠ 已复原）。
func (n *L2Network) resolveCompensationAlarms(cfg model.Config, s residueScan) {
	if n.alarms == nil {
		return
	}
	declaredACL := make(map[string]bool, len(cfg.Acls))
	for _, a := range cfg.Acls {
		declaredACL[a.Name] = true
	}
	declaredBD := map[string]bool{}
	for _, vs := range cfg.VirtualSwitches {
		if vs.Type == "l2" {
			declaredBD[vs.Name] = true
		}
	}
	declaredTables := DeclaredTables(cfg)
	for _, a := range n.alarms.ActiveOf(orchestrator.CommitScope) {
		if a.Code != orchestrator.CommitCompensationFailed {
			continue
		}
		kind, name, ok := parseCommitOpDesc(a.Source)
		if !ok {
			continue
		}
		gone := false
		switch kind {
		case ResidueKindACL:
			gone = s.aclOK && (declaredACL[name] || !s.presentACL[name])
		case ResidueKindBD:
			gone = s.bdOK && (declaredBD[name] || !s.presentBD[name])
		case ResidueKindIPTables:
			if s.tableOK {
				id := TableID(name)
				_, declared := declaredTables[id]
				gone = declared || !s.presentTable[id]
			}
		}
		if gone {
			n.alarms.Resolve(orchestrator.CommitScope, a.Code, a.Source)
		}
	}
}

// parseCommitOpDesc 解析提交期计划操作描述，得到残渣对账可核对的 (种类, 对象名)。
// 形如 acl[web] / del-bridge-domain[vs-a] / vrf[vs-nat]；不属于可核对类别时 ok=false
// （保持告警，如实不猜测——VM/容器等残渣本部署不做数据面核对）。
func parseCommitOpDesc(desc string) (kind, name string, ok bool) {
	i := strings.IndexByte(desc, '[')
	if i <= 0 || !strings.HasSuffix(desc, "]") {
		return "", "", false
	}
	op := desc[:i]
	obj := desc[i+1 : len(desc)-1]
	if obj == "" {
		return "", "", false
	}
	switch op {
	case "acl", "del-acl":
		return ResidueKindACL, obj, true
	case "bridge-domain", "del-bridge-domain":
		return ResidueKindBD, obj, true
	case "vrf", "del-vrf":
		return ResidueKindIPTables, obj, true
	}
	return "", "", false
}

// ReconcileResidue 残渣对账（供 15s 巡检单独调用；EnsureConsistent 内部亦走同一段逻辑）。
// 返回未收敛/查询错误清单（调用方记日志即可）；同时按结果重建或消解残渣类告警、
// 并消解已复原对象的提交期补偿告警。
func (n *L2Network) ReconcileResidue(ctx context.Context, cfg model.Config) []error {
	if n == nil {
		return nil
	}
	s := n.scanResidue(cfg)
	n.applyResidueAlarms(s.items, s)
	n.resolveCompensationAlarms(cfg, s)
	errs := append([]error{}, s.errs...)
	for _, it := range s.items {
		errs = append(errs, fmt.Errorf("%s: %s", it.Source(), it.Message()))
	}
	return errs
}
