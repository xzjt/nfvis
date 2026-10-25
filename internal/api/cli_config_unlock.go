package api

// O-3（报告 docs/REPORT-v3.0.29.md §7）：无「强制释放编辑锁」命令。
//
// 现场（报告原文）：脏锁只能等 10 分钟（`DefaultLockIdleTTL`）或 `systemctl restart nfvis`，
// `show configuration sessions` 也不给照做路径。运维因此只有两条路：干等，或重启服务
// （后者会连带打断所有在途操作）。
//
// 本文件实现 `request system configuration unlock [force]`：
//   - **不带 force**：只释放**本会话**自己持有的锁（等价 discard + 释放）。本会话没持锁时如实说。
//   - **带 force**：释放**全部**锁（super-user 级，写审计）。代价必须点名：持锁会话的
//     **未提交 candidate 会被丢弃**，它的后续操作会拿到 ErrNotEditing/ErrLockLost。
//
// 落点说明（为什么在本文件而不在 cli_compute_oper.go 的 requestSystem 里）：
// `requestSystem` 是「编排动作分发器」，本命令只动配置引擎的编辑锁、与底座编排无关；
// 且 cli_compute_oper.go 属于其它会话的在改文件。故在 `execOper` 的 `case "request"` 里
// **先拦一次**（见 cliexec.go），命中即返回，未命中照旧交给 execRequest。
//
// 引擎侧只调用既有导出方法（`Engine.Sessions` / `Engine.Discard` / `Engine.DiscardSession`），
// **不改 engine.go**。强制释放「别人的」锁的做法是：以该持锁会话的身份调用 `Discard`
// ——引擎按匹配键（holder + sessionID）判定，因此能精确落到那把锁上，且走的是引擎既有的
// 「释放锁 + 清编辑态」那条唯一路径（不另造一份释放逻辑，避免与 sweep/接管语义漂移）。

import (
	"fmt"
	"strings"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/schema"
)

// requestConfigUnlockEarly `request system configuration unlock [force]` 的**前置拦截**。
// 返回 (输出, true) 表示本命令已被处理；(_, false) 表示不归本文件管，调用方照旧分发。
//
// 判据只看 token 前缀（`request system configuration unlock`）——命令树已把该形态建模，
// 非法的尾部参数由这里给语法提示（不落到 requestSystem 的 configuration 分支）。
func (x *cliExecutor) requestConfigUnlockEarly(user, class, source string, s *cliSession, t []string) (string, bool) {
	// t 已剥离 "request"（调用方传 t[1:]）。期望：system configuration unlock [force] [--yes]
	toks := stripAllConfirm(t) // --yes 不是树节点（与 execRequest 同口径）
	if len(toks) < 3 || toks[0] != "system" || toks[1] != "configuration" || toks[2] != "unlock" {
		return "", false
	}
	// 入口权限（与命令树同源：unlock 是 S、force 也是 S——force 能丢别人的未提交候选）。
	if !x.allowTokens(class, mustNode(schema.OperRoot(), "request", "system"),
		append([]string{"request"}, toks...)...) {
		return "%% 无权限执行该命令\n", true
	}
	force := false
	for _, a := range toks[3:] {
		if a == "force" {
			force = true
			continue
		}
		return fmt.Sprintf("%% 语法: request system configuration unlock [force]（未知参数 %q）\n", a), true
	}
	return x.configUnlock(user, class, source, s, force), true
}

// configUnlock 释放编辑锁。
func (x *cliExecutor) configUnlock(user, class, source string, s *cliSession, force bool) string {
	views, err := x.engine.Sessions()
	if err != nil {
		return "%% 查询持锁会话失败: " + err.Error() + "\n"
	}
	if len(views) == 0 {
		return "（无持锁会话，无需释放）\n"
	}

	if !force {
		// 只释放本会话：引擎按匹配键（holder + 会话稳定标识）判定，别人的锁自然不在射程内。
		sess := x.sessOf(user, source)
		err := x.engine.Discard(sess)
		if err == nil {
			// DISCARD 会清空 candidate——CLI 本地若停在配置模式，继续留在那儿只会得到
			// 「未持有 candidate」的报错，故一并退回操作模式（与 discard/exit 同口径）。
			if s != nil && s.Mode == "config" {
				s.Mode = "oper"
				s.Path = nil
			}
			return "本会话的编辑锁已释放（未提交的 candidate 已丢弃）\n"
		}
		// 本会话没锁或锁在别人的会话手里：如实说明，并给出照做命令。
		own := sessionsOfUser(views, user)
		msg := fmt.Sprintf("%%%% 本会话未持有编辑锁（%v）\n", err)
		if len(own) == 0 {
			other := ""
			for _, v := range views {
				if v.User != user {
					other = v.Holder
					break
				}
			}
			if other != "" {
				msg += fmt.Sprintf("当前锁由 %s 持有；如确需释放请用 request system configuration unlock force"+
					"（**会丢弃该会话的未提交 candidate**）\n", other)
			}
		} else {
			msg += "当前用户有其它会话持锁；释放它请用 request system configuration unlock force" +
				"（**会丢弃该会话的未提交 candidate**）\n"
		}
		return msg
	}

	// force：释放**全部**锁（S 级，写审计）。
	// 代价在前：持锁会话的未提交 candidate 会被丢弃。
	var released, failed []string
	for _, v := range views {
		// 只按持锁会话释放；confirmed 在途行（无 session_id 且不是锁持有者）不是锁，跳过。
		if v.SessionID == "" && v.Holder == "" {
			continue
		}
		sess := config.Session{User: v.User, Source: sourceOfHolder(v.Holder), ID: v.SessionID}
		if v.User == "" {
			// 无匹配用户的 confirmed 行（决策 #377/E13）没有可释放的会话身份。
			continue
		}
		if derr := x.engine.Discard(sess); derr != nil {
			// 已不是持有者（并发释放/被接管）按达成处理，不阻断其余释放。
			if strings.Contains(derr.Error(), "未持有") || strings.Contains(derr.Error(), "未持有 candidate") {
				released = append(released, v.Holder)
				continue
			}
			failed = append(failed, fmt.Sprintf("%s（%v）", v.Holder, derr))
			continue
		}
		released = append(released, v.Holder)
	}

	// 释放后复核：如实报告仍未释放的（不谎报成功）。
	after, aerr := x.engine.Sessions()
	var still []string
	if aerr == nil {
		for _, v := range after {
			if v.Holder != "" {
				still = append(still, v.Holder)
			}
		}
	}

	x.audit(user, "config.unlock-force",
		fmt.Sprintf("强制释放全部编辑锁（释放 %d：%s）", len(released), strings.Join(released, ", ")), nil)

	var b strings.Builder
	if len(released) > 0 {
		fmt.Fprintf(&b, "已强制释放 %d 个编辑锁：%s\n", len(released), strings.Join(released, ", "))
		b.WriteString("注意：这些会话的**未提交 candidate 已被丢弃**，它们后续的配置操作需重新进入配置模式。\n")
	}
	for _, f := range failed {
		fmt.Fprintf(&b, "%%%% 释放失败：%s\n", f)
	}
	if aerr != nil {
		fmt.Fprintf(&b, "%%%% 释放后复核失败（结果不确定，请用 show configuration sessions 复查）：%v\n", aerr)
	} else if len(still) > 0 {
		fmt.Fprintf(&b, "%%%% 释放后仍有持锁会话：%s（可能刚被新会话取得）\n", strings.Join(still, ", "))
	}
	if len(released) == 0 && len(failed) == 0 {
		b.WriteString("（无持锁会话，无需释放）\n")
	}
	return b.String()
}

// sessionsOfUser 某用户的会话视图（不含无 User 的 confirmed 占位行）。
func sessionsOfUser(views []config.SessionView, user string) []config.SessionView {
	out := make([]config.SessionView, 0, len(views))
	for _, v := range views {
		if v.User == user {
			out = append(out, v)
		}
	}
	return out
}

// sourceOfHolder 从展示用身份键（user@source）取出来源；取不到时退回 "api"
// （source 是固定枚举 ssh/console/api；force 释放要构造一个 Session，
// 引擎按 holder+id 匹配锁，来源只要与 holder 里的后缀一致即可）。
func sourceOfHolder(holder string) string {
	if i := strings.LastIndex(holder, "@"); i >= 0 && i+1 < len(holder) {
		return holder[i+1:]
	}
	return "api"
}

// configUnlockHint 给 `show configuration sessions` 末尾追加的可照做命令（O-3）。
//
// 为什么要在读视图里给命令：报告的原话是「`show configuration sessions` 也不给照做路径」——
// 操作者看到「锁由 admin@ssh 持有」之后，下一步该敲什么无从得知。这里把出路直接印在
// 同一屏上：自己持锁 ⇒ discard；别人的锁 ⇒ force（并写明代价）。
// 由调用方按 class 决定是否印（非 super-user 无权 force）。
func configUnlockHint(views []config.SessionView, user, class string) string {
	if len(views) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("提示：本会话持锁用 discard 释放；如锁由其它会话持有，可用\n")
	if class == aaa.ClassSuperUser {
		b.WriteString("  request system configuration unlock force\n")
		b.WriteString("强制释放全部编辑锁（**会丢弃持锁会话未提交的 candidate**）。\n")
	} else {
		b.WriteString("  request system configuration unlock（释放本会话的锁）\n")
		b.WriteString("（force 释放他人锁需 super-user；当前 class 无权强制释放。）\n")
	}
	return b.String()
}
