package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/model"
)

// 配置事务端点（OpenAPI /configuration/*，FR-CFG-001~006/008/009、决策 #22）。
//
// API 会话语义：token 用户 + Source "api" 构成引擎会话持有者（user@api），
// 与 CLI 会话（user@ssh/user@console）按会话锁规则互斥（决策 #26）。
// 配置端点要求 configure 权限（命令树 §4：S）。

// sessionFromIdentity 由 token 身份构造引擎会话（*Server 方法，缺稳定标识时记日志）。
//
// 决策 #317：会话标识取 token 稳定 ID（决策 #301），使同一用户的不同 token（控制台一个、
// CLI 脚本一个、REST 客户端一个）成为**互不干扰**的独立会话——R79-1 的根因正是所有 REST
// 流量都归并成 `user@api` 一个身份键，于是任一 token 登出即丢弃该用户全部候选。
// 无身份（理论上不该到达受保护端点）或无稳定 ID 的**旧式调用**退回保守的身份键语义
// （只影响自己、不动他人），并记一条日志（修复过程可核查、不静默）。
func (s *Server) sessionFromIdentity(r *http.Request) config.Session {
	info, ok := Identity(r)
	if !ok {
		if s.log != nil {
			s.log.Warn("配置会话缺少认证身份，按保守身份键处理（只影响自己）", "source", "api")
		}
		return config.Session{User: "anonymous", Source: "api"}
	}
	if info.ID == "" && s.log != nil {
		s.log.Warn("配置会话缺少稳定标识，按身份键归并（只影响自己、不动他人）", "user", info.User, "source", "api")
	}
	return config.Session{User: info.User, Source: "api", ID: info.ID}
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

// discardOwnSession 结束本会话时的候选清理（决策 #119 → #317 → #318）。
//
// 按**会话稳定标识**（token ID）定位本会话的锁，**不区分接入源**：CLI 会话取得的锁其 holder 是
// `user@ssh`/`user@console`，而登出/吊销请求的会话身份来自 REST（holder `user@api`）——按身份键
// 匹配会漏（R98-1：CLI 一次性/脚本调用留下的干净锁因此无人释放）。按 token ID 匹配则精确：
// 同一用户的**另一** token 不受影响（R79-1 的保护语义不变）。无稳定 ID 的旧式调用退回 Discard。
func discardOwnSession(engine *config.Engine, sess config.Session, log *slog.Logger) {
	var err error
	if sess.ID != "" {
		err = engine.DiscardSession(sess.User, sess.ID)
	} else {
		err = engine.Discard(sess)
	}
	switch {
	case err == nil, errors.Is(err, config.ErrNotEditing), errors.Is(err, config.ErrLockLost):
		// 未在编辑是常态，不算失败；结束会话本就不能因清理失败而失败。
	default:
		if log != nil {
			log.Warn("结束会话时释放 candidate 失败", "err", err)
		}
	}
}

// mapEngineError 引擎错误 → 统一错误响应。
func mapEngineError(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		return
	case errors.Is(err, config.ErrLocked):
		writeError(w, http.StatusConflict, "CONFLICT", err.Error(), nil) // 409 会话锁占用
	case errors.Is(err, config.ErrLockLost):
		// 决策 #318：锁由同一用户的另一会话持有（可能已被接管）——明确报出，不静默。
		writeError(w, http.StatusConflict, "CONFLICT", err.Error(), nil)
	case errors.Is(err, config.ErrNotEditing):
		writeError(w, http.StatusConflict, "CONFLICT", "当前会话未持有 candidate（先 PUT /configuration/candidate）", nil)
	case errors.Is(err, config.ErrNoRevision):
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error(), nil)
	case errors.Is(err, config.ErrConfirmRequired):
		writeError(w, http.StatusBadRequest, "CONFIRM_REQUIRED", err.Error(), nil)
	case errors.Is(err, config.ErrFirewallConfirmRequired):
		// 决策 #388：防火墙变更的自锁保护（与上一条同族、同状态码语义）。
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

// ---------- 未知字段校验（BUG-22，round46） ----------
//
// 缺陷（外部测试报告 docs/REPORT-v3.0.29.md BUG-22）：REST `PUT /configuration/candidate`
// 接受 JSON 里的**任何**字段名，未知键被 `encoding/json` 静默丢弃，仍回 `{"committed":true}`：
//
//	PUT /configuration/candidate  {"system":{"dns":{"servers":["198.51.100.97"]}}}
//	→ 200 {"committed":true,"revision":125}   ← 真实字段是 system.dns_servers，配置什么都没变
//
// 同一份模型在 **CLI 路径**上是严格的：配置模式语句先回放 schema 树、再由 `fromJSONTree` 用
// `DisallowUnknownFields` 落模型（cliexec.go:1999），关键字写错立刻报「配置中不存在字段」。
// REST 侧此前只有裸 `json.Unmarshal`，是这条「命令成功、事实没变」的唯一缺口。
//
// 修法只在 **HTTP/文件入口**：先用 `DisallowUnknownFields` 严格解一次，只为拿「未知键」这一类
// 错误并给出可操作的文案；**判定与放行仍由 encoding/json 自己做**（不在这里重写一套 schema
// 校验），随后照旧 `json.Unmarshal` 得到 cfg 继续——REST 的 merge/override/auto-commit 语义、
// 锁获取顺序、既有错误码一律不变。
//
// 分档：只拒绝**结构体内**（顶层到各级子对象）的未知键。`annotations`（map[string]string）与
// 容器的 `env` 是自由键值对，`DisallowUnknownFields` 本来就不检查 map 的键——这一点由
// fix_bug22_unknown_field_test.go 的回归用例钉住（**不得**为一刀切而拒绝它们）。
//
// 有意**不**在这里做（如实登记的边界）：
//   - `internal/system/backup.go` 的归档恢复：归档是本产品的内部产物，旧版本归档里的历史键
//     必须继续能吃（严格化会让老备份无法恢复，破坏兼容），故那里保持宽松（见该处注释）。
//   - `model.Config` 的解码本身：CLI/REST/候选合并/归档共用同一入口，在模型层严格化会同时
//     改变 CLI 的报错口径与备份兼容性；本缺陷是「入口没校验」，不是「模型太宽松」。

// unknownConfigFieldMessage 检查配置文档里的**未知字段**，命中则返回中文报错文案（"" = 无未知键）。
//
// 只识别 `json: unknown field "x"` 这一类解码错误；其它 JSON 错误（类型不符、语法错误）**不**在此
// 处理，交给调用方原有的 `json.Unmarshal` 报「配置文档 JSON 不合法: …」——保持既有文案与状态码。
func unknownConfigFieldMessage(data []byte) string {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var probe model.Config
	err := dec.Decode(&probe)
	if err == nil {
		return ""
	}
	m := unknownFieldRe.FindStringSubmatch(err.Error()) // 同一个 regexp（cliexec.go，CLI 侧同源错误类）
	if m == nil {
		return ""
	}
	path, parent := unknownConfigFieldPath(data, m[1])
	if path == "" {
		path = m[1] // 定位不到路径时退回 encoding/json 给的键名（不谎报上下文）
	}
	// 文案：点名字段 + 结构提示；能猜到「想写的是哪个」时插在同一个括号里。
	hint := ""
	if s := similarKnownFields(parent, m[1]); len(s) > 0 {
		hint = "相近的已知字段: " + strings.Join(quoteAll(s), "、") + "；"
	}
	return `配置文档含未知字段 "` + path + `"（` + hint +
		"请检查字段名；可用 show configuration | display json 查看当前结构）"
}

// unknownConfigFieldPath 尽力给出未知键在文档里的**完整路径**（如 `system.dns.servers`、
// `virtual_switches[0].nosuch`），并返回未知键所在的对象类型（供「相近的已知字段」提示）。
//
// 为什么需要它：`encoding/json` 只报最后一段键名（且报错时 `Decoder.InputOffset()` 已走到文档
// 末尾，实测取不到位置），而 `{"system":{"dns":{"servers":[…]}}}` 这类**整棵子树都不存在**的写法
// 只会报 `unknown field "dns"`——只给 `dns` 不足以让人看出写错的是 `dns.servers`。
//
// 本函数沿**模型类型**把文档往下走一遍**只为文案**：判定与放行完全由上面的严格解码决定，这里走
// 不通时（返回空路径）文案退回键名，**不影响任何行为**。因此这里刻意只处理三种形状：结构体
// （查键）、数组/切片（带下标）、其余（map/interface 等**自由键值对**，整棵跳过——它们的键天然
// 合法，也不可能是本次报错的位置）。
func unknownConfigFieldPath(doc []byte, key string) (string, reflect.Type) {
	var tree any
	if err := json.Unmarshal(doc, &tree); err != nil {
		return "", nil // 文档本身不合法时调用方会在后续 Unmarshal 报出，这里不抢戏
	}
	steps, parent, ok := findUnknownField(reflect.TypeOf(model.Config{}), tree, nil, key)
	if !ok {
		return "", nil
	}
	return joinPathSteps(steps), parent
}

// joinPathSteps 路径段拼成可读路径（数组下标紧贴前一段：`virtual_switches[0].nosuch`）。
func joinPathSteps(steps []string) string {
	var b strings.Builder
	for _, s := range steps {
		if b.Len() > 0 && !strings.HasPrefix(s, "[") {
			b.WriteByte('.')
		}
		b.WriteString(s)
	}
	return b.String()
}

// findUnknownField 沿模型类型在 JSON 树上定位未知键（见 unknownConfigFieldPath 的说明）。
// 返回的 steps 已拼成路径段（数组下标写成 `[i]`）。
func findUnknownField(t reflect.Type, node any, steps []string, key string) ([]string, reflect.Type, bool) {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || node == nil {
		return nil, nil, false
	}
	switch t.Kind() {
	case reflect.Struct:
		obj, ok := node.(map[string]any)
		if !ok {
			return nil, nil, false
		}
		fields := jsonFieldsOf(t) // 声明顺序
		known := false
		for _, f := range fields {
			if f.name == key {
				known = true
				break
			}
		}
		if !known {
			if v, present := obj[key]; present { // 未知键就在这一层
				return appendLeafPath(steps, key, v), t, true
			}
		}
		for _, f := range fields {
			child, present := obj[f.name]
			if !present {
				continue
			}
			if p, parent, ok := findUnknownField(f.typ, child, appendStep(steps, f.name), key); ok {
				return p, parent, ok
			}
		}
	case reflect.Slice, reflect.Array:
		arr, ok := node.([]any)
		if !ok {
			return nil, nil, false
		}
		for i, e := range arr {
			if p, parent, ok := findUnknownField(t.Elem(), e, appendStep(steps, "["+strconv.Itoa(i)+"]"), key); ok {
				return p, parent, ok
			}
		}
	}
	// 其余类型（map/interface/标量）不在此校验（map 的键是自由键值对）
	return nil, nil, false
}

// appendLeafPath 追加未知键；若它的值是**单键对象**，一并带上里面的键（最多两层）。
//
// 成因：`dns` 这类整棵子树都不在模型里时，解码器只能报最外层；操作者真正写错的是里面的叶子
// （BUG-22 现场＝`{"dns":{"servers":[…]}}`，正确字段是 `dns_servers`，只报 `dns` 看不出问题）。
// **只在值恰好是单键对象时下钻**——多键时不知道该点哪个，宁可只报确知的那一段（不猜）。
func appendLeafPath(steps []string, key string, value any) []string {
	out := appendStep(steps, key)
	for i := 0; i < 2; i++ {
		obj, ok := value.(map[string]any)
		if !ok || len(obj) != 1 { // 多键（含 0 键）到此为止
			break
		}
		for k, v := range obj { // 单键：唯一的键就是它
			out = appendStep(out, k)
			value = v
		}
	}
	return out
}

// appendStep 复制后追加（避免兄弟分支共享底层数组互相覆盖）。
func appendStep(steps []string, s string) []string {
	out := make([]string, len(steps)+1)
	copy(out, steps)
	out[len(steps)] = s
	return out
}

// jsonField json 标签派生出的一个字段。
type jsonField struct {
	name string
	typ  reflect.Type
}

// jsonFieldsCache reflect.Type → []jsonField（每次请求都反射一遍没有必要）。
var jsonFieldsCache sync.Map

// jsonFieldsOf 结构体的 JSON 字段表（与 encoding/json 的取名规则一致：无 tag 用字段名、
// `json:"-"` 跳过、未导出字段跳过；`omitempty` 等选项不影响名字）。
func jsonFieldsOf(t reflect.Type) []jsonField {
	if cached, ok := jsonFieldsCache.Load(t); ok {
		return cached.([]jsonField)
	}
	out := make([]jsonField, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // 未导出
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out = append(out, jsonField{name: name, typ: f.Type})
	}
	jsonFieldsCache.Store(t, out)
	return out
}

// similarKnownFields 在父对象里找与未知键**相近**的已知字段（仅用于提示「是不是想写 X」）。
//
// 判据刻意保守：只认**前缀包含关系**（`dns` → `dns_servers`，即 BUG-22 的现场；
// `dns_server` → `dns_servers`，单复数写错）。不做编辑距离之类的模糊匹配——
// 提示写错比不提示更糟，前三级已够覆盖「少写/多写一个词」这一类真实笔误。
func similarKnownFields(parent reflect.Type, key string) []string {
	if parent == nil || parent.Kind() != reflect.Struct {
		return nil
	}
	var out []string
	for _, f := range jsonFieldsOf(parent) {
		if f.name == key {
			continue // 已知字段不会走到这里
		}
		if strings.HasPrefix(f.name, key) || strings.HasPrefix(key, f.name) {
			out = append(out, f.name)
		}
	}
	sort.Strings(out)
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

func quoteAll(items []string) []string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = `"` + s + `"`
	}
	return out
}

// handlePutCandidate PUT /configuration/candidate：写 candidate（FR-CFG-008）。
//
// 两种语义（决策 #22/#127）：
//   - 缺省 = **override**（整体替换，`load override`）；
//   - `X-NFVIS-Merge: true` = **merge**（按 model.Merge 与现有 candidate 合并，`load merge`）
//     ——只覆盖文档里出现的字段，未出现的保持不动。
//
// X-NFVIS-Auto-Commit: true 时校验+下发+落库一次完成（决策 #22）。
//
// BUG-22：文档含未知字段时 400 VALIDATION_FAILED 并点名路径（见 unknownConfigFieldMessage）——
// 校验在 `engine.Edit` **之前**，因此不取锁、不产生候选、revision 不变（失败无副作用）。
func (s *Server) handlePutCandidate(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionFromIdentity(r)
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "读取请求体失败", nil)
		return
	}
	if msg := unknownConfigFieldMessage(body); msg != "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", msg, nil)
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
	sess := s.sessionFromIdentity(r)
	if err := s.engine.Discard(sess); err != nil {
		mapEngineError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCommit POST /configuration/commit：提交 candidate（FR-CFG-002/003）。
func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionFromIdentity(r)
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
	sess := s.sessionFromIdentity(r)
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
	verrs, err := s.engine.CommitCheck(s.sessionFromIdentity(r))
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
	sess := s.sessionFromIdentity(r)
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
//
// 决策 #317：非 super-user 只能看到**自己的**会话（与 /system/api-tokens 同口径）；
// super-user 看全部。配置锁只有具备 configure 权限（super-user）的会话能取得，故此过滤
// 对非 super-user 实际等价于「看不到别人的编辑锁」——不泄露其他用户的会话标识。
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	views, err := s.engine.Sessions()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	if info, ok := Identity(r); ok {
		views = visibleSessions(views, info.User, info.Class)
	}
	writeJSON(w, http.StatusOK, views)
}

// visibleSessions 按调用者范围过滤持锁会话（决策 #317）：super-user 全量，其他 class 仅自己。
func visibleSessions(views []config.SessionView, user, class string) []config.SessionView {
	if class == aaa.ClassSuperUser {
		return views
	}
	out := make([]config.SessionView, 0, len(views))
	for _, v := range views {
		if v.User == user {
			out = append(out, v)
		}
	}
	return out
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
