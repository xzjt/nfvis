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
	"sync"
	"time"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// 事务引擎错误。ErrLocked/ErrNoRevision 定义于 store_sqlite.go。
var (
	ErrNotEditing      = errors.New("当前会话未持有 candidate（需先进入配置模式）")
	ErrConfirmRequired = errors.New("管理口地址/网关变更必须以 commit confirmed 提交（FR-CFG-012）")
)

// 事件类型（经 Options.OnEvent 上报，M5 事件总线接入）。
const (
	EventConfirmedTimeout = "confirmed-timeout-rollback" // commit confirmed 超时自动回滚（FR-CFG-003 告警）
	EventLockTimeout      = "candidate-lock-timeout"     // candidate 空闲超时释放
)

// DefaultLockIdleTTL candidate 会话空闲超时（骨架 §3.4：空闲超时自动释放）。
const DefaultLockIdleTTL = 10 * time.Minute

// Session 配置会话标识。User 用于审计与锁展示，Source 用于 FR-CFG-012 判定。
type Session struct {
	User   string
	Source string // ssh | console | api
}

func (s Session) holder() string { return s.User + "@" + s.Source }

// CommitOpts commit 参数。
type CommitOpts struct {
	ConfirmedMinutes int    // >0 时为 commit confirmed（FR-CFG-003）
	Message          string // 提交说明，入修订记录
}

// CommitResult commit 结果（对应 OpenAPI CommitResult）。
type CommitResult struct {
	Revision       int
	ConfirmedUntil *time.Time
	Warnings       []string
}

// SessionView 会话列表视图（show system configuration sessions，FR-CFG-009）。
type SessionView struct {
	Holder         string     `json:"holder"`
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
	return fmt.Sprintf("commit 校验失败（FR-CFG-002），共 %d 条", len(e.Errors))
}

// ImageInfo 镜像仓库元数据子集（FR-CFG-011⑤ 使用；完整仓库管理属 M4）。
type ImageInfo struct {
	Name string
	Type string // vm-image | container-image
}

// ImageResolver 镜像仓库查询接口（规则⑤：镜像存在性与类型匹配）。
type ImageResolver interface {
	Lookup(name string) (ImageInfo, bool)
}

// TopologyReader 物理 NIC NUMA 拓扑查询接口（规则⑩性能警告使用；
// M3 接入 govpp 后由 state 模块提供真实实现）。
type TopologyReader interface {
	InterfaceNUMA(name string) (numa int, ok bool)
}

// Options 引擎可选项（零值取默认）。
type Options struct {
	LockIdleTTL    time.Duration                            // candidate 空闲超时，默认 10m
	Now            func() time.Time                         // 时钟注入（测试）
	AfterFunc      func(d time.Duration, fn func()) func()  // 定时器注入（测试），返回 stop
	OnEvent        func(Event)                              // 事件回调（在引擎锁内调用，须快速返回）
	OnCommitted    func(revision int, user string)          // commit 成功回调（M5-1 config-committed 事件）
	Validate       func(model.Config) []model.ValidateError // commit 校验器，默认 model.Validate + CheckResources
	ImageResolver  ImageResolver                            // 镜像仓库（nil = 跳过规则⑤）
	TopologyReader TopologyReader                           // NUMA 拓扑（nil = 跳过规则⑩）
}

// Engine 配置事务引擎。内部串行（单写多读，骨架 §4）。
type Engine struct {
	mu      sync.Mutex
	store   *Store
	applier orchestrator.Applier

	lockIdleTTL time.Duration
	now         func() time.Time
	afterFunc   func(d time.Duration, fn func()) func()
	onEvent     func(Event)
	onCommit    func(revision int, user string)
	validate    func(model.Config) []model.ValidateError
	images      ImageResolver
	topology    TopologyReader

	confirmStop func() // 在途 confirmed 定时器的 stop

	candidate *model.Config // nil = 无会话持锁
	holder    string
	dirty     bool
}

// NewEngine 装配引擎；空库时写入初始空配置（rev 1），并恢复在途的
// confirmed 状态（nfvisd 重启不丢定时回滚，骨架 §3.4「计时器在 nfvisd 侧」）。
func NewEngine(store *Store, applier orchestrator.Applier, opts Options) (*Engine, error) {
	e := &Engine{
		store:       store,
		applier:     applier,
		lockIdleTTL: opts.LockIdleTTL,
		now:         opts.Now,
		afterFunc:   opts.AfterFunc,
		onEvent:     opts.OnEvent,
		onCommit:    opts.OnCommitted,
		validate:    opts.Validate,
	}
	if e.lockIdleTTL <= 0 {
		e.lockIdleTTL = DefaultLockIdleTTL
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
		if _, err := store.AppendRevision(b, e.now(), "初始化空配置"); err != nil {
			return nil, err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.recoverConfirmedLocked(); err != nil {
		return nil, err
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
// committed 副本。持有者重复调用幂等；nfvisd 重启后同持有者可接管。
func (e *Engine) Edit(sess Session) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sweepLocked()
	h := sess.holder()

	li, err := e.store.GetLock()
	if err != nil {
		return err
	}
	if li != nil && li.Holder != h {
		return fmt.Errorf("%w: 由 %s 持有", ErrLocked, li.Holder)
	}
	if li != nil && e.candidate != nil {
		// 同持有者重复 configure：幂等，保留未提交变更
		return e.store.RefreshLock(h, e.now())
	}
	if li == nil {
		if err := e.store.AcquireLock(h, e.now()); err != nil {
			return err
		}
	}
	committed, err := e.committedLocked()
	if err != nil {
		if li == nil {
			_ = e.store.ReleaseLock(h)
		}
		return err
	}
	cand := committed
	e.candidate = &cand
	e.holder = h
	e.dirty = false
	return e.store.RefreshLock(h, e.now())
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
	e.candidate = &cand
	e.dirty = true
	return e.store.RefreshLock(sess.holder(), e.now())
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
	return e.store.RefreshLock(sess.holder(), e.now())
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
			AcquiredAt:   li.AcquiredAt,
			LastActivity: li.LastActivity,
			Dirty:        e.dirty && e.holder == li.Holder,
		})
	}
	cf, err := e.store.GetConfirmed()
	if err != nil {
		return nil, err
	}
	if cf != nil {
		deadline := cf.Deadline
		if len(out) == 0 {
			out = append(out, SessionView{Holder: cf.Holder})
		}
		out[0].ConfirmedUntil = &deadline
	}
	return out, nil
}

// ---------- commit / confirmed / rollback / compare ----------

// Commit 校验 + 下发底座 + 落库（FR-CFG-002/003/012）。失败时 candidate 保留。
// 在途 confirmed 的隐式确认：任意新 commit 即确认（FR-CFG-004）。
func (e *Engine) Commit(ctx context.Context, sess Session, opts CommitOpts) (CommitResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sweepLocked()
	res := CommitResult{}

	// 副作用必须后于持有者检查：非持有者的 commit 虽然会拿到 ErrNotEditing，
	// 但若把隐式确认放在检查之前，任何用户都能顺带取消他人在途 confirmed
	// 的自动回滚计时，使未确认配置永久生效（破坏 FR-CFG-012 回滚语义）。
	if err := e.requireHolderLocked(sess); err != nil {
		return res, err
	}
	if cf, err := e.store.GetConfirmed(); err != nil {
		return res, err
	} else if cf != nil {
		e.clearConfirmedLocked(AuditEntry{
			Time: e.now(), User: sess.User, Action: "config.confirm",
			Detail: "新 commit 隐式确认在途 confirmed（FR-CFG-004）", Result: "success",
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
	if len(verrs) > 0 {
		e.store.AppendAudit(AuditEntry{
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

	// FR-CFG-012：SSH 会话变更管理口必须 commit confirmed（已设管理口被改动/删除才算；
	// 从无到有的首次设置属初始化，不触发自锁保护）
	mgmtChanged := sysMgmtChanged(committed.System, newCfg.System)
	if mgmtChanged && sess.Source == "ssh" && opts.ConfirmedMinutes <= 0 {
		return res, ErrConfirmRequired
	}

	// FR-CFG-011⑤：镜像存在性与类型匹配（依赖仓库，注入接口）
	if e.images != nil {
		verrs = append(verrs, e.checkImages(&newCfg)...)
		if len(verrs) > 0 {
			e.store.AppendAudit(AuditEntry{
				Time: e.now(), User: sess.User, Action: "config.commit",
				Detail: formatValidateErrors(verrs), Result: "failure",
			})
			return res, &ValidationError{Errors: verrs}
		}
	}

	// 生效提示（FR-SYS-009 / FR-SYS-002）
	if mgmtChanged {
		res.Warnings = append(res.Warnings, "警告: 管理口地址/网关已变更，注意连通性（FR-CFG-012）")
	}
	if !configEq(committed.Vpp, newCfg.Vpp) {
		res.Warnings = append(res.Warnings, "警告: vpp 变更需 request vpp restart（或整机 reboot）后生效（FR-SYS-009）")
	}
	if !configEq(committed.ResourcePools, newCfg.ResourcePools) {
		res.Warnings = append(res.Warnings, "警告: resource-pools 变更需 reboot 生效（FR-SYS-002）")
	}
	res.Warnings = append(res.Warnings, e.numaWarnings(committed, newCfg)...)

	// FR-CFG-011⑤：镜像检查需要 committed 之后的候选配置

	// 下发底座（失败逆序补偿，全有或全无，骨架 §3.3）
	if err := e.applier.Apply(ctx, committed, newCfg); err != nil {
		e.store.AppendAudit(AuditEntry{
			Time: e.now(), User: sess.User, Action: "config.commit",
			Detail: fmt.Sprintf("%s\n底座错误: %v", model.Diff(committed, newCfg), err), Result: "failure",
		})
		return res, fmt.Errorf("底座下发失败（已补偿）: %w", err)
	}

	// 落库：追加式快照，历史修订天然保留（覆盖前快照不丢失）
	newRev, err := e.store.AppendRevision(mustJSON(newCfg), e.now(), opts.Message)
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

	e.store.AppendAudit(AuditEntry{
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
func (e *Engine) ConfirmCommit(sess Session) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sweepLocked()
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
	e.store.AppendAudit(AuditEntry{
		Time: e.now(), User: sess.User, Action: "config.rollback",
		Detail: fmt.Sprintf("rollback %d：候选配置置为 rev %d（需 commit 生效）", n, rev-n), Result: "success",
	})
	return e.store.RefreshLock(sess.holder(), e.now())
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

func (e *Engine) requireHolderLocked(sess Session) error {
	if e.candidate == nil || e.holder == "" {
		return ErrNotEditing
	}
	if e.holder != sess.holder() {
		return ErrNotEditing
	}
	return nil
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
	err := e.store.ReleaseLock(e.holder)
	e.candidate = nil
	e.holder = ""
	e.dirty = false
	return err
}

// sweepLocked 兜底巡检：confirmed 到期回滚（定时器丢失/重启时序）与
// candidate 空闲超时释放（骨架 §3.4）。
func (e *Engine) sweepLocked() {
	now := e.now()
	if cf, _ := e.store.GetConfirmed(); cf != nil && !now.Before(cf.Deadline) {
		e.doConfirmedRollback(cf)
	}
	li, _ := e.store.GetLock()
	if li != nil && now.Sub(li.LastActivity) > e.lockIdleTTL {
		// 必须以锁记录中的 holder 释放：nfvisd 重启后引擎内存态为空（e.holder == ""），
		// 用 e.holder 去 ReleaseLock 会因 SQL 匹配不到行而静默失败，残留锁永不可清扫。
		holder := li.Holder
		var relErr error
		if e.candidate != nil && e.holder == holder {
			relErr = e.releaseLocked()
		} else {
			relErr = e.store.ReleaseLock(holder)
		}
		if relErr != nil {
			e.store.AppendAudit(AuditEntry{
				Time: now, User: holder, Action: "config.lock-timeout",
				Detail: "candidate 空闲超时，自动释放会话锁失败: " + relErr.Error(), Result: "failure",
			})
			return
		}
		e.store.AppendAudit(AuditEntry{
			Time: now, User: holder, Action: "config.lock-timeout",
			Detail: "candidate 空闲超时，自动释放会话锁", Result: "success",
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

// doConfirmedRollback 超时自动回滚（FR-CFG-003）：以新修订恢复基线配置、
// 下发底座把运行态一并回退，并告警。调用方持引擎锁。
func (e *Engine) doConfirmedRollback(cf *ConfirmedInfo) {
	baseJSON, err := e.store.LoadRevision(cf.BaseRev)
	if err != nil {
		_ = e.store.ClearConfirmed()
		e.emit(EventConfirmedTimeout, fmt.Sprintf("confirmed 基线快照 rev %d 缺失: %v", cf.BaseRev, err))
		return
	}
	var baseCfg model.Config
	if err := json.Unmarshal(baseJSON, &baseCfg); err != nil {
		_ = e.store.ClearConfirmed()
		e.emit(EventConfirmedTimeout, fmt.Sprintf("confirmed 基线快照 rev %d 解析失败: %v", cf.BaseRev, err))
		return
	}
	// 追加回滚修订之前先取当前 committed（即未确认的超时配置），供底座差量回退。
	cur, curErr := e.committedLocked()
	now := e.now()
	if _, err := e.store.AppendRevision(baseJSON, now,
		fmt.Sprintf("commit confirmed 超时，自动回滚到 rev %d（FR-CFG-003）", cf.BaseRev)); err != nil {
		e.emit(EventConfirmedTimeout, fmt.Sprintf("自动回滚落库失败: %v", err))
		return
	}
	_ = e.store.ClearConfirmed()
	if e.confirmStop != nil {
		e.confirmStop()
		e.confirmStop = nil
	}

	// 回滚必须同样下发底座：超时前的 commit 已把配置生效到 VPP/libvirt/Docker，
	// 只落库不下发会让运行态无限期残留未确认配置（与 Commit 成功路径不对称）。
	// 下发失败时不回退 DB 修订（committed 已指向基线，恢复收敛以 DB 为准），
	// 但审计记 failure 并告警，等待 VPP 重连恢复/人工干预收敛。
	applyErr := curErr
	if applyErr == nil && e.applier != nil {
		if err := e.applier.Apply(context.Background(), cur, baseCfg); err != nil {
			applyErr = err
		}
	}
	if applyErr != nil {
		e.store.AppendAudit(AuditEntry{
			Time: now, User: "system", Action: "config.rollback-auto",
			Detail: fmt.Sprintf("commit confirmed 超时，已自动回滚（基线 rev %d），但底座回退失败: %v", cf.BaseRev, applyErr),
			Result: "failure",
		})
		e.emit(EventConfirmedTimeout, fmt.Sprintf(
			"commit confirmed 超时已回滚到 rev %d，但底座回退失败（等待恢复收敛）: %v", cf.BaseRev, applyErr))
	} else {
		e.store.AppendAudit(AuditEntry{
			Time: now, User: "system", Action: "config.rollback-auto",
			Detail: fmt.Sprintf("commit confirmed 超时未确认，已自动回滚（基线 rev %d）", cf.BaseRev), Result: "success",
		})
		e.emit(EventConfirmedTimeout, fmt.Sprintf("commit confirmed 超时，已自动回滚到 rev %d 并产生告警", cf.BaseRev))
	}

	// 持锁会话的 candidate 同步回滚后配置
	if e.candidate != nil {
		e.candidate = &baseCfg
		e.dirty = true
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
	e.store.AppendAudit(audit)
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
			errs = append(errs, model.ValidateError{Path: path, Message: fmt.Sprintf("仓库中不存在镜像 %q", image)})
			return
		}
		if info.Type != want {
			errs = append(errs, model.ValidateError{Path: path,
				Message: fmt.Sprintf("镜像类型不匹配（FR-CFG-011⑤）：需要 %s，实际 %s", want, info.Type)})
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
				out = append(out, fmt.Sprintf("警告: VM %s 的 vNIC %s 物理 NIC %s 位于 NUMA %d，与内存 NUMA %d 不一致，跨 NUMA 访存将降低性能（FR-CFG-011⑩）",
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

// sysMgmtChanged 判定管理口是否被变更（FR-CFG-012）：
// 基线已有管理口且新配置中地址/网关不同（含删除）时为 true。
func sysMgmtChanged(old, new *model.SystemConfig) bool {
	if old == nil || old.Management == nil {
		return false
	}
	bm := (*model.MgmtConfig)(nil)
	if new != nil {
		bm = new.Management
	}
	return !configEq(old.Management, bm)
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

// AuditTrail 返回最近 limit 条配置变更审计记录（show log audit / GET /audit-logs）。
func (e *Engine) AuditTrail(limit, offset int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	return e.store.ListAudit(limit, offset)
}

// Audit 追加一条运行态操作审计（FR-OPS-031：生命周期操作入审计通道）。
// 与配置变更审计（config.commit 等）同表，便于统一 `show log audit` 呈现。
func (e *Engine) Audit(user, action, detail, result string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.store.AppendAudit(AuditEntry{Time: e.now(), User: user, Action: action, Detail: detail, Result: result})
}
