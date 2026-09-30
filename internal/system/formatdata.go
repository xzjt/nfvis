package system

// 决策 #305：`request system storage format-data` —— 恢复出厂**数据状态**（保留管理面可达）。
//
// 一句话语义：把本机**受管数据**恢复到出厂状态，但保证管理面仍然可达（操作者执行完仍能
// 登录继续配置，不必带外干预、不必重启才能恢复）。三组口径：
//
//	① 收敛（复用既有编排，不另写一套删对象的逻辑）：把配置重置为**保留节最小配置**并提交——
//	   受管 VNF/容器（含 qcow2 内部快照）与全部网络配置对象由事务引擎的 applier **级联倒序**
//	   删除（决策 #196 的倒序与容错、#192 的删表延后、#100/#186 的 DPDK 端口延后收敛原样生效）；
//	   某个对象已不存在时容错继续。
//	② 保留（保命条款，硬性）：`system.management`/`system.api`/`system.login` 三节与物理口声明
//	   `set interfaces …`、DPDK 声明 `set vpp dpdk dev …`（`vpp.dpdk`）**逐字段原样保留**；
//	   底座与身份（VPP/libvirt/docker、产品二进制、TLS 证书、SSH host key、systemd 单元）不动。
//	   有意不保留 `vpp.cpu`/`vpp.memory`/`resource-pools`——前者依赖后者（校验强制核号/大页偏好
//	   必须落在 `resource-pools` 声明内），一起保留会让提交校验自相矛盾。
//	③ 清数据（受管数据）：镜像（经仓库逐删，保持 index.json 同步）、备份归档、抓包、core dump、
//	   诊断归档、VNF 磁盘/快照目录**内容**清空（目录本体与属主/权限保留）。
//
// 诚实性：**幂等**（第二次如实回「已是出厂态」，不提交空修订、不报错）；**部分失败如实报告**
// （残留逐条列出、非空即返回错误，绝不在未清干净时返回成功）；结果给可核对统计。
//
// 守卫与确认**照搬** `request system zeroize`（super-user、CLI 双确认、REST confirm=true、
// Web 高危档、审计两条），不另造一套——本文件只负责编排与统计，确认/审计在调用方。

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/model"
)

// keptSections 保留节的稳定命名（保命条款）。与契约 `FormatDataResult.kept_sections` 同源，
// 供结果、文档与守护对齐（改名即三处一起改）。
var keptSections = []string{
	"system.management", "system.api", "system.login", "interfaces", "vpp.dpdk",
}

// FormatDataCounts 按类别统计的**删除对象数**（配置里除保留节之外的一切）。
type FormatDataCounts struct {
	Containers      int `json:"containers"`
	VMs             int `json:"vms"`
	VirtualSwitches int `json:"virtual_switches"`
	VRFs            int `json:"vrfs"`
	Routes          int `json:"routes"`
	ACLs            int `json:"acls"`
	NATRules        int `json:"nat_rules"`
	PortMirroring   int `json:"port_mirroring"`
	QosPolicies     int `json:"qos_policies"`
	Bonds           int `json:"bonds"`
	LLDPInterfaces  int `json:"lldp_interfaces"`
}

// total 本次将删除的对象总数（0 = 配置已是保留节最小形态）。
func (c FormatDataCounts) total() int {
	return c.Containers + c.VMs + c.VirtualSwitches + c.VRFs + c.Routes + c.ACLs +
		c.NATRules + c.PortMirroring + c.QosPolicies + c.Bonds + c.LLDPInterfaces
}

// FormatDataResult 结果摘要（契约 FormatDataResult）。
type FormatDataResult struct {
	Status         string           `json:"status"`
	Revision       int              `json:"revision"`
	KeptSections   []string         `json:"kept_sections"`
	RemovedObjects FormatDataCounts `json:"removed_objects"`
	RemovedImages  int              `json:"removed_images"`
	PurgedFiles    int              `json:"purged_files"`
	FreedBytes     int64            `json:"freed_bytes"`
	AlreadyFactory bool             `json:"already_factory,omitempty"`

	// Residuals 未清掉的残留（逐条含原因）。非空即表示本次未清干净——FormatData 据此返回错误，
	// 调用方（REST/CLI）必须如实报失败，不得当成功。
	Residuals []string `json:"residuals,omitempty"`
}

// keptConfig 由当前 committed 配置推导**保留节最小配置**（保命条款）。
// 逐字段摘取，不重建结构——保留的字段与原文同一份值，单测断言前后逐字段相等。
func keptConfig(cur model.Config) model.Config {
	out := model.Config{}
	// 物理口声明（set interfaces …）：口名/描述/MTU/启停/SR-IOV/ingress-policy 全保留。
	out.Interfaces = cur.Interfaces
	// DPDK 声明（set vpp dpdk dev …）：只保留 vpp.dpdk 子节，不带 cpu/memory（见文件头 ②）。
	if cur.Vpp != nil && cur.Vpp.DPDK != nil {
		out.Vpp = &model.VppConfig{DPDK: cur.Vpp.DPDK}
	}
	if cur.System != nil {
		out.System = &model.SystemConfig{
			Management: cur.System.Management,
			API:        cur.System.API,
			Login:      cur.System.Login,
		}
	}
	return out
}

// countRemoved 统计当前配置里本次将删除的对象数（纯函数，便于单测）。
func countRemoved(cur model.Config) FormatDataCounts {
	c := FormatDataCounts{
		Containers:      len(cur.ContainerFunctions),
		VMs:             len(cur.VirtualMachineFunctions),
		VirtualSwitches: len(cur.VirtualSwitches),
		VRFs:            len(cur.Vrfs),
		ACLs:            len(cur.Acls),
		PortMirroring:   len(cur.PortMirroring),
		QosPolicies:     len(cur.QosPolicies),
		Bonds:           len(cur.Bonds),
	}
	for _, v := range cur.Vrfs {
		c.Routes += len(v.Routes)
	}
	if cur.Nat != nil {
		c.NATRules = len(cur.Nat.Rules) + len(cur.Nat.Static) + len(cur.Nat.SourcePools)
	}
	if cur.Protocols != nil && cur.Protocols.LLDP != nil {
		c.LLDPInterfaces = len(cur.Protocols.LLDP.Interfaces)
		if cur.Protocols.LLDP.Enabled {
			c.LLDPInterfaces++
		}
	}
	return c
}

// purgeTarget 一个受管数据目录（清空其内容、保留目录本体与属主/权限）。
type purgeTarget struct {
	Label string
	Path  string
}

// purgeTargets format-data 清空的受管数据目录清单。
// 注意：**不含** images（镜像经仓库 Delete 逐个删，索引与文件必须同步）；
// **不含** /var/lib/nfvis 本身（只清其下受管子目录的内容）、nfvis.db、tls/、software/、
// kernel-baseline.bak（决策 #305 ③ 的「不得删除」清单）。
func (m *Manager) purgeTargets() []purgeTarget {
	return []purgeTarget{
		{"备份归档", m.cfg.Dir},
		{"抓包文件", m.cfg.Captures},
		{"core dump", m.cfg.CoreDumps},
		{"诊断归档", m.cfg.TechSupport},
		{"VNF 磁盘与快照", m.cfg.VMs},
	}
}

// FormatData 恢复出厂数据状态（决策 #305）。调用方须已完成确认（本方法不再询问）。
//
// 幂等：配置已是保留节最小形态且受管数据目录已空时，不提交空修订、计数全 0、返回
// AlreadyFactory=true 且不报错。部分失败：残留非空时返回的 error 里逐条列出，调用方不得当成功。
func (m *Manager) FormatData(ctx context.Context, user string) (FormatDataResult, error) {
	res := FormatDataResult{
		Status:       "formatted",
		KeptSections: append([]string{}, keptSections...),
	}

	cur, err := m.engine.Committed()
	if err != nil {
		return res, fmt.Errorf("读取当前配置: %w", err)
	}
	kept := keptConfig(cur)
	configChanged := !reflect.DeepEqual(cur, kept)
	res.RemovedObjects = countRemoved(cur)

	rev, err := m.engine.CurrentRevision()
	if err != nil {
		return res, fmt.Errorf("读取当前修订号: %w", err)
	}
	res.Revision = rev

	// ① 收敛：把配置重置为保留节最小配置（受管对象与网络配置由 applier 级联倒序删除）。
	if configChanged {
		sess := config.Session{User: user, Source: config.SourceFormatData}
		if err := m.engine.Edit(sess); err != nil {
			return res, fmt.Errorf("进入配置模式: %w", err)
		}
		defer func() { _ = m.engine.Release(sess) }()
		if err := m.engine.UpdateCandidate(sess, kept); err != nil {
			return res, fmt.Errorf("置为保留节最小配置: %w", err)
		}
		// 不置 AllowNoSuperUser：保留节含 system.login（用户与 class），至少一个 super-user 仍在，
		// 与 zeroize「有意清空账号」的例外无关（那是全仓库唯一允许置位处）。
		commitRes, err := m.engine.Commit(ctx, sess, config.CommitOpts{
			Message: "format-data（恢复出厂数据状态）",
		})
		if err != nil {
			return res, fmt.Errorf("下发保留节最小配置: %w", err)
		}
		res.Revision = commitRes.Revision
	}

	// ③ 清数据：镜像（经仓库逐删，保持 index.json 与文件同步）→ 受管数据目录。
	if m.images != nil {
		for _, meta := range m.images.List() {
			if err := m.images.Delete(meta.Name, 0); err != nil {
				res.Residuals = append(res.Residuals,
					fmt.Sprintf("镜像 %s：删除失败: %v", meta.Name, err))
				continue
			}
			res.RemovedImages++
			res.FreedBytes += meta.SizeBytes
		}
	}
	for _, t := range m.purgeTargets() {
		files, bytes, errs := purgeDir(t.Label, t.Path)
		res.PurgedFiles += files
		res.FreedBytes += bytes
		res.Residuals = append(res.Residuals, errs...)
	}

	// ⑤ 幂等：什么都没删 = 已是出厂态（如实结论，不假装删了一堆东西）。
	res.AlreadyFactory = !configChanged && res.RemovedImages == 0 && res.PurgedFiles == 0
	if res.AlreadyFactory {
		res.Status = "already-factory"
	}

	if len(res.Residuals) > 0 {
		return res, fmt.Errorf("未清干净：%d 项残留——%s",
			len(res.Residuals), strings.Join(res.Residuals, "；"))
	}
	return res, nil
}

// Summary 结果的一句话摘要（CLI 与 REST 共用同一文案，避免两处漂移）。
func (r FormatDataResult) Summary() string {
	if r.AlreadyFactory {
		return fmt.Sprintf("已是出厂态：无受管对象/数据可清（修订 %d）", r.Revision)
	}
	return fmt.Sprintf("已重置数据分区：删除对象 %d 个（容器 %d / VNF %d / 交换机 %d / VRF %d / 路由 %d / ACL %d / NAT %d / 镜像 %d）、清理文件 %d 个、释放 %d 字节（修订 %d）；保留 %s",
		r.RemovedObjects.total(), r.RemovedObjects.Containers, r.RemovedObjects.VMs,
		r.RemovedObjects.VirtualSwitches, r.RemovedObjects.VRFs, r.RemovedObjects.Routes,
		r.RemovedObjects.ACLs, r.RemovedObjects.NATRules, r.RemovedImages,
		r.PurgedFiles, r.FreedBytes, r.Revision, strings.Join(r.KeptSections, "/"))
}

// purgeDir 清空目录内容（保留目录本体与属主/权限），返回清理的文件数与释放字节数。
//
// 目录不存在视为已清空（幂等，第二次执行不报错）；单个条目删除失败逐条记残留并继续
// ——由调用方汇总，绝不在未清干净时报成功。
func purgeDir(label, dir string) (files int, bytes int64, errs []string) {
	if dir == "" {
		return 0, 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, []string{fmt.Sprintf("%s（%s）：读取目录失败: %v", label, dir, err)}
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		n, sz := pathSize(p)
		if err := os.RemoveAll(p); err != nil {
			errs = append(errs, fmt.Sprintf("%s（%s）：删除 %s 失败: %v", label, dir, e.Name(), err))
			continue
		}
		files += n
		bytes += sz
	}
	return files, bytes, errs
}

// pathSize 统计路径下的文件数与总字节（文件计 1；目录递归，读不到的条目按 0 计、不阻断）。
func pathSize(p string) (files int, bytes int64) {
	info, err := os.Lstat(p)
	if err != nil {
		return 0, 0
	}
	if !info.IsDir() {
		return 1, info.Size()
	}
	_ = filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 读不到的条目按 0 计，不让统计本身阻断清理
		}
		if d.IsDir() {
			return nil
		}
		if fi, e := d.Info(); e == nil {
			files++
			bytes += fi.Size()
		}
		return nil
	})
	return files, bytes
}
