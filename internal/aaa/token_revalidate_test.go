package aaa

// 决策 #506 / #507（round49）：token 必须随用户降权、删除、改口令失效。
//
// 本轮之前：签发时把 user/class **固化**进内存表（Login），VerifyToken 只查表、不核对当前
// committed 配置，也没有任何主动吊销路径——用户被删、被降权、口令被重置后，旧 token 在
// 其 60 分钟 TTL 内照常可用（高危及权）。
//
// 两条修法的单元守护：
//  1. **按当前配置重算身份**：VerifyToken 在确认 token 未过期后从 committed 配置取该用户，
//     用户不存在 ⇒ ErrUnauthorized；存在 ⇒ class 取 effectiveClass(cfg,u)（不取签发值）；
//  2. **主动吊销**：RevokeUserTokens(user) 吊销该用户全部活动会话并返回其稳定 ID 列表；
//     ActiveTokenID(id) 报告某稳定 ID 是否仍活动（面向「已握手终端」的复核）。

import (
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
)

// mutableSource 可替换快照的配置源：模拟「token 签发后配置被改（降权 / 删用户）」。
type mutableSource struct {
	mu  sync.Mutex
	cfg model.Config
}

func newMutableSource(cfg model.Config) *mutableSource { return &mutableSource{cfg: cfg} }

func (m *mutableSource) Committed() (model.Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg, nil
}

func (m *mutableSource) set(cfg model.Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = cfg
}

// TestVerifyTokenFollowsCurrentConfigClass 身份按**当前 committed 配置**重算：
// 签发后把用户的 class 改掉（模拟 CLI `set system login user <n> class <c>` 这条不经
// REST 主动吊销的路径），同一 token 的下一请求即以新 class 呈现——降权与升权两个方向都钉住。
func TestVerifyTokenFollowsCurrentConfigClass(t *testing.T) {
	src := newMutableSource(testConfig())
	now, _ := fakeClock()
	s := NewService(src, now)

	tok, err := s.Login("admin", "s3cret-Passw0rd!")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if tok.Class != ClassSuperUser {
		t.Fatalf("前置：admin 应为 super-user，实得 %q", tok.Class)
	}

	// 降权：admin → read-only
	downgraded := testConfig()
	downgraded.System.Login.Users[0].Class = ClassReadOnly
	src.set(downgraded)
	info, err := s.VerifyToken(tok.Token)
	if err != nil {
		t.Fatalf("降权后 token 仍应有效（只是权限变低）: %v", err)
	}
	if info.Class != ClassReadOnly {
		t.Fatalf("class 应取当前配置（read-only），实得签发时的 %q", info.Class)
	}
	if info.User != "admin" || info.ID != tok.ID {
		t.Fatalf("用户与稳定 ID 不变: %+v", info)
	}
	// 清单同源：会话清单里的 class 也应随配置更新（同一份事实，不留陈旧读数）。
	for _, v := range s.ListTokens("admin", ClassSuperUser) {
		if v.ID == tok.ID && v.Class != ClassReadOnly {
			t.Fatalf("清单里的 class 应随当前配置: %+v", v)
		}
	}

	// 升权：netop（operator）→ super-user
	netTok, err := s.Login("netop", "s3cret-Passw0rd!")
	if err != nil {
		t.Fatalf("Login netop: %v", err)
	}
	if netTok.Class != ClassOperator {
		t.Fatalf("前置：netop 应为 operator，实得 %q", netTok.Class)
	}
	upgraded := testConfig()
	upgraded.System.Login.Users[1].Class = ClassSuperUser
	src.set(upgraded)
	info, err = s.VerifyToken(netTok.Token)
	if err != nil {
		t.Fatalf("升权后 token 应有效: %v", err)
	}
	if info.Class != ClassSuperUser {
		t.Fatalf("升权后 class 应取当前配置: %q", info.Class)
	}
}

// TestVerifyTokenRejectsDeletedUser 用户被删除后，其旧 token 立即无效（不靠主动吊销兜底）。
func TestVerifyTokenRejectsDeletedUser(t *testing.T) {
	src := newMutableSource(testConfig())
	now, _ := fakeClock()
	s := NewService(src, now)

	adminTok, err := s.Login("admin", "s3cret-Passw0rd!")
	if err != nil {
		t.Fatalf("Login admin: %v", err)
	}
	netopTok, err := s.Login("netop", "s3cret-Passw0rd!")
	if err != nil {
		t.Fatalf("Login netop: %v", err)
	}

	// 删掉 admin（保留 netop），无任何主动吊销
	withoutAdmin := testConfig()
	withoutAdmin.System.Login.Users = withoutAdmin.System.Login.Users[1:]
	src.set(withoutAdmin)

	if _, err := s.VerifyToken(adminTok.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("用户已删除，旧 token 应 ErrUnauthorized: %v", err)
	}
	if _, err := s.VerifyToken(netopTok.Token); err != nil {
		t.Fatalf("其它用户的 token 不应受影响: %v", err)
	}
	// 用户被删除时**缺省 class 回退**不再适用：不得把「查不到的用户」当成 read-only 放行
	info, err := s.VerifyToken(netopTok.Token)
	if err != nil || info.Class != ClassOperator {
		t.Fatalf("在册用户仍按其 class: %+v err=%v", info, err)
	}
}

// TestRevokeUserTokens 主动吊销：按用户吊销全部活动会话，返回稳定 ID 列表（集合正确、顺序稳定）。
func TestRevokeUserTokens(t *testing.T) {
	src := newMutableSource(testConfig())
	now, _ := fakeClock()
	s := NewService(src, now)

	admin1, _ := s.Login("admin", "s3cret-Passw0rd!")
	admin2, _ := s.Login("admin", "s3cret-Passw0rd!")
	netop, _ := s.Login("netop", "s3cret-Passw0rd!")

	got := s.RevokeUserTokens("admin")
	want := []string{admin1.ID, admin2.ID}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("应吊销 admin 的 2 个会话，实得 %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("返回的 ID 集合不符（应稳定升序）: got=%v want=%v", got, want)
		}
	}
	// 两个 token 立即失效；他人不受影响
	for _, tok := range []*TokenInfo{admin1, admin2} {
		if _, err := s.VerifyToken(tok.Token); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("被吊销的 token 应 ErrUnauthorized: %v", err)
		}
	}
	if _, err := s.VerifyToken(netop.Token); err != nil {
		t.Fatalf("他人 token 不应受影响: %v", err)
	}
	if list := s.ListTokens("admin", ClassSuperUser); len(list) != 1 || list[0].User != "netop" {
		t.Fatalf("清单应只剩 netop 的会话: %+v", list)
	}
	// 重复吊销：空结果、不报错；空用户名同样安全（不误伤全体）
	if again := s.RevokeUserTokens("admin"); len(again) != 0 {
		t.Fatalf("重复吊销应返回空列表: %v", again)
	}
	if got := s.RevokeUserTokens(""); len(got) != 0 {
		t.Fatalf("空用户名不应吊销任何会话: %v", got)
	}
	if _, err := s.VerifyToken(netop.Token); err != nil {
		t.Fatalf("空用户名不得误伤其它会话: %v", err)
	}
}

// TestActiveTokenID 稳定 ID 的活动性复核（面向已握手终端的二次校验）：
// 空 ID 保守放行为真（无凭据来源可比，不拦）；已吊销 / 已过期 / 未知 ID 为假。
func TestActiveTokenID(t *testing.T) {
	now, clockPtr := fakeClock()
	s := NewService(newMutableSource(testConfig()), now)

	if !s.ActiveTokenID("") {
		t.Fatalf("空 ID 应保守放行（旧式登记无从核对）")
	}
	if s.ActiveTokenID("no-such-id") {
		t.Fatalf("未知 ID 不应视为活动")
	}
	tok, err := s.Login("admin", "s3cret-Passw0rd!")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !s.ActiveTokenID(tok.ID) {
		t.Fatalf("签发后应有活动会话")
	}
	// 主动吊销后不再活动
	s.RevokeUserTokens("admin")
	if s.ActiveTokenID(tok.ID) {
		t.Fatalf("被吊销的会话不应再视为活动")
	}
	// 过期（TTL 越界，随访问清扫）后不再活动
	tok2, err := s.Login("admin", "s3cret-Passw0rd!")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	*clockPtr = clockPtr.Add(61 * time.Minute)
	if s.ActiveTokenID(tok2.ID) {
		t.Fatalf("过期会话不应再视为活动")
	}
}
