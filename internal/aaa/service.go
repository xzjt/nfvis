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

// 预置 login class（命令树 §4 权限矩阵）。
const (
	ClassSuperUser = "super-user"
	ClassOperator  = "operator"
	ClassReadOnly  = "read-only"
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

// dummyHash 一次性生成的假口令哈希：用户不存在或未设口令时也执行一次等价的
// PBKDF2 校验，抹平响应时间差，防止通过时间侧信道枚举有效用户名。
var dummyHash = sync.OnceValue(func() string {
	h, err := HashPassword("nfvis-timing-equalizer")
	if err != nil {
		return ""
	}
	return h
})

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
	User      string
	Class     string
	ExpiresAt time.Time
}

type tokenEntry struct {
	user      string
	class     string
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
		VerifyPassword(dummyHash(), password) // 抹平时间差：与真实用户路径等价 PBKDF2 开销
		s.recordFailure(user, now)
		return nil, ErrInvalidCredentials
	}
	if u.PasswordHash == "" {
		VerifyPassword(dummyHash(), password) // 同上：不泄露"该用户未设口令"的时差
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
		expiresAt: now.Add(s.tokenTTL(cfg)),
	}
	tok, err := randomToken()
	if err != nil {
		return nil, err
	}
	s.tokens[tok] = entry
	return &TokenInfo{Token: tok, User: entry.user, Class: entry.class, ExpiresAt: entry.expiresAt}, nil
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
	return &TokenInfo{Token: token, User: entry.user, Class: entry.class, ExpiresAt: entry.expiresAt}, nil
}

// Authorize 判定 class 是否获得授权（FR-SEC-002）。
//
// 预置 class（super-user/operator/read-only）按命令树 §4 权限矩阵的等级判定：
// 用户 class 能力等级须覆盖操作所需等级 required。
// 自定义 class 为纯路径 ACL：deny 前缀优先拒绝，allow 前缀放行，其余默认拒绝
// ——授权完全由管理员显式配置的路径表决定（与 required 等级正交）。
func (s *Service) Authorize(cfgClass string, required schema.Class, path ...string) bool {
	switch cfgClass {
	case ClassSuperUser:
		return true
	case ClassOperator:
		return required <= schema.ClassOperator
	case ClassReadOnly:
		return required == schema.ClassReadOnly
	}
	// 自定义 class：查 committed 配置中的 allow/deny 前缀表
	cfg, err := s.src.Committed()
	if err != nil {
		return false
	}
	if cfg.System == nil || cfg.System.Login == nil {
		return false
	}
	full := strings.Join(path, " ")
	for _, c := range cfg.System.Login.Classes {
		if c.Name != cfgClass {
			continue
		}
		for _, d := range c.Deny {
			if matchPrefix(full, d) {
				return false
			}
		}
		for _, a := range c.Allow {
			if matchPrefix(full, a) {
				return true
			}
		}
		return false // 自定义 class 默认拒绝
	}
	return false
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
