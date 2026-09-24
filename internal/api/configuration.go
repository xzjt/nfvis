package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/model"
)

// 配置事务端点（OpenAPI /configuration/*，FR-CFG-001~006/008/009、决策 #22）。
//
// API 会话语义：token 用户 + Source "api" 构成引擎会话持有者（user@api），
// 与 CLI 会话（user@ssh/user@console）按会话锁规则互斥（决策 #26）。
// 配置端点要求 configure 权限（命令树 §4：S）。

// sessionFromIdentity 由 token 身份构造引擎会话。
func sessionFromIdentity(r *http.Request) config.Session {
	info, ok := Identity(r)
	if !ok {
		return config.Session{User: "anonymous", Source: "api"}
	}
	return config.Session{User: info.User, Source: "api"}
}

// endOneShot 一次性事务的收尾：候选干净时交还全局编辑锁（决策 #151）。
//
// 「取锁 → 写候选 → 立即提交」这类入口提交后不再需要继续编辑——REST 的 login-users 一族
// （建/删用户、改 class、重置口令、自助改密）、带 `X-NFVIS-Auto-Commit: true` 的直提写、
// CLI 操作模式下的 delete 型 request 命令。但 `Engine.Commit` **不释放**锁（只有
// `Release`/`Discard`/空闲超时释放），于是锁一直留到登出（决策 #119）或 `lockIdleTTL`：
// 其它会话随后的配置写会被 409「candidate 会话锁被占用: 由 X 持有」挡住（round76 校验
// 三次复现）。控制台配置页是**客户端**显式收尾（`app.js` 的 `cfgEndSession()`），
// 服务端的一次性事务此前没有对应收尾。
//
// 判据只有一条：**只在候选不脏时释放**（与配置页同款语义）——
//   - 提交成功（候选已清空）→ 释放；
//   - 写候选之前就失败（校验 400/409、UpdateCandidate 失败）→ 候选没动过 → 释放
//     （留着锁只会挡住别人）；
//   - 候选脏（有未提交改动）→ **不释放**：那是操作者的改动，不能替它丢掉；
//   - 提交因校验失败（`ErrValidation`，候选保留）→ **不释放**：操作者要接着改。
//
// 调用方须已成功 `Edit`（本会话持锁）；未持锁时 `Candidate` 返回 `ErrNotEditing`，
// 本函数静默返回——不替别的会话、也不替已交还的会话做收尾。
func endOneShot(engine *config.Engine, sess config.Session, log *slog.Logger) {
	if _, dirty, err := engine.Candidate(); err != nil || dirty {
		return
	}
	if err := engine.Release(sess); err != nil && log != nil {
		log.Warn("一次性事务交还会话锁失败", "err", err)
	}
}

// mapEngineError 引擎错误 → 统一错误响应。
func mapEngineError(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		return
	case errors.Is(err, config.ErrLocked):
		writeError(w, http.StatusConflict, "CONFLICT", err.Error(), nil) // 409 会话锁占用
	case errors.Is(err, config.ErrNotEditing):
		writeError(w, http.StatusConflict, "CONFLICT", "当前会话未持有 candidate（先 PUT /configuration/candidate）", nil)
	case errors.Is(err, config.ErrNoRevision):
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error(), nil)
	case errors.Is(err, config.ErrConfirmRequired):
		writeError(w, http.StatusBadRequest, "CONFIRM_REQUIRED", err.Error(), nil)
	default:
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			// FR-API-005：校验错误逐条列入 detail
			detail := make([]ErrorDetail, 0, len(ve.Errors))
			for _, e := range ve.Errors {
				detail = append(detail, ErrorDetail{Path: e.Path, Message: e.Message})
			}
			writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "commit 校验失败", detail)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
	}
}

// handleGetCandidate GET /configuration/candidate：读取当前会话 candidate。
func (s *Server) handleGetCandidate(w http.ResponseWriter, r *http.Request) {
	cfg, dirty, err := s.engine.Candidate()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"candidate": redactConfigView(cfg), // 敏感字段不返回（FR-SEC-007 / 决策 #25）
		"dirty":     dirty,
	})
}

// handleGetConfiguration GET /configuration：读取 committed 配置（全量，脱敏）。
//
// 决策 #119（round42 覆盖核查缺口 #1）：此前只有分段读取（GET /system、GET /vpp/config
// 等配置段 + 资源端点），CLI `show configuration` 没有 REST 等价物；增量 2（配置读写）
// 的表单回显与配置总览依赖它。脱敏口径与 candidate 读取一致（FR-SEC-007 / 决策 #25）。
func (s *Server) handleGetConfiguration(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	out := map[string]any{"configuration": redactConfigView(cfg)}
	if rev, err := s.engine.CurrentRevision(); err == nil {
		out["revision"] = rev
	}
	writeJSON(w, http.StatusOK, out)
}

// handleConfigurationHistory GET /configuration/history：配置提交历史（决策 #142）。
//
// 与 CLI `show configuration history` **同源**（都调 Engine.History）——列表只给元数据：
// rev / 提交时间 / 提交者 / 提交说明 / 是否当前生效。有意**不回配置正文**
// （FR-SEC-007：历史快照里可能有口令哈希一类敏感字段，列表不需要它们）。
// 权限与 GET /configuration 同口径（ClassReadOnly 的 show configuration 族）。
func (s *Server) handleConfigurationHistory(w http.ResponseWriter, r *http.Request) {
	revs, err := s.engine.History(0)
	if err != nil {
		mapEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, revs) // 无历史时为空数组（契约声明是数组，不发 null）
}

// handlePutCandidate PUT /configuration/candidate：写 candidate（FR-CFG-008）。
//
// 两种语义（决策 #22/#127）：
//   - 缺省 = **override**（整体替换，`load override`）；
//   - `X-NFVIS-Merge: true` = **merge**（按 model.Merge 与现有 candidate 合并，`load merge`）
//     ——只覆盖文档里出现的字段，未出现的保持不动。
//
// X-NFVIS-Auto-Commit: true 时校验+下发+落库一次完成（决策 #22）。
func (s *Server) handlePutCandidate(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromIdentity(r)
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "读取请求体失败", nil)
		return
	}
	var cfg model.Config
	if err := json.Unmarshal(body, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "配置文档 JSON 不合法: "+err.Error(), nil)
		return
	}

	if err := s.engine.Edit(sess); err != nil {
		mapEngineError(w, err)
		return
	}
	// X-NFVIS-Auto-Commit: true 是「写候选 + 提交」一个请求内完成的一次性事务，
	// 收尾交还会话锁（决策 #151）；无该头时操作者正在编辑，锁必须留着。
	if r.Header.Get("X-NFVIS-Auto-Commit") == "true" {
		defer endOneShot(s.engine, sess, s.log)
	}
	merge := r.Header.Get("X-NFVIS-Merge") == "true"
	if merge {
		if err := s.engine.MergeCandidate(sess, cfg); err != nil {
			mapEngineError(w, err)
			return
		}
	} else if err := s.engine.UpdateCandidate(sess, cfg); err != nil {
		mapEngineError(w, err)
		return
	}

	if r.Header.Get("X-NFVIS-Auto-Commit") != "true" {
		w.Header().Set("X-NFVIS-Committed", "false")
		// merge 时入参与结果不同：回显**合并后的 candidate**（override 时二者相同）
		view := cfg
		if merge {
			if cur, _, err := s.engine.Candidate(); err == nil {
				view = cur
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"candidate": redactConfigView(view), "dirty": true})
		return
	}

	// 单请求直提（决策 #22）
	res, err := s.engine.Commit(r.Context(), sess, CommitOptsFrom(r))
	if err != nil {
		mapEngineError(w, err)
		return
	}
	w.Header().Set("X-NFVIS-Committed", "true")
	writeJSON(w, http.StatusOK, commitResponseOf(res))
}

// handleDeleteCandidate DELETE /configuration/candidate：丢弃 candidate 并释放锁
// （discard，骨架 §3.4）。
func (s *Server) handleDeleteCandidate(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromIdentity(r)
	if err := s.engine.Discard(sess); err != nil {
		mapEngineError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCommit POST /configuration/commit：提交 candidate（FR-CFG-002/003）。
func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromIdentity(r)
	res, err := s.engine.Commit(r.Context(), sess, CommitOptsFrom(r))
	if err != nil {
		mapEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, commitResponseOf(res))
}

// handleCommitConfirm POST /configuration/commit:confirm：确认在途 confirmed
// （FR-CFG-004）。
func (s *Server) handleCommitConfirm(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromIdentity(r)
	if err := s.engine.ConfirmCommit(sess); err != nil {
		mapEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "confirmed"})
}

// handleCheck POST /configuration/check：仅校验 candidate 不下发（CLI `commit check` 的
// REST 等价物，round42 覆盖核查缺口 #10 / 决策 #122）。校验发现问题时**仍返回 200**——
// 检查本身是成功的，结果在 ok 与 errors 里（客户端不必把 400 当"检查失败"处理）。
func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	verrs, err := s.engine.CommitCheck(sessionFromIdentity(r))
	if err != nil {
		mapEngineError(w, err)
		return
	}
	detail := make([]ErrorDetail, 0, len(verrs))
	for _, e := range verrs {
		detail = append(detail, ErrorDetail{Path: e.Path, Message: e.Message})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": len(verrs) == 0, "errors": detail})
}

// handleDiff GET /configuration/diff：candidate ⇄ committed 差异（JunOS 风格）。
func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	diff, err := s.engine.CompareCandidate()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(diff))
}

// handleRollback POST /configuration/rollback/{n}：取历史快照为 candidate
// （FR-CFG-005，需再 commit 生效）。
func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromIdentity(r)
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "rollback 编号必须为正整数", nil)
		return
	}
	if err := s.engine.Rollback(sess, n); err != nil {
		mapEngineError(w, err)
		return
	}
	cand, dirty, err := s.engine.Candidate()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidate": redactConfigView(cand), "dirty": dirty, "message": "candidate 已替换为历史快照，需 commit 生效"})
}

// handleSessions GET /system/configuration/sessions：持锁会话列表（FR-CFG-009，决策 #26）。
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	views, err := s.engine.Sessions()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

// ---------- 辅助 ----------

// CommitOptsFrom 解析 commit 请求体（{confirmed_minutes, message}，均可缺省）。
func CommitOptsFrom(r *http.Request) config.CommitOpts {
	var body struct {
		ConfirmedMinutes int    `json:"confirmed_minutes"`
		Message          string `json:"message"`
	}
	if r.Body != nil {
		data, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err == nil && len(data) > 0 {
			_ = json.Unmarshal(data, &body)
		}
	}
	return config.CommitOpts{ConfirmedMinutes: body.ConfirmedMinutes, Message: body.Message}
}

func commitResponseOf(res config.CommitResult) map[string]any {
	out := map[string]any{
		"committed": true,
		"revision":  res.Revision,
	}
	if res.ConfirmedUntil != nil {
		out["confirmed_until"] = res.ConfirmedUntil
	}
	if len(res.Warnings) > 0 {
		out["warnings"] = res.Warnings
	}
	return out
}
