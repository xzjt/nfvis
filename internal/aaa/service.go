// Package aaa 实现本地认证/授权/账号（AAA，FR-SEC-002/003/005/008、FR-API-001）。
//
// 用户/class/口令策略声明式存于配置文档 system.login（附录 A #25），本包从
// committed 配置实时读取；口令仅存加盐 PBKDF2-SHA256 哈希。连续失败锁定与
// API Token 为运行态（内存），nfvisd 重启后 token 失效、锁定清零。
package aaa

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/schema"
)

// AAA 错误（映射 API 状态码）。
var (
	ErrInvalidCredentials = errors.New("用户名或口令错误")
	ErrNoUsers            = errors.New("系统尚未初始化本地用户")
	ErrLocked             = errors.New("连续失败次数过多，账号已锁定")
	ErrUnauthorized       = errors.New("token 无效或已过期")
	ErrForbidden          = errors.New("无权限执行该操作")
)

// 预置 login class（命令树 §4 权限矩阵）。名称取自 schema 的单一事实源（决策 #324）——
// 等级映射（名称→schema.Class）也由 schema.PresetClassLevel 给出，本包不再各写一份。
const (
	ClassSuperUser = schema.ClassNameSuperUser
	ClassOperator  = schema.ClassNameOperator
	ClassReadOnly  = schema.ClassNameReadOnly
)

const (
	pbkdf2Iterations = 600000
	pbkdf2KeyLen     = 32
	tokenBytes       = 32

	defaultTokenTTL    = 60 * time.Minute
	defaultLockoutN    = 5
	defaultLockoutMins = 10
	defaultMinLength   = 8
)

// ---------- 口令哈希（附录 A #25） ----------

// HashPassword 生成加盐 PBKDF2-SHA256 哈希：
// pbkdf2$sha256$<iter>$<b64salt>$<b64hash>。
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成盐: %w", err)
	}
	dk, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iterations, pbkdf2KeyLen)
	if err != nil {
		return "", fmt.Errorf("派生口令哈希: %w", err)
	}
	return fmt.Sprintf("pbkdf2$sha256$%d$%s$%s",
		pbkdf2Iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk)), nil
}

// VerifyPassword 常量时间校验口令与哈希。
func VerifyPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 5 || parts[0] != "pbkdf2" || parts[1] != "sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[2])
	if err != nil || iter < 1 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	if err != nil {
		return false
	}
	return hmac.Equal(got, want)
}

// ---------- 口令策略（FR-SEC-003） ----------

// CheckPasswordPolicy 按配置策略校验新口令，返回违规项列表（空 = 合规）。
// 默认：长度 ≥8；complexity 开启时要求至少 3/4 类字符。
func CheckPasswordPolicy(pw string, p *model.PasswordPolicy) []string {
	var bad []string
	minLen := defaultMinLength
	complexity := false
	if p != nil {
		if p.MinLength > 0 {
			minLen = p.MinLength
		}
		complexity = p.Complexity
	}
	if len(pw) < minLen {
		bad = append(bad, fmt.Sprintf("长度不足 %d 字符", minLen))
	}
	if complexity {
		var hasUpper, hasLower, hasDigit, hasSpecial bool
		for _, r := range pw {
			switch {
			case unicode.IsUpper(r):
				hasUpper = true
			case unicode.IsLower(r):
				hasLower = true
			case unicode.IsDigit(r):
				hasDigit = true
			case unicode.IsPunct(r) || unicode.IsSymbol(r):
				hasSpecial = true
			}
		}
		kinds := 0
		for _, b := range []bool{hasUpper, hasLower, hasDigit, hasSpecial} {
			if b {
				kinds++
			}
		}
		if kinds < 3 {
			bad = append(bad, "复杂度不足（需至少 3/4 类：大写/小写/数字/特殊字符）")
		}
	}
	return bad
}

// ---------- 服务 ----------

// ConfigSource 提供 committed 配置读取（由事务引擎实现）。
type ConfigSource interface {
	Committed() (model.Config, error)
}

// TokenInfo 已验证 token 的身份信息。
type TokenInfo struct {
	Token     string
	ID        string // token 稳定 ID（UUID；仅内存保存，与 token 同生命周期，决策 #301）
	User      string
	Class     string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// TokenView 活动会话清单条目（决策 #301）：不含 token 本体（凭据永不回显），
// 供 ListTokens 输出与 REST GET /system/api-tokens 使用。
type TokenView struct {
	ID        string    `json:"token_id"`
	User      string    `json:"user"`
	Class     string    `json:"class"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Current 报告该会话是否为 viewerID 发起请求的会话（决策 #301 的「当前会话」标记；
// viewerID 为空串时恒 false——调用方没有稳定 ID 可比，不猜）。
func (v TokenView) Current(viewerID string) bool {
	return viewerID != "" && v.ID == viewerID
}

type tokenEntry struct {
	id        string
	user      string
	class     string
	issuedAt  time.Time
	expiresAt time.Time
}

type lockoutState struct {
	failures    int
	lockedUntil time.Time
}

// Service 本地 AAA 服务。
type Service struct {
	mu       sync.Mutex
	src      ConfigSource
	now      func() time.Time
	tokens   map[string]tokenEntry
	lockouts map[string]*lockoutState
}

// NewService 构造 AAA 服务。
func NewService(src ConfigSource, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{
		src:      src,
		now:      now,
		tokens:   map[string]tokenEntry{},
		lockouts: map[string]*lockoutState{},
	}
}

// Login 认证并签发 token（FR-API-001/FR-SEC-003/005）。
// 连续失败达阈值锁定账号；成功后失败计数清零。
func (s *Service) Login(user, password string) (*TokenInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()

	now := s.now()
	if st := s.lockouts[user]; st != nil && now.Before(st.lockedUntil) {
		return nil, fmt.Errorf("%w：%s 前禁止登录", ErrLocked, st.lockedUntil.Format("15:04:05"))
	}

	cfg, err := s.src.Committed()
	if err != nil {
		return nil, err
	}
	if len(loginUsers(cfg)) == 0 {
		return nil, ErrNoUsers
	}
	u := findUser(cfg, user)
	if u == nil {
		s.recordFailure(user, now)
		return nil, ErrInvalidCredentials
	}
	if u.PasswordHash == "" {
		return nil, fmt.Errorf("用户 %s 未设置口令", user)
	}
	if !VerifyPassword(u.PasswordHash, password) {
		s.recordFailure(user, now)
		return nil, ErrInvalidCredentials
	}
	delete(s.lockouts, user)

	entry := tokenEntry{
		user:      u.Name,
		class:     effectiveClass(cfg, u),
		issuedAt:  now,
		expiresAt: now.Add(s.tokenTTL(cfg)),
	}
	tok, err := randomToken()
	if err != nil {
		return nil, err
	}
	id, err := newTokenID()
	if err != nil {
		return nil, err
	}
	entry.id = id
	s.tokens[tok] = entry
	return &TokenInfo{Token: tok, ID: entry.id, User: entry.user, Class: entry.class, IssuedAt: entry.issuedAt, ExpiresAt: entry.expiresAt}, nil
}

// Logout 吊销 token（FR-API-001 可吊销）。
func (s *Service) Logout(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, token)
}

// VerifyToken 校验 token 并返回身份（FR-SEC-002：token 与用户绑定且继承其权限）。
func (s *Service) VerifyToken(token string) (*TokenInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	entry, ok := s.tokens[token]
	if !ok {
		return nil, ErrUnauthorized
	}
	return &TokenInfo{Token: token, ID: entry.id, User: entry.user, Class: entry.class, IssuedAt: entry.issuedAt, ExpiresAt: entry.expiresAt}, nil
}

// ---------- 活动会话清单与逐 token 吊销（决策 #301） ----------

// ErrTokenNotFoundOrForbidden 吊销/定位一个「不存在、或存在但 viewer 无权操作」的会话。
// 两种情况共用同一错误与文案：非 super-user 的调用方不能借响应差异探测他人会话是否存在。
var ErrTokenNotFoundOrForbidden = errors.New("会话不存在或无权操作该会话")

// isSuperUser 报告 class 是否为预置 super-user（活动会话的全量可见/全权吊销判据）。
// 自定义 class 一律按「仅自己」处理——它没有全量凭据的语义。
func isSuperUser(class string) bool { return class == ClassSuperUser }

// ListTokens 列出活动会话（决策 #301）。
//
// 权限矩阵：super-user 列出**全部用户**的 token；其他 class（含自定义 class）只列自己的。
// 已过 TTL 的会话先被清扫，不会出现在清单里。输出按签发时间升序（同刻按 ID 排），
// 保证同一状态多次读取的输出稳定。
func (s *Service) ListTokens(viewerUser, viewerClass string) []TokenView {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	all := isSuperUser(viewerClass)
	out := make([]TokenView, 0, len(s.tokens))
	for _, e := range s.tokens {
		if !all && e.user != viewerUser {
			continue
		}
		out = append(out, TokenView{ID: e.id, User: e.user, Class: e.class, IssuedAt: e.issuedAt, ExpiresAt: e.expiresAt})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].IssuedAt.Equal(out[j].IssuedAt) {
			return out[i].IssuedAt.Before(out[j].IssuedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// RevokeToken 吊销指定 ID 的活动会话（决策 #301）。
//
// 权限矩阵：super-user 可吊销任意 token；其他 class 只能吊销自己的。
// 「不存在」与「存在但无权操作」一律返回 ErrTokenNotFoundOrForbidden——同一错误不泄露存在性。
// 吊销立即生效：该 token 的下一次 VerifyToken 返回 ErrUnauthorized。
func (s *Service) RevokeToken(viewerUser, viewerClass, id string) error {
	if id == "" {
		return ErrTokenNotFoundOrForbidden
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	for tok, e := range s.tokens {
		if e.id != id {
			continue
		}
		if !isSuperUser(viewerClass) && e.user != viewerUser {
			return ErrTokenNotFoundOrForbidden
		}
		delete(s.tokens, tok)
		return nil
	}
	return ErrTokenNotFoundOrForbidden
}

// ---------- 生效权限视图的判定查询（决策 #304） ----------
//
// 判定逻辑的**单一事实源**：运行期逐命令授权（Authorize）与生效权限视图
// （CLI `show configuration permissions <class>` / REST `GET /configuration/permissions`）
// 共用同一套函数。视图侧只暴露「解析 class 定义 + 逐路径判定 + 依据」的查询能力，
// **不得另写一套判定**（本决策要点）。

// ClassSource 生效权限视图里 class 的来源。
type ClassSource string

const (
	ClassSourcePreset ClassSource = "preset" // 预置三档（super-user/operator/read-only）
	ClassSourceCustom ClassSource = "custom" // 配置里自定义的 class（allow/deny 前缀表）
)

// 判定依据（视图逐路径展示；四类允许/拒绝理由 + 预置等级不足这一反例）。
const (
	ReasonPresetSatisfied    = "预置等级满足"
	ReasonPresetInsufficient = "预置等级不足"
	ReasonAllowPrefixHit     = "allow 前缀命中"
	ReasonDenyPrefixHit      = "deny 前缀命中"
	ReasonDefaultDeny        = "默认拒绝"
)

// ClassDefView class 的生效定义：预置档只有名称；自定义 class 带 allow/deny 前缀表。
type ClassDefView struct {
	Name   string
	Source ClassSource
	Allow  []string
	Deny   []string
}

// ResolveClass 解析 class 定义（决策 #304）。预置三档恒存在；自定义从 committed 配置的
// system.login.classes 读；未知 class（非预置、配置里也没有）返回 ok=false——视图侧据此
// 明确报错，而不是拿「默认拒绝」冒充一个真实存在的 class。
func (s *Service) ResolveClass(name string) (ClassDefView, bool) {
	switch name {
	case ClassSuperUser, ClassOperator, ClassReadOnly:
		return ClassDefView{Name: name, Source: ClassSourcePreset}, true
	}
	allow, deny, found := s.customClassRules(name)
	if !found {
		return ClassDefView{}, false
	}
	return ClassDefView{Name: name, Source: ClassSourceCustom, Allow: allow, Deny: deny}, true
}

// Evaluate 判定该 class 对 (required, path) 的授权并给出依据（决策 #304）。
// 与运行期 Authorize **同一实现**：预置档按等级；自定义按 deny→allow→默认拒绝。
func (d ClassDefView) Evaluate(required schema.Class, path ...string) (bool, string) {
	if d.Source == ClassSourcePreset {
		return evaluatePreset(d.Name, required)
	}
	return evaluateClassRules(d.Allow, d.Deny, path)
}

// Authorize 判定 class 是否获得授权（FR-SEC-002）。
//
// 预置 class（super-user/operator/read-only）按命令树 §4 权限矩阵的等级判定：
// 用户 class 能力等级须覆盖操作所需等级 required。
// 自定义 class 为纯路径 ACL：deny 前缀优先拒绝，allow 前缀放行，其余默认拒绝
// ——授权完全由管理员显式配置的路径表决定（与 required 等级正交）。
//
// 与 ResolveClass/ClassDefView.Evaluate 共用同一套判定函数（决策 #304 重构，
// 行为逐字不变，由既有 aaa 单测兜底）；预置档的**等级映射**自决策 #324 起取自
// schema.PresetClassLevel（命令树单一事实源），本包不再各写一份名称→等级的表。
func (s *Service) Authorize(cfgClass string, required schema.Class, path ...string) bool {
	if lvl, ok := schema.PresetClassLevel(cfgClass); ok {
		return lvl.Covers(required)
	}
	allow, deny, found := s.customClassRules(cfgClass)
	if !found {
		return false
	}
	ok, _ := evaluateClassRules(allow, deny, path)
	return ok
}

// customClassRules 从 committed 配置读自定义 class 的 allow/deny 前缀表
// （决策 #304 从 Authorize 抽出，与原逻辑逐字等价）。
func (s *Service) customClassRules(name string) (allow, deny []string, found bool) {
	cfg, err := s.src.Committed()
	if err != nil {
		return nil, nil, false
	}
	if cfg.System == nil || cfg.System.Login == nil {
		return nil, nil, false
	}
	for _, c := range cfg.System.Login.Classes {
		if c.Name == name {
			return c.Allow, c.Deny, true
		}
	}
	return nil, nil, false
}

// evaluatePreset 预置 class 的等级判定 + 依据（与 Authorize 逐字等价；等级映射单源在
// schema.PresetClassLevel，决策 #324）。
func evaluatePreset(name string, required schema.Class) (bool, string) {
	lvl, ok := schema.PresetClassLevel(name)
	if !ok {
		return false, ReasonPresetInsufficient
	}
	if lvl.Covers(required) {
		return true, ReasonPresetSatisfied
	}
	return false, ReasonPresetInsufficient
}

// evaluateClassRules 自定义 class 的路径前缀判定 + 依据（deny 优先、allow 放行、
// 其余默认拒绝；与 Authorize 原自定义分支逐字等价）。
func evaluateClassRules(allow, deny []string, path []string) (bool, string) {
	full := strings.Join(path, " ")
	for _, d := range deny {
		if matchPrefix(full, d) {
			return false, ReasonDenyPrefixHit
		}
	}
	for _, a := range allow {
		if matchPrefix(full, a) {
			return true, ReasonAllowPrefixHit
		}
	}
	return false, ReasonDefaultDeny
}

// ChangePassword 校验旧口令与新口令策略（FR-SEC-008）。配置变更本身由
// API 层经事务引擎完成（单请求直提并入审计）。
func (s *Service) ChangePassword(user, oldPassword, newPassword string) error {
	cfg, err := s.src.Committed()
	if err != nil {
		return err
	}
	u := findUser(cfg, user)
	if u == nil || u.PasswordHash == "" || !VerifyPassword(u.PasswordHash, oldPassword) {
		return ErrInvalidCredentials
	}
	if bad := CheckPasswordPolicy(newPassword, loginPolicy(cfg)); len(bad) > 0 {
		return fmt.Errorf("新口令不满足策略：%s", strings.Join(bad, "；"))
	}
	return nil
}

// HasUsers 报告系统是否已存在本地用户（首次启动引导判定）。
func (s *Service) HasUsers() (bool, error) {
	cfg, err := s.src.Committed()
	if err != nil {
		return false, err
	}
	return len(loginUsers(cfg)) > 0, nil
}

// ---------- 内部 ----------

func (s *Service) recordFailure(user string, now time.Time) {
	st := s.lockouts[user]
	if st == nil {
		st = &lockoutState{}
		s.lockouts[user] = st
	}
	st.failures++
	cfg, err := s.src.Committed()
	threshold, minutes := defaultLockoutN, defaultLockoutMins
	if err == nil {
		if p := loginPolicy(cfg); p != nil {
			if p.LockoutThreshold > 0 {
				threshold = p.LockoutThreshold
			}
			if p.LockoutMinutes > 0 {
				minutes = p.LockoutMinutes
			}
		}
	}
	if st.failures >= threshold {
		st.lockedUntil = now.Add(time.Duration(minutes) * time.Minute)
		st.failures = 0
	}
}

func (s *Service) sweepLocked() {
	now := s.now()
	for t, e := range s.tokens {
		if now.After(e.expiresAt) {
			delete(s.tokens, t)
		}
	}
	for u, st := range s.lockouts {
		if !now.Before(st.lockedUntil) && st.lockedUntil != (time.Time{}) {
			delete(s.lockouts, u)
		}
	}
}

func (s *Service) tokenTTL(cfg model.Config) time.Duration {
	if cfg.System != nil && cfg.System.API != nil && cfg.System.API.TokenTTLMinutes > 0 {
		return time.Duration(cfg.System.API.TokenTTLMinutes) * time.Minute
	}
	return defaultTokenTTL
}

func loginUsers(cfg model.Config) []model.LoginUserConfig {
	if cfg.System == nil || cfg.System.Login == nil {
		return nil
	}
	return cfg.System.Login.Users
}

func loginPolicy(cfg model.Config) *model.PasswordPolicy {
	if cfg.System == nil || cfg.System.Login == nil {
		return nil
	}
	return cfg.System.Login.PasswordPolicy
}

func findUser(cfg model.Config, name string) *model.LoginUserConfig {
	if cfg.System == nil || cfg.System.Login == nil {
		return nil
	}
	for i := range cfg.System.Login.Users {
		if cfg.System.Login.Users[i].Name == name {
			return &cfg.System.Login.Users[i]
		}
	}
	return nil
}

// effectiveClass 解析用户归属 class（缺省 read-only，FR-SEC-002）。
func effectiveClass(cfg model.Config, u *model.LoginUserConfig) string {
	if u.Class != "" {
		return u.Class
	}
	return ClassReadOnly
}

// matchPrefix 命令路径前缀匹配（按 token 边界："show" 匹配 "show version"
// 但不匹配 "showx"；"virtual-switches vs1" 匹配其子路径）。
func matchPrefix(path, prefix string) bool {
	if prefix == "" {
		return false
	}
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	if len(path) == len(prefix) {
		return true
	}
	return path[len(prefix)] == ' '
}

func randomToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成 token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// newTokenID 生成会话稳定 ID（UUID v4 格式字符串，决策 #301）。
// 仅内存保存、与 token 同生命周期：不落库、不参与认证（认证只认 token 本体），
// 列表与逐 token 吊销以它为键。
func newTokenID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成会话 ID: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
