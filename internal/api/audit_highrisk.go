package api

// 决策 #150：高危档动作的审计「两行」（设计 §7）。
//
// 高危档动作（恢复出厂、软件升级/回退、恢复配置、证书上传、本地用户与口令策略的写操作）
// 要在审计里留**两条**记录：动作开始执行之前写「意图」（result=intent），动作结束后写
// 「结果」（result=success|failure）。本文件是**操作级**（直接动底座的）那几个动作的
// 共用助手：REST handler 与 CLI 执行器都调它，于是同一条动作在两个入口的 action 名、
// 意图文案与脱敏口径完全一致——不需要在两个入口各写一遍。
//
// 配置事务型的高危变更（删用户 / 改口令策略 / 证书文件引用）不在本文件：它们没有独立的
// 动作入口，两侧都汇到 Engine.Commit，意图行由 internal/config 在提交时落（同一份实现）。
//
// 口径：
//   - action 沿用既有动作名（`system.zeroize` 等）——界面与 `show log audit` 不需要新的
//     动作分类，靠 result 就能把一次动作的两条配对；
//   - 意图行 detail 写清「要做什么」+ 关键参数（对象名/包名/归档来源），**不含任何秘密**
//     （口令、哈希、PEM 正文一律不进审计）；
//   - 结果行成功写结果说明、失败写原因（成功/失败都落库）；
//   - 被**前置拒绝**的请求（参数不全、确认词没给、confirm=false）不落审计——动作没开始，
//     与既有口径一致（拒绝不留半截状态）。

import "github.com/xzjt/nfvis/internal/config"

// highRiskAction 一条高危动作的审计口径（action 名 + 意图文案）。
type highRiskAction struct {
	Action string
	Intent string
}

// runHighRisk 高危动作的共用助手：**写意图 → 执行 → 写结果**。
//
// run 返回结果行的补充说明（成功时用；空则用意图原文）与执行错误。调用方（REST handler /
// CLI 执行器）只负责业务动作本身，审计两条的形状由本函数保证。
func runHighRisk(eng *config.Engine, user string, a highRiskAction, run func() (string, error)) error {
	eng.Audit(user, a.Action, a.Intent, config.AuditResultIntent)
	detail, err := run()
	if err != nil {
		eng.Audit(user, a.Action, a.Intent+" 失败: "+err.Error(), "failure")
		return err
	}
	if detail == "" {
		detail = a.Intent + "（已完成）"
	}
	eng.Audit(user, a.Action, detail, "success")
	return nil
}

// ---------- 各高危动作的审计口径（CLI 与 REST 共用同一份） ----------

// highRiskZeroize 恢复出厂。
func highRiskZeroize() highRiskAction {
	return highRiskAction{
		Action: "system.zeroize",
		Intent: "恢复出厂：清空配置/镜像/VNF，重置本地账号",
	}
}

// highRiskSoftwareAdd 软件升级。sha256 只取前 12 位入审计（够核对「用的是哪个包」，
// 又不让一行 detail 被 64 位十六进制占满）。
func highRiskSoftwareAdd(pkg, sha256 string) highRiskAction {
	detail := "安装软件包 " + pkg
	if len(sha256) >= 12 {
		detail += "（按 sha256 " + sha256[:12] + "… 校验）"
	}
	return highRiskAction{Action: "system.software.add", Intent: detail}
}

// highRiskSoftwareRollback 软件回退。
func highRiskSoftwareRollback() highRiskAction {
	return highRiskAction{
		Action: "system.software.rollback",
		Intent: "回退到上一版本：替换 nfvis 并重启 nfvisd",
	}
}

// highRiskRestore 恢复配置。src 说明归档来源（CLI 给路径、REST 给「上传的归档」）。
func highRiskRestore(src string) highRiskAction {
	return highRiskAction{
		Action: "system.restore",
		Intent: "从备份归档恢复配置（" + src + "）：归档里的配置将替换当前配置",
	}
}

// highRiskTLSInstall 证书上传（REST 直传 PEM；CLI 侧是配置里的 cert-file/key-file，
// 由 Engine.Commit 记意图行）。意图只写「要做什么」——PEM 正文与私钥绝不入审计。
func highRiskTLSInstall() highRiskAction {
	return highRiskAction{
		Action: "system.tls.install",
		Intent: "上传并安装外部 TLS 证书（PEM 正文不入审计）：管理面证书立即替换",
	}
}
