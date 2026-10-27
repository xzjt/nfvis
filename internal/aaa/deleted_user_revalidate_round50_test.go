package aaa

// 决策 #513（round50）：纵深防御——活动会话清单与终端复核都要按**当前 committed**核对
// 用户是否仍在册。
//
// 由来：主动吊销只挂在 REST 的用户 handler 三处；CLI `delete/set system login user` + commit、
// load/restore、zeroize 等整文档提交不触发它。VerifyToken 只保护后续 HTTP 请求，而已升级的
// 串口/VNC/容器 shell 是长连接（不再发 HTTP），`ActiveTokenID` 又只查内存表 ⇒ 被删用户的容器
// shell 仍可用、`show system api tokens` 仍把它列为活动。装配层的收尾是第一道，这里两道读视图
// 复核是第二道。

import (
	"errors"
	"testing"
)

// TestActiveTokenIDRejectsDeletedUser 用户被删除（无任何主动吊销）后：
// 其 token 既过不了 VerifyToken，也报告为「非活动」——终端复核因此会关掉它。
func TestActiveTokenIDRejectsDeletedUser(t *testing.T) {
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

	if !s.ActiveTokenID(adminTok.ID) {
		t.Fatalf("前置：在册用户的会话应为活动")
	}

	// 删掉 admin（保留 netop），不做任何主动吊销
	withoutAdmin := testConfig()
	withoutAdmin.System.Login.Users = withoutAdmin.System.Login.Users[1:]
	src.set(withoutAdmin)

	if s.ActiveTokenID(adminTok.ID) {
		t.Fatalf("用户已删除，其会话不应再报告为活动（终端复核据此关闭）")
	}
	if _, err := s.VerifyToken(adminTok.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("用户已删除，旧 token 应 ErrUnauthorized: %v", err)
	}
	// 其它用户不受影响
	if !s.ActiveTokenID(netopTok.ID) {
		t.Fatalf("在册用户的会话仍应活动，不得误伤")
	}
	// 空 ID 的保守放行语义不变（旧式登记无从核对）
	if !s.ActiveTokenID("") {
		t.Fatalf("空 ID 应保守放行为真")
	}
}

// TestListTokensHidesDeletedUser 活动会话清单按当前 committed 过滤：
// 已不在册的用户不再被列成「活动会话」（零主动吊销也成立）。
func TestListTokensHidesDeletedUser(t *testing.T) {
	src := newMutableSource(testConfig())
	now, _ := fakeClock()
	s := NewService(src, now)

	adminTok, _ := s.Login("admin", "s3cret-Passw0rd!")
	netopTok, _ := s.Login("netop", "s3cret-Passw0rd!")

	// 删掉 admin，仅留 netop
	withoutAdmin := testConfig()
	withoutAdmin.System.Login.Users = withoutAdmin.System.Login.Users[1:]
	src.set(withoutAdmin)

	list := s.ListTokens("admin", ClassSuperUser) // 以 super-user 视角列全部
	if len(list) != 1 || list[0].User != "netop" {
		t.Fatalf("清单应只剩在册用户的会话 netop，实得: %+v", list)
	}
	for _, v := range list {
		if v.ID == adminTok.ID {
			t.Fatalf("已删除用户的会话不该出现在清单: %+v", v)
		}
	}
	_ = netopTok
}
