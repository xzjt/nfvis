package config

// 决策 #150：配置事务里的**高危档**变更在提交时留一条「意图」行（设计 §7）。
//
// 为什么落在引擎而不是端点：高危档里的「删用户 / 改口令策略 / 证书上传」**没有独立的
// 动作入口**——CLI 是 `set|delete` 之后 `commit`（`delete system login user x` 本身只是
// 改 candidate），REST 是端点内的直提，Web 控制台是表单 + 提交；三者都汇到同一个
// `Engine.Commit`。把意图判定放在这里，两侧（CLI 与 REST）自然同源，不需要各写一遍。
//
// 口径：
//   - action 不变（`config.commit`）——界面与 `show log audit` 不需要新的动作分类，
//     靠 `result` 区分意图行与结果行；
//   - 意图行在**下发底座之前**落库（中途崩溃也留下「要做什么」），其结果行沿用既有的
//     `config.commit` 成功/失败那条 ⇒ 一次高危提交恰好两条记录，非高危提交仍是一条；
//   - detail 写人话（要做什么、涉及谁/哪个文件），**口令与哈希不入审计**（沿用既有
//     脱敏口径 `model.RedactedPlaceholder`）。
//
// 覆盖边界（有意，避免审计噪音）：只覆盖「管理面身份与信任锚」这一类高危变更——
// 本地用户（增删改 class / 改口令）、自定义 class 的 allow/deny、口令策略、
// 外部证书文件引用。其它配置变更（接口、ACL、资源池…）照旧一条记录。
//
// 与操作级助手的分工：恢复出厂与恢复配置这两个动作本身由 internal/api 的操作级助手
// 记两行（`system.zeroize` / `system.restore`）；它们提交的配置（空配置 / 归档配置）
// 会连带改掉本地用户，这里**不再重复**记一组（见 SourceZeroize / SourceRestore）。

import (
	"fmt"
	"sort"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

// 高危动作专用的配置会话来源。恢复出厂与恢复配置各自是一个**动作**（操作级助手已记
// 意图 + 结果两条），故它们的提交不再走本文件的变更判定——否则同一个动作会留下两组记录。
const (
	SourceZeroize = "system-zeroize"
	SourceRestore = "system-restore"
)

// AuditResultIntent 审计 result 字段的「意图」取值（决策 #150）：动作开始执行之前落库的
// 那条记录用它标记；动作结束后仍是 success / failure。
//
// 导出：internal/api 的操作级助手也写意图行，取值必须与这里同一份（同一件事一个写法）。
const AuditResultIntent = "intent"

// highRiskConfigIntent 返回本次提交里高危档变更的「要做什么」说明；没有高危变更时返回空串。
//
// prev / next 是 committed 与候选配置，source 是提交会话来源。
func highRiskConfigIntent(prev, next model.Config, source string) string {
	if source == SourceZeroize || source == SourceRestore {
		return "" // 这两个动作由操作级助手记两行，不重复
	}
	p, n := loginOf(prev), loginOf(next)
	var parts []string
	parts = append(parts, userChangeParts(p, n)...)
	parts = append(parts, classChangeParts(p, n)...)
	parts = append(parts, policyChangeParts(p, n)...)
	parts = append(parts, certFileChangeParts(prev, next)...)
	return strings.Join(parts, "；")
}

// loginOf 取 system.login 视图（未配置时返回空结构，便于逐字段对照）。
func loginOf(cfg model.Config) model.SystemLogin {
	if cfg.System != nil && cfg.System.Login != nil {
		return *cfg.System.Login
	}
	return model.SystemLogin{}
}

// userChangeParts 本地用户的增 / 删 / 改（按名字对照；顺序取配置里的顺序，输出确定）。
func userChangeParts(prev, next model.SystemLogin) []string {
	before := make(map[string]model.LoginUserConfig, len(prev.Users))
	for _, u := range prev.Users {
		before[u.Name] = u
	}
	after := make(map[string]model.LoginUserConfig, len(next.Users))
	for _, u := range next.Users {
		after[u.Name] = u
	}
	var parts []string
	for _, u := range next.Users { // 新增与修改：按候选配置的顺序
		old, existed := before[u.Name]
		switch {
		case !existed:
			parts = append(parts, fmt.Sprintf("创建本地用户 %s（权限类 %s）", u.Name, classLabel(u.Class)))
		case old.PasswordHash != u.PasswordHash:
			// 只说明「口令被重置」——哈希与明文都不进审计（FR-SEC-007）
			parts = append(parts, fmt.Sprintf("重置本地用户 %s 的口令（值 %s）", u.Name, model.RedactedPlaceholder))
		}
		if existed && classLabel(old.Class) != classLabel(u.Class) {
			parts = append(parts, fmt.Sprintf("调整本地用户 %s 的权限类：%s → %s", u.Name, classLabel(old.Class), classLabel(u.Class)))
		}
	}
	for _, u := range prev.Users { // 删除：按已提交配置的顺序
		if _, kept := after[u.Name]; !kept {
			parts = append(parts, fmt.Sprintf("删除本地用户 %s（原权限类 %s）", u.Name, classLabel(u.Class)))
		}
	}
	return parts
}

// classChangeParts 自定义 class（命令树路径 allow/deny）的增 / 删 / 改。
func classChangeParts(prev, next model.SystemLogin) []string {
	before := make(map[string]model.ClassDef, len(prev.Classes))
	for _, c := range prev.Classes {
		before[c.Name] = c
	}
	after := make(map[string]model.ClassDef, len(next.Classes))
	for _, c := range next.Classes {
		after[c.Name] = c
	}
	var parts []string
	for _, c := range next.Classes {
		old, existed := before[c.Name]
		switch {
		case !existed:
			parts = append(parts, fmt.Sprintf("新建权限类 %s（允许 %d 条 / 拒绝 %d 条命令路径）", c.Name, len(c.Allow), len(c.Deny)))
		case !sameStrings(old.Allow, c.Allow) || !sameStrings(old.Deny, c.Deny):
			parts = append(parts, fmt.Sprintf("调整权限类 %s 的命令路径清单（允许 %d 条 / 拒绝 %d 条）", c.Name, len(c.Allow), len(c.Deny)))
		}
	}
	for _, c := range prev.Classes {
		if _, kept := after[c.Name]; !kept {
			parts = append(parts, fmt.Sprintf("删除权限类 %s", c.Name))
		}
	}
	return parts
}

// policyChangeParts 口令策略的逐字段变更（只列真的变了的项）。
//
// 数值 0 与「未配置」在产品语义上等价（走内建缺省），故 0 渲染成「未设置」——
// 于是 `nil ⇄ 全零` 这类**语义没变**的写法不会产生意图行（避免无谓审计噪音）。
func policyChangeParts(prev, next model.SystemLogin) []string {
	p, n := prev.PasswordPolicy, next.PasswordPolicy
	var parts []string
	field := func(name string, oldV, newV string) {
		if oldV != newV {
			parts = append(parts, fmt.Sprintf("%s %s → %s", name, oldV, newV))
		}
	}
	field("口令最小长度", numLabel(policyInt(p, func(x *model.PasswordPolicy) int { return x.MinLength })),
		numLabel(policyInt(n, func(x *model.PasswordPolicy) int { return x.MinLength })))
	field("口令复杂度要求", boolLabel(policyBool(p, func(x *model.PasswordPolicy) bool { return x.Complexity })),
		boolLabel(policyBool(n, func(x *model.PasswordPolicy) bool { return x.Complexity })))
	field("口令有效期（天）", numLabel(policyInt(p, func(x *model.PasswordPolicy) int { return x.ExpireDays })),
		numLabel(policyInt(n, func(x *model.PasswordPolicy) int { return x.ExpireDays })))
	field("连续失败锁定阈值", numLabel(policyInt(p, func(x *model.PasswordPolicy) int { return x.LockoutThreshold })),
		numLabel(policyInt(n, func(x *model.PasswordPolicy) int { return x.LockoutThreshold })))
	field("锁定时长（分钟）", numLabel(policyInt(p, func(x *model.PasswordPolicy) int { return x.LockoutMinutes })),
		numLabel(policyInt(n, func(x *model.PasswordPolicy) int { return x.LockoutMinutes })))
	if len(parts) == 0 {
		return nil
	}
	return []string{"修改口令策略：" + strings.Join(parts, "，")}
}

// certFileChangeParts 外部证书文件引用（`system api tls cert-file|key-file`）的变更。
//
// 这是「证书上传」在 CLI 侧的那条路径：CLI 给的是**文件路径**（配置声明），提交时由
// nfvisd 按配置安装；REST 的 `PUT /system/tls` 是直接上传 PEM 正文（操作级助手记录）。
// 两条路径的机制不同，但都属高危档、都记两条。
func certFileChangeParts(prev, next model.Config) []string {
	pc, pk := certPaths(prev)
	nc, nk := certPaths(next)
	if pc == nc && pk == nk {
		return nil
	}
	ref := fmt.Sprintf("cert-file %s，key-file %s", pathLabel(nc), pathLabel(nk))
	switch {
	case pc == "" && pk == "":
		return []string{"安装外部证书（按配置：" + ref + "）"}
	case nc == "" && nk == "":
		return []string{"移除外部证书文件引用（原 cert-file " + pathLabel(pc) + "，key-file " + pathLabel(pk) + "）"}
	}
	return []string{"更换外部证书文件：" + ref}
}

// certPaths 取 system.api 的证书/私钥文件路径（未配置时为空串）。
func certPaths(cfg model.Config) (cert, key string) {
	if cfg.System == nil || cfg.System.API == nil {
		return "", ""
	}
	return cfg.System.API.CertFile, cfg.System.API.KeyFile
}

// classLabel 权限类显示名：未设置即缺省 read-only（与 internal/api 的判定同口径）。
func classLabel(class string) string {
	if class == "" {
		return "read-only（默认）"
	}
	return class
}

// pathLabel 路径显示名：空串显示为「未设置」，免得读成「路径是空的」。
func pathLabel(p string) string {
	if p == "" {
		return "未设置"
	}
	return p
}

// numLabel 数值显示名：0 表示未设置（产品走内建缺省）。
func numLabel(v int) string {
	if v == 0 {
		return "未设置"
	}
	return fmt.Sprint(v)
}

// boolLabel 布尔显示名（false 是明确取值，不写「未设置」）。
func boolLabel(v bool) string {
	if v {
		return "开"
	}
	return "关"
}

func policyInt(p *model.PasswordPolicy, get func(*model.PasswordPolicy) int) int {
	if p == nil {
		return 0
	}
	return get(p)
}

func policyBool(p *model.PasswordPolicy, get func(*model.PasswordPolicy) bool) bool {
	if p == nil {
		return false
	}
	return get(p)
}

// sameStrings 两个字符串清单是否等价（顺序无关；allow/deny 的书写顺序不影响语义）。
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string{}, a...), append([]string{}, b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// commitResultMissing 结果行缺失时的原因文案（给意图行的兜底结果用）。
func commitResultMissing(err error) string {
	if err != nil {
		return err.Error()
	}
	return "提交在写出结果之前返回"
}
