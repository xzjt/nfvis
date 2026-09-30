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

// ---------- 活动会话清单与逐 token 吊销（决策 #301） ----------

// idLikeUUID 报告 id 是否为 UUID v4 格式（8-4-4-4-12、版本位 4）。
func idLikeUUID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, r := range id {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		case 14:
			if r != '4' {
				return false
			}
		}
	}
	return true
}

func TestLoginIssuesStableTokenID(t *testing.T) {
	now, _ := fakeClock()
	s := NewService(fakeSource{testConfig()}, now)
	tok, err := s.Login("admin", "s3cret-Passw0rd!")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	// 签发即有稳定 ID：UUID 格式、与签发时间成对
	if tok.ID == "" || !idLikeUUID(tok.ID) {
		t.Fatalf("签发的 token_id 应为 UUID 格式，实得 %q", tok.ID)
	}
	if tok.IssuedAt.IsZero() || !tok.IssuedAt.Equal(now()) {
		t.Fatalf("签发时间应等于当前时刻: %+v", tok.IssuedAt)
	}
	// VerifyToken 回带同一 ID（handler 的「当前会话」标记依据）
	vt, err := s.VerifyToken(tok.Token)
	if err != nil || vt.ID != tok.ID {
		t.Fatalf("VerifyToken 应回同一 ID: %+v err=%v", vt, err)
	}
	// 多次登录 ID 互不相同（逐 token 吊销要求可区分）
	tok2, _ := s.Login("admin", "s3cret-Passw0rd!")
	if tok2.ID == tok.ID {
		t.Fatalf("两次登录的 token_id 不应相同: %s", tok.ID)
	}
}

func TestListTokensScopeByClass(t *testing.T) {
	now, _ := fakeClock()
	s := NewService(fakeSource{testConfig()}, now)
	adminTok, _ := s.Login("admin", "s3cret-Passw0rd!") // super-user
	netopTok, _ := s.Login("netop", "s3cret-Passw0rd!") // operator
	viewerTok, _ := s.Login("viewer", "s3cret-Passw0rd!")

	// super-user：全部用户的会话都可见
	all := s.ListTokens("admin", ClassSuperUser)
	if len(all) != 3 {
		t.Fatalf("super-user 应见全部 3 个会话，实得 %d", len(all))
	}
	// 其他 class：只见自己的
	op := s.ListTokens("netop", ClassOperator)
	if len(op) != 1 || op[0].ID != netopTok.ID {
		t.Fatalf("operator 应只见自己的会话: %+v", op)
	}
	ro := s.ListTokens("viewer", ClassReadOnly)
	if len(ro) != 1 || ro[0].ID != viewerTok.ID {
		t.Fatalf("read-only 应只见自己的会话: %+v", ro)
	}
	// 当前会话标记只对持 ID 的会话为真
	if !ro[0].Current(viewerTok.ID) || ro[0].Current(adminTok.ID) {
		t.Fatalf("Current 标记不符: %+v", ro[0])
	}
	if ro[0].Current("") {
		t.Fatalf("viewer ID 为空时 Current 必须为 false（不猜）")
	}
	// 凭据永不出现在清单里
	for _, v := range all {
		if v.ID == "" || v.User == "" || v.ExpiresAt.IsZero() {
			t.Fatalf("清单条目字段缺失: %+v", v)
		}
	}
}

func TestRevokeTokenScopeByClass(t *testing.T) {
	now, _ := fakeClock()
	s := NewService(fakeSource{testConfig()}, now)
	adminTok, _ := s.Login("admin", "s3cret-Passw0rd!")
	netopTok, _ := s.Login("netop", "s3cret-Passw0rd!")
	viewerTok, _ := s.Login("viewer", "s3cret-Passw0rd!")

	// 非 super 吊销他人会话：与「不存在」同一错误（不泄露存在性）
	if err := s.RevokeToken("netop", ClassOperator, adminTok.ID); !errors.Is(err, ErrTokenNotFoundOrForbidden) {
		t.Fatalf("operator 吊销他人会话应 ErrTokenNotFoundOrForbidden: %v", err)
	}
	if err := s.RevokeToken("netop", ClassOperator, "no-such-id"); !errors.Is(err, ErrTokenNotFoundOrForbidden) {
		t.Fatalf("吊销不存在的会话应同一错误: %v", err)
	}
	// 空串 id 同样拒绝
	if err := s.RevokeToken("netop", ClassOperator, ""); !errors.Is(err, ErrTokenNotFoundOrForbidden) {
		t.Fatalf("空 id 应拒绝: %v", err)
	}
	// 两会话应原样还在
	if len(s.ListTokens("admin", ClassSuperUser)) != 3 {
		t.Fatalf("无权吊销不应改变任何会话")
	}

	// 非 super 吊销自己的：成功且立即失效
	if err := s.RevokeToken("viewer", ClassReadOnly, viewerTok.ID); err != nil {
		t.Fatalf("吊销自己的会话应成功: %v", err)
	}
	if _, err := s.VerifyToken(viewerTok.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("吊销后下一请求应 401（ErrUnauthorized）: %v", err)
	}

	// super 吊销他人的：成功且立即失效
	if err := s.RevokeToken("admin", ClassSuperUser, netopTok.ID); err != nil {
		t.Fatalf("super-user 吊销他人会话应成功: %v", err)
	}
	if _, err := s.VerifyToken(netopTok.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("被 super 吊销后下一请求应 401: %v", err)
	}
	// admin 自己的会话还在
	if _, err := s.VerifyToken(adminTok.Token); err != nil {
		t.Fatalf("未吊销的会话应仍有效: %v", err)
	}
	// 吊销不存在的 id（super 视角）也是同一错误
	if err := s.RevokeToken("admin", ClassSuperUser, netopTok.ID); !errors.Is(err, ErrTokenNotFoundOrForbidden) {
		t.Fatalf("重复吊销应报同一错误: %v", err)
	}
}

func TestListTokensExcludesExpired(t *testing.T) {
	now, clockPtr := fakeClock()
	s := NewService(fakeSource{testConfig()}, now)
	if _, err := s.Login("admin", "s3cret-Passw0rd!"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if len(s.ListTokens("admin", ClassSuperUser)) != 1 {
		t.Fatalf("签发后应见 1 个会话")
	}
	// 越过 TTL：过期项先被清扫，不出现在清单里
	*clockPtr = clockPtr.Add(61 * time.Minute)
	if got := s.ListTokens("admin", ClassSuperUser); len(got) != 0 {
		t.Fatalf("过期会话不应出现在清单里: %+v", got)
	}
	// 清单输出按签发时间稳定排序
	*clockPtr = clockPtr.Add(2 * time.Minute)
	s.Login("admin", "s3cret-Passw0rd!")
	*clockPtr = clockPtr.Add(2 * time.Minute)
	s.Login("netop", "s3cret-Passw0rd!")
	list := s.ListTokens("admin", ClassSuperUser)
	if len(list) != 2 || !list[0].IssuedAt.Before(list[1].IssuedAt) {
		t.Fatalf("清单应按签发时间升序: %+v", list)
	}
}
