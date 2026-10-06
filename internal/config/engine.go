// Package config 实现配置事务引擎与持久化（M1 核心）。
//
// 状态机（骨架 §3.4）：edit() 进入持锁编辑 → set/delete/merge 修改 candidate（dirty）→
// commit 时 校验 + 下发底座 + 落库；discard()/空闲超时释放锁回无锁态；
// commit confirmed 由 nfvisd 侧定时器在超时未确认时自动回滚并告警（FR-CFG-003）。
package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/xzjt/nfvis/internal/clocksync"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// 事务引擎错误。ErrLocked/ErrNoRevision 定义于 store_sqlite.go。
var (
	ErrNotEditing = errors.New("当前会话未持有 candidate（需先进入配置模式）")
	// ErrLockLost 本会话曾经（或将）持锁，但当前锁由**同一用户的另一会话**持有——按决策
	// #318，干净锁可被同一用户的新会话接管，原会话再操作必须得到这条**明确**的错误，而不是
	// 静默地以为「锁还是我的」。与 ErrNotEditing（压根没进入过配置模式）区分开。
	ErrLockLost        = errors.New("本会话已失去 candidate 编辑权（编辑锁由同一用户的另一会话持有，可能已被接管；请重新进入配置模式）")
	ErrConfirmRequired = errors.New("管理口地址/网关变更必须以 commit confirmed 提交")
	// ErrFirewallConfirmRequired 决策 #388：非 console 会话的**防火墙变更**必须 commit confirmed——
	// 与 FR-CFG-012 管理口自锁保护同族（改错默认策略/规则会把当前管理路径切断），文案写明
	// 原因与照做路径（超时未确认自动回滚即恢复访问）。
	ErrFirewallConfirmRequired = errors.New("防火墙变更可能切断管理访问：必须以 commit confirmed <分钟> 提交（超时未确认将自动回滚、恢复访问）")
)

// 事件类型（经 Options.OnEvent 上报，M5 事件总线接入）。
const (
	EventConfirmedTimeout = "confirmed-timeout-rollback" // commit confirmed 超时自动回滚（FR-CFG-003 告警）
	EventLockTimeout      = "candidate-lock-timeout"     // candidate 空闲超时释放
)

// DefaultLockIdleTTL candidate 会话空闲超时（骨架 §3.4：空闲超时自动释放）。
const DefaultLockIdleTTL = 10 * time.Minute

// DefaultCleanLockIdleTTL **干净锁**的空闲回收阈值（决策 #318）。
//
// 「干净锁」＝持有会话的候选无未提交改动（dirty=false）——没有需要保护的东西，故比 10 分钟
// 的常规阈值更快回收（复用既有 `sweepLocked` 巡检，不新造定时器）。1 分钟的口径：足够覆盖
// 「进入配置模式后思考片刻再敲第一条 set」，又不会让一次遗留的干净锁挡住后续调用太久。
const DefaultCleanLockIdleTTL = 1 * time.Minute

// Session 配置会话标识。User 用于审计与锁展示，Source 用于 FR-CFG-012 判定。
type Session struct {
	User   string
	Source string // ssh | console | api
	// ID 会话稳定标识（决策 #317）：同一用户在**同一 Source** 下的多个会话（控制台、CLI、
	// 脚本各一个）以此互相区分。REST 侧取 token 稳定 ID（决策 #301），CLI 侧取发起该命令
	// 的 token 稳定 ID。空串 = 无稳定标识的旧式调用（引导/恢复/一次性内部动作），
	// **退回按身份键 user@source 归并**（保守：只影响自己、不动他人）。
	ID string
}

// holder 展示与审计用的身份键（user@source，不含会话标识）。
func (s Session) holder() string { return s.User + "@" + s.Source }

// key 会话锁的匹配键（决策 #317）：身份键 + 稳定会话 ID。ID 为空串时退回身份键本身，
// 与迁移前的老锁（session_id 为空）保持同一语义。
func (s Session) key() string { return sessionKeyFor(s.holder(), s.ID) }

// sessionKeyFor 由「展示用身份键 + 稳定会话 ID」构造匹配键。
//
// 用 `#` 作分隔：用户名的字符集不含 `#`、Source 是固定枚举，故拼接无歧义；
// LockInfo.key 与 Session.key 共用本函数，保证两侧算法单源。
func sessionKeyFor(holder, id string) string {
	if id == "" {
		return holder
	}
	return holder + "#" + id
}

// userOfHolder 从展示用身份键（user@source）取出所属用户（会话列表展示用）。
// 用户名不含 `@`、Source 是固定枚举，取最后一个 `@` 之前即用户名；取不到时原样返回。
func userOfHolder(holder string) string {
	if i := strings.LastIndex(holder, "@"); i > 0 {
		return holder[:i]
	}
	return holder
}

// CommitOpts commit 参数。
type CommitOpts struct {
	ConfirmedMinutes int    // >0 时为 commit confirmed（FR-CFG-003）
	Message          string // 提交说明，入修订记录
	// AllowNoSuperUser 豁免「配置里至少要保留一个 super-user 账号」的兜底（决策 #152）。
	//
	// **仅 request system zeroize（恢复出厂，FR-OPS-007）使用**：该动作的目的就是复位
	// 账号，提交空配置后账号表为空，由下次启动的引导重建 admin。其余任何路径（REST
	// commit、CLI commit / load override、配置恢复）都不得置位——置位等于关掉自锁防护。
	AllowNoSuperUser bool
}

// CommitResult commit 结果（对应 OpenAPI CommitResult）。
type CommitResult struct {
	Revision       int
	ConfirmedUntil *time.Time
	Warnings       []string
}

// SessionView 会话列表视图（show system configuration sessions，FR-CFG-009）。
//
// 决策 #317：除展示用的 holder 外，如实给出会话稳定标识与所属用户——同一用户的不同会话
// 不再被合并成一条，操作者可据此判断「是谁、哪个会话」在编辑。
type SessionView struct {
	Holder         string     `json:"holder"`
	SessionID      string     `json:"session_id"`
	User           string     `json:"user"`
	AcquiredAt     time.Time  `json:"acquired_at"`
	LastActivity   time.Time  `json:"last_activity"`
	Dirty          bool       `json:"dirty"`
	ConfirmedUntil *time.Time `json:"confirmed_until,omitempty"`
}

// Event 引擎上报的事件。
type Event struct {
	Type    string
	Time    time.Time
	Message string
}

// ValidationError commit 校验失败（FR-CFG-002 要求逐条列出全部错误）。
type ValidationError struct {
	Errors []model.ValidateError
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("commit 校验失败，共 %d 条", len(e.Errors))
}

// ImageInfo 镜像仓库元数据子集（FR-CFG-011⑤ 使用；完整仓库管理属 M4）。
type ImageInfo struct {
	Name string
	Type string // vm-image | container-image
}

// ImageResolver 镜像仓库查询接口（规则⑤：镜像存在性与类型匹配）。
// Names 用于把「镜像不存在」的报错变成**可照着排查**的话（列出仓库现有镜像）。
type ImageResolver interface {
	Lookup(name string) (ImageInfo, bool)
	// Names 返回仓库现有镜像名（升序），用于把「镜像不存在」的报错变成可照着排查的话。
	Names() []string
}

// TopologyReader 物理 NIC NUMA 拓扑查询接口（规则⑩性能警告使用；
// M3 接入 govpp 后由 state 模块提供真实实现）。
type TopologyReader interface {
	InterfaceNUMA(name string) (numa int, ok bool)
}

// Options 引擎可选项（零值取默认）。
type Options struct {
	LockIdleTTL time.Duration // candidate 空闲超时，默认 10m
	// CleanLockIdleTTL 「干净锁」（候选无未提交改动）的空闲回收阈值，默认 1m（决策 #318）。
	CleanLockIdleTTL time.Duration
	Now              func() time.Time                         // 时钟注入（测试）
	AfterFunc        func(d time.Duration, fn func()) func()  // 定时器注入（测试），返回 stop
	OnEvent          func(Event)                              // 事件回调（在引擎锁内调用，须快速返回）
	OnCommitted      func(revision int, user string)          // commit 成功回调（M5-1 config-committed 事件）
	Validate         func(model.Config) []model.ValidateError // commit 校验器，默认 model.Validate + CheckResources
	// TimeSynced 宿主时钟是否已与 NTP 同步的探针（NFR-006）。注入而非直接依赖 internal/system
	// ——引擎是最内层，不该反向依赖宿主交互包（骨架 §3.5）。nil = 不记录（字段为 NULL，表示未知）。
	TimeSynced     func() bool
	ImageResolver  ImageResolver  // 镜像仓库（nil = 跳过规则⑤）
	TopologyReader TopologyReader // NUMA 拓扑（nil = 跳过规则⑩）
}

// Engine 配置事务引擎。内部串行（单写多读，骨架 §4）。
type Engine struct {
	mu      sync.Mutex
	store   *Store
	applier orchestrator.Applier

	lockIdleTTL      time.Duration
	cleanLockIdleTTL time.Duration
	now              func() time.Time
	timeSynced       func() bool
	afterFunc        func(d time.Duration, fn func()) func()
	onEvent          func(Event)
	onCommit         func(revision int, user string)
	validate         func(model.Config) []model.ValidateError
	images           ImageResolver
	topology         TopologyReader

	confirmStop func() // 在途 confirmed 定时器的 stop

	candidate  *model.Config // nil = 无会话持锁
	sessionKey string        // 当前持锁会话的匹配键（决策 #317）
	holder     string        // 当前持锁会话的展示用身份键（user@source）
	sessionID  string        // 当前持锁会话的稳定标识（决策 #317）
	dirty      bool
	// superseded 记录「本会话的干净锁被谁接管了」（决策 #318 边界）：`被接管者键 → 接管者键`。
	// 被接管后，原会话只要面对的仍是那把接管者的锁，其 Edit/写候选/提交都得到**明确**的
	// ErrLockLost（而不是静默重新接管、把现场又变回自己）；接管者释放后原会话即可重新进入。
	// 仅内存态、随进程重启清空，条目数由接管次数上界。
	superseded map[string]string
}

// NewEngine 装配引擎；空库时写入初始空配置（rev 1），并恢复在途的
// confirmed 状态（nfvisd 重启不丢定时回滚，骨架 §3.4「计时器在 nfvisd 侧」）。
func NewEngine(store *Store, applier orchestrator.Applier, opts Options) (*Engine, error) {
	e := &Engine{
		store:            store,
		applier:          applier,
		lockIdleTTL:      opts.LockIdleTTL,
		cleanLockIdleTTL: opts.CleanLockIdleTTL,
		now:              opts.Now,
		timeSynced:       opts.TimeSynced,
		afterFunc:        opts.AfterFunc,
		onEvent:          opts.OnEvent,
		onCommit:         opts.OnCommitted,
		validate:         opts.Validate,
		superseded:       map[string]string{},
	}
	if e.lockIdleTTL <= 0 {
		e.lockIdleTTL = DefaultLockIdleTTL
	}
	if e.cleanLockIdleTTL <= 0 {
		e.cleanLockIdleTTL = DefaultCleanLockIdleTTL
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.afterFunc == nil {
		e.afterFunc = func(d time.Duration, fn func()) func() {
			t := time.AfterFunc(d, fn)
			return func() { t.Stop() }
		}
	}
	if e.validate == nil {
		// 默认校验链：结构/语义校验（model.Validate）+ 资源账本配额（FR-CMP-002）
		e.validate = func(c model.Config) []model.ValidateError {
			return append(model.Validate(c), model.CheckResources(c)...)
		}
	}
	e.images = opts.ImageResolver
	e.topology = opts.TopologyReader

	rev, _, err := store.LatestRevision()
	if err != nil {
		return nil, err
	}
	if rev == 0 {
		b, err := json.Marshal(model.Config{})
		if err != nil {
			return nil, err
		}
		if _, err := store.AppendRevision(b, e.now(), "初始化空配置", "system"); err != nil {
			return nil, err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.recoverConfirmedLocked(); err != nil {
		return nil, err
	}
	// 决策 #317：candidate 只存在于内存，进程重启后必然没有未提交编辑；遗留的 candidate_lock
	// 行已无保护对象，且其 session_id 指向旧 token（新会话的 ID 必然不同）——不清掉会把新会话
	// 挡在门外直到空闲超时（10 分钟）。故装配时释放遗留锁（如实登记：候选随进程消失）。
	// 注意：在途 commit confirmed 记在另一张表（confirmed_pending），不受此影响。
	if li, err := store.GetLock(); err != nil {
		return nil, err
	} else if li != nil {
		if err := store.ReleaseLock(li.Holder, li.SessionID); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// Close 停止在途定时器（nfvisd 优雅退出）。存储生命周期由装配方管理。
func (e *Engine) Close() {
	e.mu.Lock()
	if e.confirmStop != nil {
		e.confirmStop()
		e.confirmStop = nil
	}
	e.mu.Unlock()
}

// ---------- candidate 生命周期 ----------

// Edit 进入配置模式：获取 candidate 会话锁（FR-CFG-009），candidate 置为
// committed 副本。**同一会话**（同一 key，决策 #317：身份键 + 稳定会话 ID）重复调用幂等；
// 其它会话的**脏**锁一律拒绝。nfvisd 重启后遗留锁已在装配时释放（候选随进程消失），故重启后
// 任意会话都可重新进入配置模式。
//
// 决策 #318（干净锁不排他）：锁的排他性只为保护**未提交的候选**而存在。当锁的持有会话其候选
// **无未提交改动**（dirty=false）时，**同一用户**的任何新会话可直接**接管**该锁——原持有者失去
// 锁、新会话成为持有者（记一条 `config.lock-takeover` 审计，不报错）。边界**逐条保持**：
//   - `dirty=true` 时严格排他，绝不丢弃/抢占他人未提交候选（R79-1 的保护语义不变）；
//   - 仅限同一用户内接管，跨用户仍按既有语义报 ErrLocked（super-user 的额外能力不变）；
//   - 接管不改变 `sessions` 端点的可见性与 #317 的会话/身份分离（只是换了个持锁会话）；
//   - 被接管后原会话再操作得到 **ErrLockLost**（明确报错，不静默变成别人）——含它的写语句
//     重新进入配置模式（Edit）也被拒，直到接管者释放锁（见 superseded）。
//
// 由来（R98-1）：#317 按 token 拆开 CLI 会话后，CLI 一次性/脚本调用留下的**干净锁**不再被
// 下一次调用接管（新 token = 新会话键），于是挡住后续调用（真机 fulltest 195/3/11）。
func (e *Engine) Edit(sess Session) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sweepLocked()
	h := sess.holder()
	k := sess.key()

	li, err := e.store.GetLock()
	if err != nil {
		return err
	}
	// 决策 #318 边界：本会话的干净锁**刚被**另一会话接管 ⇒ 不静默重新接管回来，
	// 给明确的 ErrLockLost（接管者释放锁后此条自动失效，本会话可重新进入）。
	if li != nil && li.key() != k && e.superseded[k] == li.key() {
		return fmt.Errorf("%w（已被 %s 接管）", ErrLockLost, li.Holder)
	}
	takeover := false
	if li != nil && li.key() != k {
		if e.canTakeOverLocked(li, sess) {
			takeover = true
		} else {
			return fmt.Errorf("%w: 由 %s 持有", ErrLocked, li.Holder)
		}
	}
	if li != nil && !takeover && e.candidate != nil {
		// 同一会话重复 configure：幂等，保留未提交变更
		return e.store.RefreshLock(li.Holder, li.SessionID, e.now())
	}
	if li == nil || takeover {
		if takeover {
			// 接管：原持有者失去锁（其候选无未提交改动，丢弃不丢东西），本会话成为持有者。
			if err := e.store.ReleaseLock(li.Holder, li.SessionID); err != nil {
				return err
			}
			e.clearLockStateLocked()
			e.superseded[li.key()] = k // 记住「原会话被谁接管」——它再操作时报 ErrLockLost
			e.appendAudit(AuditEntry{
				Time: e.now(), User: sess.User, Action: "config.lock-takeover",
				Detail: fmt.Sprintf("接管空闲的干净编辑锁：原会话 %s 的候选无未提交改动，改由 %s 持有", li.Holder, h),
				Result: "success",
			})
		}
		if err := e.store.AcquireLock(h, sess.ID, e.now()); err != nil {
			return err
		}
	}
	committed, err := e.committedLocked()
	if err != nil {
		if li == nil || takeover {
			_ = e.store.ReleaseLock(h, sess.ID)
		}
		return err
	}
	delete(e.superseded, k) // 本会话已成为（或重新成为）持有者，旧的「被接管」记录失效
	cand := committed
	e.candidate = &cand
	e.holder = h
	e.sessionID = sess.ID
	e.sessionKey = k
	e.dirty = false
	return e.store.RefreshLock(h, sess.ID, e.now())
}

// canTakeOverLocked 判定本次 Edit 能否接管既有锁（决策 #318，调用方持引擎锁）：
// 锁的持有会话的候选**无未提交改动**，且是**同一用户**的会话。
//
// 干净判据与 Sessions() 的 Dirty 列同源（lockDirtyLocked）：只有「内存里 candidate 存在
// 且确实是这把锁的会话在持」才算脏；锁行存在而内存 candidate 为空（装配后遗留锁等）也按干净处理
// ——没有未提交改动可保护。跨用户永不接管（仍按既有权限/互斥语义）。
func (e *Engine) canTakeOverLocked(li *LockInfo, sess Session) bool {
	if e.lockDirtyLocked(li) {
		return false
	}
	return userOfHolder(li.Holder) == sess.User
}

// lockDirtyLocked 这把锁的持有会话是否有未提交改动（决策 #318；与 Sessions() 的 dirty 同源）。
func (e *Engine) lockDirtyLocked(li *LockInfo) bool {
	return li != nil && e.dirty && e.candidate != nil && e.sessionKey == li.key()
}

// clearLockStateLocked 清空内存编辑态（不动存储；调用方持锁）。
func (e *Engine) clearLockStateLocked() {
	// 决策 #374（R142 E7）：`superseded`（「干净锁被谁接管」）随会话生命周期收敛——
	// 清掉本会话自己的「被接管」记录，以及**指向本会话**的条目（被本会话接管过的会话
	// 重获尝试机会）。此前映射只增不减（被接管者永不回来即永久驻留）。
	// 注意顺序：接管路径先 clear 后 `superseded[oldKey]=newKey`，本清理不会抹掉新记录。
	if e.sessionKey != "" {
		delete(e.superseded, e.sessionKey)
		for k, v := range e.superseded {
			if v == e.sessionKey {
				delete(e.superseded, k)
			}
		}
	}
	e.candidate = nil
	e.holder = ""
	e.sessionID = ""
	e.sessionKey = ""
	e.dirty = false
}

// DiscardSession 结束**本会话（按稳定标识）**时丢弃其 candidate 并释放锁（决策 #318）。
//
// 与 Discard(Session) 的区别：**不以接入源（Source）为匹配维度**。CLI 会话取得的锁其 holder
// 是 `user@ssh`（或 `user@console`），而登出/吊销请求的会话身份来自 REST（holder `user@api`）；
// 二者按会话稳定标识（token ID）是**同一个会话**，按身份键却是两个 —— 此前 `handleLogout`
// 用 Discard({user,"api",id}) 清不掉 CLI 会话留下的锁（R98-1 的释放缺口）。匹配仍只认本 token
// 的稳定标识：同一用户的**另一** token 不受影响（R79-1 的保护语义不变）。
//
// id 为空（无稳定标识的旧式调用）返回 ErrNotEditing —— 调用方应退回 Discard(sess) 走身份键语义。
func (e *Engine) DiscardSession(user, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sweepLocked()
	if id == "" {
		return ErrNotEditing
	}
	li, err := e.store.GetLock()
	if err != nil {
		return err
	}
	if li == nil || li.SessionID != id || userOfHolder(li.Holder) != user {
		return ErrNotEditing
	}
	if e.sessionKey == li.key() {
		return e.releaseLocked()
	}
	// 理论不可达（内存编辑态与锁行同源）；仍按锁行释放，不留无主锁。
	return e.store.ReleaseLock(li.Holder, li.SessionID)
}

// Release 退出配置模式并释放锁（保留 candidate 变更与否由调用方先 commit/discard 决定）。
func (e *Engine) Release(sess Session) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sweepLocked()
	if err := e.requireHolderLocked(sess); err != nil {
		return err
	}
	return e.releaseLocked()
}

// Discard 丢弃 candidate 并释放锁（骨架 §3.4：discard → 无锁）。
func (e *Engine) Discard(sess Session) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sweepLocked()
	if err := e.requireHolderLocked(sess); err != nil {
		return err
	}
	if err := e.releaseLocked(); err != nil {
		return err
	}
	e.dirty = false
	return nil
}

// UpdateCandidate 整体替换 candidate（API PUT /configuration/candidate 与
// load override 语义，FR-CFG-008）。
func (e *Engine) UpdateCandidate(sess Session, cfg model.Config) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sweepLocked()
	if err := e.requireHolderLocked(sess); err != nil {
		return err
	}
	cand := deepCopyConfig(cfg)
	// 脱敏字段继承（R44-1）：配置视图（GET /configuration、candidate 读取）按 FR-SEC-007 /
	// 决策 #25 **移除**口令哈希，客户端据此整文档回写（load override 语义）时会把哈希抹掉——
	// 提交即让该账号失去口令（登录不能）。对"入参里缺失的敏感叶子"从 committed 同名用户继承：
	// 客户端没收到过的东西，不该被它"删掉"。CLI 的 save/load 往返带哈希，不受影响。
	if committed, err := e.committedLocked(); err == nil {
		inheritSensitive(&cand, committed)
	}
	e.candidate = &cand
	e.dirty = true
	return e.store.RefreshLock(sess.holder(), sess.ID, e.now())
}

// inheritSensitive 把 committed 里的敏感叶子补进 candidate 的**同名对象**（当前仅口令哈希）。
func inheritSensitive(cand *model.Config, committed model.Config) {
	if cand.System == nil || cand.System.Login == nil || committed.System == nil || committed.System.Login == nil {
		return
	}
	old := map[string]string{}
	for _, u := range committed.System.Login.Users {
		if u.PasswordHash != "" {
			old[u.Name] = u.PasswordHash
		}
	}
	for i := range cand.System.Login.Users {
		u := &cand.System.Login.Users[i]
		if u.PasswordHash == "" {
			if h, ok := old[u.Name]; ok {
				u.PasswordHash = h
			}
		}
	}
}

// MergeCandidate 增量合并 candidate（load merge 语义，FR-CFG-008）。
func (e *Engine) MergeCandidate(sess Session, patch model.Config) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sweepLocked()
	if err := e.requireHolderLocked(sess); err != nil {
		return err
	}
	merged, err := model.Merge(*e.candidate, patch)
	if err != nil {
		return err
	}
	e.candidate = &merged
	e.dirty = true
	return e.store.RefreshLock(sess.holder(), sess.ID, e.now())
}

// Candidate 返回当前会话的 candidate 与 dirty 标记。
func (e *Engine) Candidate() (model.Config, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sweepLocked()
	if err := e.requireEditingLocked(); err != nil {
		return model.Config{}, false, err
	}
	return deepCopyConfig(*e.candidate), e.dirty, nil
}

// Committed 返回当前生效配置（只读，任意会话可用）。
func (e *Engine) Committed() (model.Config, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.committedLocked()
}

// Sessions 返回持锁会话与 confirmed 状态（FR-CFG-009）。
// JSON 字段与 OpenAPI ConfigSession 契约一致。
func (e *Engine) Sessions() ([]SessionView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sweepLocked()
	var out []SessionView
	li, err := e.store.GetLock()
	if err != nil {
		return nil, err
	}
	if li != nil {
		out = append(out, SessionView{
			Holder:       li.Holder,
			SessionID:    li.SessionID,
			User:         userOfHolder(li.Holder),
			AcquiredAt:   li.AcquiredAt,
			LastActivity: li.LastActivity,
			Dirty:        e.lockDirtyLocked(li),
		})
	}
	cf, err := e.store.GetConfirmed()
	if err != nil {
		return nil, err
	}
	if cf != nil {
		deadline := cf.Deadline
		// 决策 #377/E13：在途 confirmed 归属到 **Holder 匹配的会话行**——此前挂在列表首行
		//（当前持锁会话），多会话时会把别人的 confirmed 记错行。无匹配行则如实追加一行。
		idx := -1
		for i := range out {
			if out[i].Holder == cf.Holder {
				idx = i
				break
			}
		}
		if idx < 0 {
			out = append(out, SessionView{Holder: cf.Holder, User: userOfHolder(cf.Holder)})
			idx = len(out) - 1
		}
		out[idx].ConfirmedUntil = &deadline
	}
	return out, nil
}

// ---------- commit / confirmed / rollback / compare ----------

// Commit 校验 + 下发底座 + 落库（FR-CFG-002/003/012）。失败时 candidate 保留。
// 在途 confirmed 的隐式确认：**持有会话**的任意新 commit 即确认（FR-CFG-004）。
//
// 决策 #364（R142-9）：隐式确认移到持有者校验**之后**——被拒的 commit（无锁/错锁）既不该
// 替在超时保护期内的变更「背书」，也不该把 `config.confirm` 审计记到被拒者名下。
//
// 返回值取具名（决策 #150）：高危档变更的意图行落库后，需要一个 defer 兜住
// 「意图与结果必须成对」——它要读最终的错误值。
func (e *Engine) Commit(ctx context.Context, sess Session, opts CommitOpts) (res CommitResult, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sweepLocked()

	if err := e.requireHolderLocked(sess); err != nil {
		return res, err
	}

	if cf, err := e.store.GetConfirmed(); err != nil {
		return res, err
	} else if cf != nil {
		e.clearConfirmedLocked(AuditEntry{
			Time: e.now(), User: sess.User, Action: "config.confirm",
			Detail: "新 commit 隐式确认在途 confirmed", Result: "success",
		})
	}
	rev, _, err := e.store.LatestRevision()
	if err != nil {
		return res, err
	}
	if !e.dirty {
		res.Revision = rev // 无变更 commit 不产生新修订
		return res, nil
	}

	// FR-CFG-002：schema + 语义校验，失败逐条列出
	verrs := e.validate(*e.candidate)
	// 决策 #152：自锁兜底——整文档提交不能把本机提交成「无人可登录」。
	// 挂在这里（e.validate 之后、下面的失败分支之前）有两个理由：①自定义校验链
	// （Options.Validate）可能把它整条漏掉，故不走那条链；②与既有校验共用同一个
	// 失败分支 ⇒ 同样返回 ErrValidation 形态的 ValidationError、candidate 与编辑锁
	// 都保留，操作者改完可以直接重提。
	if !opts.AllowNoSuperUser {
		verrs = append(verrs, model.CheckSuperUserPresent(*e.candidate)...)
	}
	if len(verrs) > 0 {
		e.appendAudit(AuditEntry{
			Time: e.now(), User: sess.User, Action: "config.commit",
			Detail: formatValidateErrors(verrs), Result: "failure",
		})
		return res, &ValidationError{Errors: verrs}
	}

	committed, err := e.committedLocked()
	if err != nil {
		return res, err
	}
	newCfg := *e.candidate

	// FR-CFG-012（决策 #121 扩围）：管理口地址/网关变更必须 commit confirmed——
	// 判据取"**该会话是否经网络接入**"：ssh 与 api（REST / Web 控制台）都依赖管理网连通性，
	// 改管理口可能切断自己的管理路径，故都要求确认；本地串口（console）不依赖管理网，
	// 保持豁免。首次声明也算变更（发现 #12(a)：nil 与空配置等价处理）。
	mgmtChanged := sysMgmtChanged(committed.System, newCfg.System)
	if mgmtChanged && sess.Source != "console" && opts.ConfirmedMinutes <= 0 {
		return res, ErrConfirmRequired
	}
	// 决策 #388：防火墙变更与上一条同族（FR-CFG-012 的判据 sess.Source != "console" 一致）——
	// 改错默认策略/规则会把当前管理路径切断，非 console 会话必须 commit confirmed；
	// console 会话（带外、不依赖管理网）豁免。两条判据分开、各自给原因，便于操作者照做。
	fwChanged := sysFirewallChanged(committed.System, newCfg.System)
	if fwChanged && sess.Source != "console" && opts.ConfirmedMinutes <= 0 {
		return res, ErrFirewallConfirmRequired
	}

	// FR-CFG-011⑤：镜像存在性与类型匹配（依赖仓库，注入接口）
	if e.images != nil {
		verrs = append(verrs, e.checkImages(&newCfg)...)
		if len(verrs) > 0 {
			e.appendAudit(AuditEntry{
				Time: e.now(), User: sess.User, Action: "config.commit",
				Detail: formatValidateErrors(verrs), Result: "failure",
			})
			return res, &ValidationError{Errors: verrs}
		}
	}

	// 生效提示（FR-SYS-009 / FR-SYS-002）
	if mgmtChanged {
		res.Warnings = append(res.Warnings, "警告: 管理口地址/网关已变更，注意连通性")
	}
	if !configEq(committed.Vpp, newCfg.Vpp) {
		res.Warnings = append(res.Warnings, "警告: vpp 变更需 request vpp restart（或整机 reboot）后生效")
	}
	if !configEq(committed.ResourcePools, newCfg.ResourcePools) {
		res.Warnings = append(res.Warnings, "警告: resource-pools 变更需 reboot 生效")
	}
	// 删掉被 NAT 用过的 L3 交换机：VPP 在该表上留 `nat44-ei-hi` 引用，只有数据面重启才释放
	// （round86 真机 R86-4/R86-10 实证）。提交本身按「延后收敛」口径成功（决策 #192），但
	// **表不会随本次提交从数据面消失**——必须在提交输出里说清，否则就是假成功；残渣另进告警。
	for _, name := range deletedNatRefVRFs(committed, newCfg) {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"警告: L3 交换机 %s 的表被 NAT 使用过：VPP 不释放该引用，表要到 request vpp restart "+
				"后才从数据面移除（在此之前该表仍留在数据面，已记告警）", name))
	}
	res.Warnings = append(res.Warnings, crossConnectPortWarnings(newCfg)...)
	res.Warnings = append(res.Warnings, e.numaWarnings(committed, newCfg)...)

	// FR-CFG-011⑤：镜像检查需要 committed 之后的候选配置

	// 决策 #150：高危档变更（本地用户/权限类/口令策略、外部证书文件引用）在**下发底座之前**
	// 留一条「意图」行——中途崩溃或失败时，读审计的人仍看得出这次动作要做什么。结果行沿用
	// 下面既有的那条（成功/失败都写）⇒ 一次高危提交恰好两条记录，非高危提交仍是一条。
	// 判定放在这里（而不是更早）：前面的校验/confirm 守卫都是**没开始动作**的拒绝，
	// 不该留下意图；意图落在这里，恰好是「动作即将执行」的那条线。
	hrIntent := highRiskConfigIntent(committed, newCfg, sess.Source)
	resultWritten := false
	if hrIntent != "" {
		e.appendAudit(AuditEntry{
			Time: e.now(), User: sess.User, Action: "config.commit",
			Detail: hrIntent, Result: AuditResultIntent,
		})
		// 兜底：意图行之后任何一条「没走到结果行」的返回（如落库失败）都要补一条 failure，
		// 否则审计里只剩一条「要做什么」，读的人无法判断动作到底做没做。
		defer func() {
			if resultWritten {
				return
			}
			e.appendAudit(AuditEntry{
				Time: e.now(), User: sess.User, Action: "config.commit",
				Detail: hrIntent + " 未完成: " + commitResultMissing(err), Result: "failure",
			})
		}()
	}

	// 下发底座（失败逆序补偿，全有或全无，骨架 §3.3）
	if err := e.applier.Apply(ctx, committed, newCfg); err != nil {
		resultWritten = true
		e.appendAudit(AuditEntry{
			Time: e.now(), User: sess.User, Action: "config.commit",
			Detail: fmt.Sprintf("%s\n底座错误: %v", model.Diff(committed, newCfg), err), Result: "failure",
		})
		return res, fmt.Errorf("底座下发失败（已补偿）: %w", err)
	}

	// 落库：追加式快照，历史修订天然保留（覆盖前快照不丢失）；记提交者（决策 #142）
	newRev, err := e.store.AppendRevision(mustJSON(newCfg), e.now(), opts.Message, sess.User)
	if err != nil {
		return res, err
	}
	if err := e.store.PruneRevisions(storeKeepRevisions); err != nil {
		return res, err
	}
	res.Revision = newRev

	if opts.ConfirmedMinutes > 0 {
		deadline := e.now().Add(time.Duration(opts.ConfirmedMinutes) * time.Minute)
		if err := e.store.SetConfirmed(newRev-1, deadline, sess.holder()); err != nil {
			return res, err
		}
		e.confirmStop = e.afterFunc(time.Until(deadline), e.onConfirmedTimer)
		res.ConfirmedUntil = &deadline
	}

	resultWritten = true
	e.appendAudit(AuditEntry{
		Time: e.now(), User: sess.User, Action: "config.commit",
		Detail: model.Diff(committed, newCfg), Result: "success",
	})
	e.dirty = false
	if e.onCommit != nil { // M5-1：config-committed 事件（在锁内，回调须快速返回）
		e.onCommit(newRev, sess.User)
	}
	return res, nil
}

// CommitCheck 仅校验 candidate 不下发（commit check，FR-CFG-002/004）。
func (e *Engine) CommitCheck(sess Session) ([]model.ValidateError, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sweepLocked()
	if err := e.requireHolderLocked(sess); err != nil {
		return nil, err
	}
	return e.validate(*e.candidate), nil
}

// ConfirmCommit 确认在途的 commit confirmed（FR-CFG-004）。
//
// 决策 #364（R142-9）：与 Rollback 同口径，须由**持锁会话**发起——confirmed 保护的是
// 「变更后未确认即回滚」，此前任何登录会话都能确认别人的在途保护（越权背书）；跨会话确认
// 现被拒（ErrNotEditing/ErrLockLost），需重新进入编辑态（空候选的 commit 也会隐式确认）。
func (e *Engine) ConfirmCommit(sess Session) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sweepLocked()
	if err := e.requireHolderLocked(sess); err != nil {
		return err
	}
	cf, err := e.store.GetConfirmed()
	if err != nil {
		return err
	}
	if cf == nil {
		return errors.New("无待确认的 commit confirmed")
	}
	e.clearConfirmedLocked(AuditEntry{
		Time: e.now(), User: sess.User, Action: "config.confirm",
		Detail: fmt.Sprintf("confirmed 已确认（基线 rev %d）", cf.BaseRev), Result: "success",
	})
	return nil
}

// Rollback 取第 n 个历史 committed 为 candidate，需再 commit 生效
// （FR-CFG-005，保留最近 50 份历史）。
func (e *Engine) Rollback(sess Session, n int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sweepLocked()
	if err := e.requireHolderLocked(sess); err != nil {
		return err
	}
	if n < 1 || n > storeKeepRevisions-1 {
		return fmt.Errorf("rollback %d: %w", n, ErrNoRevision)
	}
	// rollback 取消在途 confirmed 等待
	if cf, _ := e.store.GetConfirmed(); cf != nil {
		e.clearConfirmedLocked(AuditEntry{
			Time: e.now(), User: sess.User, Action: "config.confirm",
			Detail: "rollback 取消在途 confirmed 等待", Result: "success",
		})
	}
	rev, _, err := e.store.LatestRevision()
	if err != nil {
		return err
	}
	data, err := e.store.LoadRevision(rev - n)
	if err != nil {
		return err
	}
	var cfg model.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("解析历史快照: %w", err)
	}
	cand := cfg
	e.candidate = &cand
	e.dirty = true
	e.appendAudit(AuditEntry{
		Time: e.now(), User: sess.User, Action: "config.rollback",
		Detail: fmt.Sprintf("rollback %d：候选配置置为 rev %d（需 commit 生效）", n, rev-n), Result: "success",
	})
	return e.store.RefreshLock(sess.holder(), sess.ID, e.now())
}

// Compare 输出 committed ⇄ 第 n 个历史快照的 JunOS 风格 diff
// （show configuration | compare rollback n，FR-CFG-006）。
func (e *Engine) Compare(n int) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rev, _, err := e.store.LatestRevision()
	if err != nil {
		return "", err
	}
	if n < 1 || n > storeKeepRevisions-1 || rev-n < 1 {
		return "", fmt.Errorf("compare rollback %d: %w", n, ErrNoRevision)
	}
	data, err := e.store.LoadRevision(rev - n)
	if err != nil {
		return "", err
	}
	var snap model.Config
	if err := json.Unmarshal(data, &snap); err != nil {
		return "", fmt.Errorf("解析历史快照: %w", err)
	}
	committed, err := e.committedLocked()
	if err != nil {
		return "", err
	}
	return model.Diff(snap, committed), nil
}

// CompareCandidate 输出 candidate ⇄ committed 的 JunOS 风格 diff（配置模式 show | compare）。
func (e *Engine) CompareCandidate() (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sweepLocked()
	if err := e.requireEditingLocked(); err != nil {
		return "", err
	}
	committed, err := e.committedLocked()
	if err != nil {
		return "", err
	}
	return model.Diff(committed, *e.candidate), nil
}

// ---------- 内部 ----------

// requireHolderLocked 要求调用会话正是当前持锁会话（决策 #317：按匹配键判定，
// 不再按身份键——同一用户的不同会话互不冒充）。
//
// 决策 #318：若锁由**同一用户的另一会话**持有（含本会话的干净锁被接管后），返回
// **ErrLockLost**（明确报错：「你已不是持有者」），而不是笼统的 ErrNotEditing——否则被接管的
// 会话会以为自己从未进入配置模式、或以为锁还在自己手里。跨用户仍是 ErrNotEditing（不泄露他人）。
func (e *Engine) requireHolderLocked(sess Session) error {
	if e.candidate == nil || e.sessionKey == "" {
		return ErrNotEditing
	}
	if e.sessionKey == sess.key() {
		return nil
	}
	if userOfHolder(e.holder) == sess.User {
		return fmt.Errorf("%w（当前由 %s 持有）", ErrLockLost, e.holder)
	}
	return ErrNotEditing
}

// requireEditingLocked 仅要求存在编辑态会话（candidate 只读视图使用）。
func (e *Engine) requireEditingLocked() error {
	if e.candidate == nil {
		return ErrNotEditing
	}
	return nil
}

// releaseLocked 释放锁并清空编辑态（调用方持锁）。
func (e *Engine) releaseLocked() error {
	err := e.store.ReleaseLock(e.holder, e.sessionID)
	e.clearLockStateLocked()
	return err
}

// sweepLocked 兜底巡检：confirmed 到期回滚（定时器丢失/重启时序）与
// candidate 空闲超时释放（骨架 §3.4）。
//
// 决策 #318：对**干净锁**（持有会话候选无未提交改动）用更短的 cleanLockIdleTTL 回收——
// 「干净锁本就没有需要保护的东西」，留久了只会挡住后续调用（R98-1）。复用本巡检，
// 不新造定时器；脏锁仍走 10 分钟的既有阈值（绝不替操作者丢掉未提交改动）。
func (e *Engine) sweepLocked() {
	now := e.now()
	if cf, _ := e.store.GetConfirmed(); cf != nil && !now.Before(cf.Deadline) {
		e.doConfirmedRollback(cf)
	}
	li, _ := e.store.GetLock()
	if li == nil {
		return
	}
	ttl, detail := e.lockIdleTTL, "candidate 空闲超时，自动释放会话锁"
	if !e.lockDirtyLocked(li) {
		ttl, detail = e.cleanLockIdleTTL, "空闲的干净锁（候选无未提交改动）超时，自动释放会话锁"
	}
	if now.Sub(li.LastActivity) > ttl {
		holder, sessionID := li.Holder, li.SessionID
		e.clearLockStateLocked()
		_ = e.store.ReleaseLock(holder, sessionID)
		e.appendAudit(AuditEntry{
			Time: now, User: holder, Action: "config.lock-timeout",
			Detail: detail, Result: "success",
		})
		e.emit(EventLockTimeout, fmt.Sprintf("会话 %s 空闲超时，candidate 已释放", holder))
	}
}

// onConfirmedTimer confirmed 定时器触发：仅当确实已到期时回滚
// （测试可提前 Fire，生产中由 time.AfterFunc 保证到点）。
func (e *Engine) onConfirmedTimer() {
	e.mu.Lock()
	defer e.mu.Unlock()
	cf, err := e.store.GetConfirmed()
	if err != nil || cf == nil {
		return
	}
	if e.now().Before(cf.Deadline) {
		return
	}
	e.doConfirmedRollback(cf)
}

// doConfirmedRollback 超时自动回滚（FR-CFG-003）：以新修订恢复基线配置并告警。
// 调用方持引擎锁。
func (e *Engine) doConfirmedRollback(cf *ConfirmedInfo) {
	baseJSON, err := e.store.LoadRevision(cf.BaseRev)
	if err != nil {
		_ = e.store.ClearConfirmed()
		e.emit(EventConfirmedTimeout, fmt.Sprintf("confirmed 基线快照 rev %d 缺失: %v", cf.BaseRev, err))
		return
	}
	now := e.now()
	newRev, err := e.store.AppendRevision(baseJSON, now,
		fmt.Sprintf("commit confirmed 超时，自动回滚到 rev %d", cf.BaseRev), "system")
	if err != nil {
		e.emit(EventConfirmedTimeout, fmt.Sprintf("自动回滚落库失败: %v", err))
		return
	}
	_ = e.store.ClearConfirmed()
	if e.confirmStop != nil {
		e.confirmStop()
		e.confirmStop = nil
	}
	e.appendAudit(AuditEntry{
		Time: now, User: "system", Action: "config.rollback-auto",
		Detail: fmt.Sprintf("commit confirmed 超时未确认，已自动回滚（基线 rev %d）", cf.BaseRev), Result: "success",
	})
	e.emit(EventConfirmedTimeout, fmt.Sprintf("commit confirmed 超时，已自动回滚到 rev %d 并产生告警", cf.BaseRev))

	// 持锁会话的 candidate 同步回滚后配置
	if e.candidate != nil {
		var cfg model.Config
		if err := json.Unmarshal(baseJSON, &cfg); err == nil {
			e.candidate = &cfg
			e.dirty = true
		}
	}

	// 决策 #388 真机验证（round169）抓到的缺口：回滚改变了 committed 配置，却只落库、
	// 不走 OnCommitted ⇒ 宿主侧重收敛（防火墙/TLS/syslog/日志保留）不触发——防火墙场景
	// 的真机实锤是「配置已回滚、nft 表仍是 policy drop」，管理面锁死到重启。回滚与
	// Commit 一样是「已提交配置变更」，必须走同一通知（M5-1 事件 + 宿主侧再收敛）。
	if e.onCommit != nil {
		e.onCommit(newRev, "system")
	}
}

// recoverConfirmedLocked 装配时恢复在途 confirmed：到期立即回滚，未到重挂定时器。
func (e *Engine) recoverConfirmedLocked() error {
	cf, err := e.store.GetConfirmed()
	if err != nil || cf == nil {
		return err
	}
	if !e.now().Before(cf.Deadline) {
		e.doConfirmedRollback(cf)
		return nil
	}
	e.confirmStop = e.afterFunc(time.Until(cf.Deadline), e.onConfirmedTimer)
	return nil
}

// clearConfirmedLocked 确认在途 confirmed 并停定时器（调用方持锁）。
func (e *Engine) clearConfirmedLocked(audit AuditEntry) {
	_ = e.store.ClearConfirmed()
	if e.confirmStop != nil {
		e.confirmStop()
		e.confirmStop = nil
	}
	e.appendAudit(audit)
}

func (e *Engine) committedLocked() (model.Config, error) {
	_, data, err := e.store.LatestRevision()
	if err != nil {
		return model.Config{}, err
	}
	var cfg model.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return model.Config{}, fmt.Errorf("解析 committed 配置: %w", err)
	}
	return cfg, nil
}

// checkImages FR-CFG-011⑤：镜像必须在仓库中且类型与 VNF 形态匹配。
func (e *Engine) checkImages(cfg *model.Config) []model.ValidateError {
	var errs []model.ValidateError
	check := func(vnf, image, want string, path string) {
		info, ok := e.images.Lookup(image)
		if !ok {
			// 报错要能照着做（附录 A #98）：列出仓库里**真实可用**的名字，并说明配置里该写哪个。
			// 此前这里写「容器镜像的可用名是 docker load 落地的 tar 内嵌 tag」——与实现不符：
			// 导入后已按仓库目录项名重打标签，配置里唯一能用的就是仓库中的镜像名；照那句提示
			// 去写反而被拒（R84-7 真机：内嵌 tag 与重打 tag 两个名字都过不了校验）。
			msg := fmt.Sprintf("仓库中不存在镜像 %q；请写仓库中的镜像名", image)
			if names := e.images.Names(); len(names) > 0 {
				msg += "，当前可用：" + strings.Join(names, "、")
			} else {
				msg += "（仓库为空，先用 request images upload 导入或 request images download 拉取）"
			}
			if want == "container-image" {
				msg += "。容器镜像的可用名就是仓库中的镜像名（导入时已按该名重打标签）"
			}
			errs = append(errs, model.ValidateError{Path: path, Message: msg})
			return
		}
		if info.Type != want {
			errs = append(errs, model.ValidateError{Path: path,
				Message: fmt.Sprintf("镜像类型不匹配：需要 %s，实际 %s", want, info.Type)})
		}
	}
	for _, vm := range cfg.VirtualMachineFunctions {
		if vm.Image != "" {
			check(vm.Name, vm.Image, "vm-image", fmt.Sprintf("virtual-machine-functions[%s].image", vm.Name))
		}
	}
	for _, ct := range cfg.ContainerFunctions {
		if ct.Image != "" {
			check(ct.Name, ct.Image, "container-image", fmt.Sprintf("container-functions[%s].image", ct.Name))
		}
	}
	return errs
}

// crossConnectPortWarnings 报出「cross-connect 交换机端口数不足（<2）」的提交提示。
//
// 点对点直通必须两端，applier 在 `cross_connect=true` 且端口数 <2 时**有意什么都不挂载**
// （internal/orchestrator/network/l2.go 的 desiredMembers 返回空集合，随后的 takeStale
// 还会摘除旧成员）——这是**合法无操作**（既有 TestL2CrossConnectDetach 把「缩减到 1 端口」
// 判为合法），但提交照常成功、操作者看不到任何提示（round84 真机登记 R84-18）。
// 触发路径是「先建两点直通、再删掉一个端口」（或 load/恢复收敛回灌一份这种配置），
// 不是 `set … cross-connect <a> <b>`（决策 #79⑤ 已要求恰两个已声明端口，不足即报错）。
// 故在提交期把「本次提交不会建立直通」如实说清（决策 #308），不改合法性、不阻断提交。
// 端口数正常（≥2）的 cross-connect 交换机不提示；>2 由别名校验与 applier 明确报错覆盖。
func crossConnectPortWarnings(cfg model.Config) []string {
	var out []string
	for _, vs := range cfg.VirtualSwitches {
		if !vs.CrossConnect || len(vs.Ports) >= 2 {
			continue
		}
		out = append(out, fmt.Sprintf(
			"警告: cross-connect 交换机 %s 当前只有 %d 个端口：二层直通需恰好两个端口，"+
				"本次提交不会建立直通（配置保留、不报错）；请补足端口或改用普通 L2 转发",
			vs.Name, len(vs.Ports)))
	}
	return out
}

// deletedNatRefVRFs 本次提交删掉的、**旧配置里被 NAT 用作转发域**的 L3 交换机名（按旧声明序）。
//
// NAT 只作用于 L3 交换机（规格书 §4.3），其转发域有两处来源（决策 #52）：规则的
// virtual-switch（inside；地址池也落在该表）与出接口所属的 Vrf 条目（outside）。这两张表在
// VPP 里都会带 `nat44-ei-hi` 引用，而该引用**只有数据面重启才释放**——删表要到
// request vpp restart 后才真正生效（round86 真机 R86-4/R86-10，决策 #192）。
// 提交因此把这些交换机列进提示：提交成功不等于表已经从数据面消失。
func deletedNatRefVRFs(old, new model.Config) []string {
	if len(old.Vrfs) == 0 || old.Nat == nil || len(old.Nat.Rules) == 0 {
		return nil
	}
	kept := make(map[string]bool, len(new.Vrfs))
	for _, v := range new.Vrfs {
		kept[v.Name] = true
	}
	owner := make(map[string]string, len(old.Vrfs))
	for _, v := range old.Vrfs {
		for _, li := range v.L3Interfaces {
			owner[li.Interface] = v.Name
		}
	}
	ref := make(map[string]bool, len(old.Nat.Rules))
	for _, r := range old.Nat.Rules {
		if r.VirtualSwitch != "" {
			ref[r.VirtualSwitch] = true
		}
		if name, ok := owner[r.Action.Interface]; ok {
			ref[name] = true
		}
	}
	var out []string
	for _, v := range old.Vrfs {
		if ref[v.Name] && !kept[v.Name] {
			out = append(out, v.Name)
		}
	}
	return out
}

// numaWarnings FR-CFG-011⑩：vNIC 物理 NIC 与 VM 内存 NUMA 不一致时给性能警告。
func (e *Engine) numaWarnings(old, new model.Config) []string {
	if e.topology == nil {
		return nil
	}
	// 以 candidate 中交换机端口的第一物理口近似 vNIC 的落点（M3 精确化）
	ifaceByVM := map[string]string{}
	for _, vs := range new.VirtualSwitches {
		for _, p := range vs.Ports {
			if p.Interface != "" {
				ifaceByVM[vs.Name] = p.Interface
			}
		}
	}
	var out []string
	for _, vm := range new.VirtualMachineFunctions {
		if vm.Memory.NumaNode == nil {
			continue
		}
		for _, nic := range vm.Interfaces {
			iface := ""
			if nic.Type == "sriov-vf" && nic.Sriov != nil {
				iface = nic.Sriov.PhysicalInterface
			} else {
				iface = ifaceByVM[nic.VirtualSwitch]
			}
			if iface == "" {
				continue
			}
			if numa, ok := e.topology.InterfaceNUMA(iface); ok && numa != *vm.Memory.NumaNode {
				out = append(out, fmt.Sprintf("警告: VM %s 的 vNIC %s 物理 NIC %s 位于 NUMA %d，与内存 NUMA %d 不一致，跨 NUMA 访存将降低性能",
					vm.Name, nic.Name, iface, numa, *vm.Memory.NumaNode))
			}
		}
	}
	return out
}

func (e *Engine) emit(typ, msg string) {
	if e.onEvent != nil {
		e.onEvent(Event{Type: typ, Time: e.now(), Message: msg})
	}
}

func deepCopyConfig(c model.Config) model.Config {
	b, _ := json.Marshal(&c)
	var out model.Config
	_ = json.Unmarshal(b, &out)
	return out
}

func mustJSON(c model.Config) []byte {
	b, err := json.Marshal(&c)
	if err != nil {
		panic(fmt.Sprintf("配置序列化失败: %v", err)) // Config 结构恒可序列化
	}
	return b
}

func formatValidateErrors(verrs []model.ValidateError) string {
	out := "校验失败:"
	for _, ve := range verrs {
		out += "\n  - " + ve.Error()
	}
	return out
}

// sysMgmtChanged 判定管理口是否被变更（FR-CFG-012）：地址/网关/网卡名任一不同（含删除）即为 true。
//
// **首次声明也算变更**（发现 #12(a)）：此前在 `old.Management == nil` 时直接返回 false，于是
// 「本机还没配过管理口」的机器上第一次 `set system management ip address …` + `commit` **不需要**
// commit confirmed——而地址/网关正是该保护要防的那类改动（决策 #71：改管理网卡可能切断当前 SSH 会话）。
// 修法：nil 与"空配置"等价处理，只要最终结果与基线不同（含 nil → 有值）就算变更。
func sysMgmtChanged(old, new *model.SystemConfig) bool {
	var om, nm *model.MgmtConfig
	if old != nil {
		om = old.Management
	}
	if new != nil {
		nm = new.Management
	}
	if om == nil && nm == nil {
		return false
	}
	return !configEq(om, nm)
}

// sysFirewallChanged 判定系统防火墙段是否被变更（决策 #388）：规则/默认策略任一不同
// （含整段新增与删除）即为 true。nil 与 nil 等价；nil 与「显式写入」不同——首次配置同样
// 算变更（与 sysMgmtChanged 的发现 #12(a) 同口径）。
func sysFirewallChanged(old, new *model.SystemConfig) bool {
	var of, nf *model.FirewallConfig
	if old != nil {
		of = old.Firewall
	}
	if new != nil {
		nf = new.Firewall
	}
	if of == nil && nf == nil {
		return false
	}
	return !configEq(of, nf)
}

// configEq JSON 语义比较：typed-nil 序列化为 "null"，与未设置/已设置天然区分。
func configEq(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}

// CurrentRevision 返回当前 committed 修订号（API ConfigAccepted 响应使用）。
func (e *Engine) CurrentRevision() (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rev, _, err := e.store.LatestRevision()
	return rev, err
}

// Revision 配置提交历史条目（决策 #142：`GET /configuration/history` 与 CLI
// `show configuration history` 的**同一份**视图；CLI 与 REST 都调 Engine.History，
// 不存在第二份取数逻辑）。有意不含配置正文（FR-SEC-007）。
type Revision struct {
	Rev         int       `json:"rev"`
	CommittedAt time.Time `json:"committed_at"`
	User        string    `json:"user"`
	Comment     string    `json:"comment"`
	Current     bool      `json:"current"`
}

// History 返回最近 limit 份 committed 修订的元数据，**最新在前**，
// 并以 current 标记当前生效的那一份（无历史时返回空切片，不是 nil——契约声明是数组，
// 发 null 会让按契约写的客户端踩空）。limit <= 0 取默认（保留窗口内的全部）。
func (e *Engine) History(limit int) ([]Revision, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	revs, err := e.store.ListRevisions(limit)
	if err != nil {
		return nil, err
	}
	latest := 0
	if len(revs) > 0 {
		latest = revs[0].Rev // 降序，首条即最新
	}
	out := make([]Revision, 0, len(revs))
	for _, r := range revs {
		out = append(out, Revision{
			Rev:         r.Rev,
			CommittedAt: r.CommittedAt,
			User:        r.User,
			Comment:     r.Message,
			Current:     r.Rev == latest,
		})
	}
	return out, nil
}

// AuditTrail 返回最近 limit 条配置变更审计记录（show log audit / GET /audit-logs）。
func (e *Engine) AuditTrail(limit, offset int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	return e.store.ListAudit(limit, offset)
}

// appendAudit 统一写审计：补上**时钟是否已同步**的标记（NFR-006）。
// 所有审计写入都必须经此，避免个别路径漏标。三态口径与告警侧共用 clocksync.Mark
// （单一事实源：探针为 nil ⇒ 未知，不谎称已同步）。
func (e *Engine) appendAudit(a AuditEntry) {
	a.TimeSynced = clocksync.Mark(e.timeSynced)
	e.store.AppendAudit(a)
}

// Audit 追加一条运行态操作审计（FR-OPS-031：生命周期操作入审计通道）。
// 与配置变更审计（config.commit 等）同表，便于统一 `show log audit` 呈现。
func (e *Engine) Audit(user, action, detail, result string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.appendAudit(AuditEntry{Time: e.now(), User: user, Action: action, Detail: detail, Result: result})
}
