package aaa

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/schema"
)

type fakeSource struct{ cfg model.Config }

func (f fakeSource) Committed() (model.Config, error) { return f.cfg, nil }

func fakeClock() (func() time.Time, *time.Time) {
	t := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t }, &t
}

func testConfig() model.Config {
	hash, _ := HashPassword("s3cret-Passw0rd!")
	return model.Config{
		System: &model.SystemConfig{
			Login: &model.SystemLogin{
				Users: []model.LoginUserConfig{
					{Name: "admin", PasswordHash: hash, Class: ClassSuperUser},
					{Name: "netop", PasswordHash: hash, Class: ClassOperator},
					{Name: "viewer", PasswordHash: hash},
				},
				Classes: []model.ClassDef{
					{Name: "netops", Allow: []string{"show", "request virtual-machine-functions"}},
					{Name: "limited", Allow: []string{"show"}, Deny: []string{"show alarms"}},
				},
			},
		},
	}
}

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("s3cret-Passw0rd!")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "pbkdf2$sha256$600000$") {
		t.Fatalf("哈希格式不符: %s", hash)
	}
	if !VerifyPassword(hash, "s3cret-Passw0rd!") {
		t.Fatalf("正确口令应通过校验")
	}
	if VerifyPassword(hash, "wrong") {
		t.Fatalf("错误口令不应通过")
	}
	if VerifyPassword("garbage", "x") {
		t.Fatalf("畸形哈希应返回 false")
	}
	// 同一口令两次加盐结果不同
	h2, _ := HashPassword("s3cret-Passw0rd!")
	if h2 == hash {
		t.Fatalf("随机盐应产生不同哈希")
	}
}

func TestCheckPasswordPolicy(t *testing.T) {
	if bad := CheckPasswordPolicy("longenough1", nil); len(bad) != 0 {
		t.Fatalf("默认策略（仅长度）不应违规: %v", bad)
	}
	if bad := CheckPasswordPolicy("short", nil); len(bad) == 0 {
		t.Fatalf("短口令应违规")
	}
	p := &model.PasswordPolicy{MinLength: 12, Complexity: true}
	if bad := CheckPasswordPolicy("onlylowercase12", p); len(bad) == 0 {
		t.Fatalf("复杂度不足应违规")
	}
	if bad := CheckPasswordPolicy("Aa1!aaaa", p); len(bad) == 0 {
		t.Fatalf("长度 %d < 12 应违规", len("Aa1!aaaa"))
	}
	if bad := CheckPasswordPolicy("Aa1!aaaaaaaa", p); len(bad) != 0 {
		t.Fatalf("合规口令不应违规: %v", bad)
	}
}

func TestLoginSuccessAndDefaults(t *testing.T) {
	now, clockPtr := fakeClock()
	s := NewService(fakeSource{testConfig()}, now)

	tok, err := s.Login("admin", "s3cret-Passw0rd!")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if tok.User != "admin" || tok.Class != ClassSuperUser {
		t.Fatalf("身份不符: %+v", tok)
	}
	if !tok.ExpiresAt.Equal(now().Add(defaultTokenTTL)) {
		t.Fatalf("默认 TTL 60 分钟: %+v", tok.ExpiresAt)
	}
	// viewer 未配 class → 缺省 read-only
	vt, err := s.Login("viewer", "s3cret-Passw0rd!")
	if err != nil || vt.Class != ClassReadOnly {
		t.Fatalf("缺省 class 应为 read-only: %+v err=%v", vt, err)
	}
	_ = clockPtr
}

func TestLoginFailures(t *testing.T) {
	s := NewService(fakeSource{testConfig()}, nil)
	if _, err := s.Login("ghost", "x"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("未知用户应 ErrInvalidCredentials: %v", err)
	}
	empty := model.Config{System: &model.SystemConfig{Login: &model.SystemLogin{}}}
	s2 := NewService(fakeSource{empty}, nil)
	if _, err := s2.Login("admin", "x"); !errors.Is(err, ErrNoUsers) {
		t.Fatalf("无用户应 ErrNoUsers: %v", err)
	}
}

func TestLockout(t *testing.T) {
	now, clockPtr := fakeClock()
	policyCfg := testConfig()
	policyCfg.System.Login.PasswordPolicy = &model.PasswordPolicy{LockoutThreshold: 3, LockoutMinutes: 10}
	s2 := NewService(fakeSource{policyCfg}, now)

	for i := 0; i < 3; i++ {
		if _, err := s2.Login("admin", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("第 %d 次失败应 ErrInvalidCredentials: %v", i+1, err)
		}
	}
	// 达阈值：正确口令也拒绝
	if _, err := s2.Login("admin", "s3cret-Passw0rd!"); !errors.Is(err, ErrLocked) {
		t.Fatalf("锁定后应 ErrLocked: %v", err)
	}
	// 其他用户不受影响
	if _, err := s2.Login("netop", "s3cret-Passw0rd!"); err != nil {
		t.Fatalf("其他用户不应被锁定: %v", err)
	}
	// 锁定过期后恢复
	*clockPtr = clockPtr.Add(11 * time.Minute)
	if _, err := s2.Login("admin", "s3cret-Passw0rd!"); err != nil {
		t.Fatalf("锁定过期后应可登录: %v", err)
	}
}

func TestTokenLifecycle(t *testing.T) {
	now, clockPtr := fakeClock()
	s := NewService(fakeSource{testConfig()}, now)
	tok, err := s.Login("admin", "s3cret-Passw0rd!")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if _, err := s.VerifyToken(tok.Token); err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	s.Logout(tok.Token)
	if _, err := s.VerifyToken(tok.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("吊销后应 ErrUnauthorized: %v", err)
	}

	// TTL 过期
	tok2, _ := s.Login("admin", "s3cret-Passw0rd!")
	*clockPtr = clockPtr.Add(61 * time.Minute)
	if _, err := s.VerifyToken(tok2.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("过期 token 应 ErrUnauthorized: %v", err)
	}
}

func TestTokenTTLFromConfig(t *testing.T) {
	cfg := testConfig()
	cfg.System.API = &model.APIConfig{TokenTTLMinutes: 15}
	now, _ := fakeClock()
	s := NewService(fakeSource{cfg}, now)
	tok, err := s.Login("admin", "s3cret-Passw0rd!")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !tok.ExpiresAt.Equal(now().Add(15 * time.Minute)) {
		t.Fatalf("TTL 应取配置 15 分钟: %+v", tok.ExpiresAt)
	}
}

func TestAuthorizePresets(t *testing.T) {
	s := NewService(fakeSource{testConfig()}, nil)
	cases := []struct {
		class    string
		required schema.Class
		want     bool
	}{
		{ClassSuperUser, schema.ClassSuperUser, true},
		{ClassSuperUser, schema.ClassReadOnly, true},
		{ClassOperator, schema.ClassOperator, true},
		{ClassOperator, schema.ClassSuperUser, false},
		{ClassOperator, schema.ClassReadOnly, true},
		{ClassReadOnly, schema.ClassReadOnly, true},
		{ClassReadOnly, schema.ClassOperator, false},
	}
	for _, c := range cases {
		if got := s.Authorize(c.class, c.required); got != c.want {
			t.Errorf("class=%s required=%v: got %v want %v", c.class, c.required, got, c.want)
		}
	}
}

func TestAuthorizeCustomClass(t *testing.T) {
	s := NewService(fakeSource{testConfig()}, nil)
	// netops：允许 show 与 request virtual-machine-functions 前缀
	if !s.Authorize("netops", schema.ClassReadOnly, "show", "version") {
		t.Fatalf("allow 前缀应放行")
	}
	_ = schema.ClassSuperUser // 自定义 class 为纯路径 ACL（见 Authorize 文档），等级由 allow 表显式决定
	if s.Authorize("netops", schema.ClassReadOnly, "configure") {
		t.Fatalf("未 allow 的路径应默认拒绝")
	}
	if !s.Authorize("netops", schema.ClassReadOnly, "request", "virtual-machine-functions", "fw-vm", "start") {
		t.Fatalf("多 token 前缀应匹配")
	}
	// limited：deny 优先于 allow
	if s.Authorize("limited", schema.ClassReadOnly, "show", "alarms") {
		t.Fatalf("deny 前缀应优先拒绝")
	}
	if !s.Authorize("limited", schema.ClassReadOnly, "show", "version") {
		t.Fatalf("deny 之外的 allow 路径应放行")
	}
	// 未知 class 一律拒绝
	if s.Authorize("ghost", schema.ClassReadOnly, "show") {
		t.Fatalf("未知 class 应拒绝")
	}
}

func TestChangePassword(t *testing.T) {
	s := NewService(fakeSource{testConfig()}, nil)
	if err := s.ChangePassword("admin", "wrong", "NewPassw0rd!x"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("旧口令错误应拒绝: %v", err)
	}
	if err := s.ChangePassword("admin", "s3cret-Passw0rd!", "short"); err == nil {
		t.Fatalf("新口令不满足策略应拒绝")
	}
	if err := s.ChangePassword("admin", "s3cret-Passw0rd!", "NewPassw0rd!x"); err != nil {
		t.Fatalf("合规修改应通过: %v", err)
	}
}

func TestEnsureBootstrapAdmin(t *testing.T) {
	now, _ := fakeClock()
	store := newBootstrapStore(t)
	e := newBootstrapEngine(t, store, now)
	s := NewService(e, now)

	// 空配置：引导创建 admin
	created, oneTime, err := EnsureBootstrapAdmin(e, s, "InitPassw0rd!")
	if err != nil || !created {
		t.Fatalf("首次引导应创建 admin: created=%v err=%v", created, err)
	}
	if oneTime != "" {
		t.Fatalf("显式口令不应返回一次性口令: %q", oneTime)
	}
	if _, err := s.Login("admin", "InitPassw0rd!"); err != nil {
		t.Fatalf("引导后应可登录: %v", err)
	}
	// 已有用户：不再引导
	created2, _, err := EnsureBootstrapAdmin(e, s, "OtherPassw0rd!")
	if err != nil || created2 {
		t.Fatalf("已有用户不应重复引导: created=%v err=%v", created2, err)
	}
	// 弱口令：拒绝
	e2 := newBootstrapEngine(t, newBootstrapStore(t), now)
	s2 := NewService(e2, now)
	if _, _, err := EnsureBootstrapAdmin(e2, s2, "short"); err == nil {
		t.Fatalf("弱引导口令应被策略拒绝")
	}
	// 随机口令：生成并仅返回一次
	e3 := newBootstrapEngine(t, newBootstrapStore(t), now)
	s3 := NewService(e3, now)
	created3, oneTime3, err := EnsureBootstrapAdmin(e3, s3, "")
	if err != nil || !created3 || oneTime3 == "" {
		t.Fatalf("随机口令引导: created=%v oneTime=%q err=%v", created3, oneTime3, err)
	}
	if _, err := s3.Login("admin", oneTime3); err != nil {
		t.Fatalf("随机口令应可登录: %v", err)
	}
}

// ---------- 引导测试的装配辅助（依赖事务引擎） ----------

func newBootstrapStore(t *testing.T) *config.Store {
	t.Helper()
	s, err := config.OpenStore(filepath.Join(t.TempDir(), "nfvis.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newBootstrapEngine(t *testing.T, store *config.Store, now func() time.Time) *config.Engine {
	t.Helper()
	e, err := config.NewEngine(store, orchestrator.NewNoopApplier(), config.Options{Now: now})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}
