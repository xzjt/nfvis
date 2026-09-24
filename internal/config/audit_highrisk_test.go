package config

// 决策 #150：配置事务里高危档变更的「意图」行——引擎侧守护。
//
// 引擎是 CLI 与 REST 的共同落点（`set|delete` + `commit`、端点直提、界面表单都汇到这里），
// 所以「一次高危提交恰好两条记录、非高危提交仍是一条」这条口径在这里判最准。

import (
	"context"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// auditOf 取审计（倒序：最新在前）。
func auditOf(t *testing.T, k *engineKit) []AuditEntry {
	t.Helper()
	rows, err := k.store.ListAudit(50, 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	return rows
}

// commitUser 在候选上按 mutate 改 system.login 后提交。
func commitLogin(t *testing.T, k *engineKit, source string, mutate func(*model.SystemLogin)) error {
	t.Helper()
	k.edit(t, "admin", source)
	cfg, _, err := k.engine.Candidate()
	if err != nil {
		t.Fatalf("Candidate: %v", err)
	}
	if cfg.System == nil {
		cfg.System = &model.SystemConfig{}
	}
	if cfg.System.Login == nil {
		cfg.System.Login = &model.SystemLogin{}
	}
	mutate(cfg.System.Login)
	if err := k.engine.UpdateCandidate(Session{User: "admin", Source: source}, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	_, err = k.engine.Commit(context.Background(), Session{User: "admin", Source: source}, CommitOpts{})
	return err
}

// TestHighRiskCommitTwoRows 高危变更：意图在前、结果在后，恰好两条。
func TestHighRiskCommitTwoRows(t *testing.T) {
	k := newEngineKit(t)
	// 先建一个用户（建用户本身也是高危档）
	if err := commitLogin(t, k, "ssh", func(l *model.SystemLogin) {
		l.Users = append(l.Users, model.LoginUserConfig{Name: "bob", Class: "operator", PasswordHash: "pbkdf2$sha256$1$AAAA$BBBB"})
	}); err != nil {
		t.Fatalf("建用户: %v", err)
	}
	rows := auditOf(t, k)
	if len(rows) != 2 {
		t.Fatalf("建用户应恰好两条，实得 %d: %+v", len(rows), rows)
	}
	if rows[1].Result != AuditResultIntent || rows[0].Result != "success" {
		t.Fatalf("顺序应为 intent（早）→ success（晚），实得 %+v", rows)
	}
	if !strings.Contains(rows[1].Detail, "创建本地用户 bob") {
		t.Errorf("意图行文案: %q", rows[1].Detail)
	}
	if strings.Contains(rows[1].Detail, "pbkdf2$") {
		t.Errorf("口令哈希不得进审计: %q", rows[1].Detail)
	}

	// 删用户 + 改口令：两条，意图说明要做什么
	if err := commitLogin(t, k, "ssh", func(l *model.SystemLogin) {
		l.Users = nil
	}); err != nil {
		t.Fatalf("删用户: %v", err)
	}
	rows = auditOf(t, k)
	if len(rows) != 4 { // 两次提交各两条
		t.Fatalf("应有 4 条，实得 %d: %+v", len(rows), rows)
	}
	if rows[1].Result != AuditResultIntent || !strings.Contains(rows[1].Detail, "删除本地用户 bob") {
		t.Fatalf("删用户的意图行不符: %+v", rows[1])
	}
	if rows[0].Result != "success" {
		t.Fatalf("删用户的结果行不符: %+v", rows[0])
	}
}

// TestHighRiskCommitFailureTwoRows 失败路径也是两条：结果行 failure 且带原因。
func TestHighRiskCommitFailureTwoRows(t *testing.T) {
	k := newEngineKit(t)
	k.applier.fail = true // 底座下发失败（校验已过、动作已开始）
	if err := commitLogin(t, k, "ssh", func(l *model.SystemLogin) {
		l.Users = append(l.Users, model.LoginUserConfig{Name: "bob", Class: "operator", PasswordHash: "pbkdf2$sha256$1$AAAA$BBBB"})
	}); err == nil {
		t.Fatalf("底座失败时 commit 应报错")
	}
	rows := auditOf(t, k)
	if len(rows) != 2 {
		t.Fatalf("失败路径也应两条，实得 %d: %+v", len(rows), rows)
	}
	if rows[1].Result != AuditResultIntent {
		t.Fatalf("第一条应是意图行: %+v", rows[1])
	}
	if rows[0].Result != "failure" || !strings.Contains(rows[0].Detail, "底座错误") {
		t.Fatalf("结果行应是 failure 且带原因: %+v", rows[0])
	}
}

// TestHighRiskCommitNoFalsePositive 语义没变的写法不产生意图行（避免审计噪音）。
func TestHighRiskCommitNoFalsePositive(t *testing.T) {
	k := newEngineKit(t)
	// ① 口令策略 nil ⇄ 全零：产品语义相同（都走内建缺省）→ 不算高危变更
	if err := commitLogin(t, k, "ssh", func(l *model.SystemLogin) {
		l.PasswordPolicy = &model.PasswordPolicy{}
	}); err != nil {
		t.Fatalf("提交空策略: %v", err)
	}
	if rows := auditOf(t, k); len(rows) != 1 || rows[0].Result != "success" {
		t.Fatalf("语义没变的策略提交不应产生意图行: %+v", rows)
	}
	// ② 普通配置变更（主机名）同样只有一条
	k.edit(t, "admin", "ssh")
	cfg, _, _ := k.engine.Candidate()
	cfg.System.Hostname = "renamed"
	if err := k.engine.UpdateCandidate(Session{User: "admin", Source: "ssh"}, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	if _, err := k.engine.Commit(context.Background(), Session{User: "admin", Source: "ssh"}, CommitOpts{}); err != nil {
		t.Fatalf("提交主机名: %v", err)
	}
	rows := auditOf(t, k)
	if len(rows) != 2 || rows[0].Result != "success" || strings.Contains(rows[0].Detail, "本地用户") {
		t.Fatalf("非高危提交应只有一条结果行: %+v", rows)
	}
}

// TestHighRiskIntentDescription 判定函数的边界（各字段各说各的，且不含秘密）。
func TestHighRiskIntentDescription(t *testing.T) {
	base := model.Config{System: &model.SystemConfig{Login: &model.SystemLogin{
		Users:          []model.LoginUserConfig{{Name: "a", Class: "operator", PasswordHash: "h1"}},
		Classes:        []model.ClassDef{{Name: "netop", Allow: []string{"show"}}},
		PasswordPolicy: &model.PasswordPolicy{MinLength: 8},
	}}}
	cases := []struct {
		name   string
		next   model.Config
		source string
		want   []string // 期望出现的片段（空 = 期望无意图）
	}{
		{
			name: "改口令（不含哈希）",
			next: model.Config{System: &model.SystemConfig{Login: &model.SystemLogin{
				Users: []model.LoginUserConfig{{Name: "a", Class: "operator", PasswordHash: "h2"}},
			}}},
			want: []string{"重置本地用户 a 的口令", model.RedactedPlaceholder},
		},
		{
			name: "改权限类",
			next: model.Config{System: &model.SystemConfig{Login: &model.SystemLogin{
				Users: []model.LoginUserConfig{{Name: "a", Class: "super-user", PasswordHash: "h1"}},
			}}},
			want: []string{"调整本地用户 a 的权限类：operator → super-user"},
		},
		{
			name: "改口令策略（逐字段）",
			next: model.Config{System: &model.SystemConfig{Login: &model.SystemLogin{
				Users:          []model.LoginUserConfig{{Name: "a", Class: "operator", PasswordHash: "h1"}},
				Classes:        []model.ClassDef{{Name: "netop", Allow: []string{"show"}}},
				PasswordPolicy: &model.PasswordPolicy{MinLength: 12, Complexity: true},
			}}},
			want: []string{"修改口令策略", "口令最小长度 8 → 12", "口令复杂度要求 关 → 开"},
		},
		{
			name: "改自定义 class 的路径清单",
			next: model.Config{System: &model.SystemConfig{Login: &model.SystemLogin{
				Users:   []model.LoginUserConfig{{Name: "a", Class: "operator", PasswordHash: "h1"}},
				Classes: []model.ClassDef{{Name: "netop", Allow: []string{"show", "request"}}},
			}}},
			want: []string{"调整权限类 netop 的命令路径清单"},
		},
		{
			name: "删自定义 class",
			next: model.Config{System: &model.SystemConfig{Login: &model.SystemLogin{}}},
			want: []string{"删除权限类 netop", "删除本地用户 a"},
		},
		{
			name:   "恢复出厂来源不重复判定",
			next:   model.Config{},
			source: SourceZeroize,
			want:   nil,
		},
		{
			name:   "恢复配置来源不重复判定",
			next:   model.Config{},
			source: SourceRestore,
			want:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := highRiskConfigIntent(base, tc.next, tc.source)
			if len(tc.want) == 0 {
				if got != "" {
					t.Fatalf("不应产生意图行，实得 %q", got)
				}
				return
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("意图行应含「%s」，实得 %q", w, got)
				}
			}
		})
	}

	// 外部证书文件引用（`system api tls cert-file|key-file`）
	withCert := base
	withCert.System = &model.SystemConfig{API: &model.APIConfig{CertFile: "/root/a.crt", KeyFile: "/root/a.key"}}
	got := highRiskConfigIntent(base, withCert, "ssh")
	if !strings.Contains(got, "安装外部证书") || !strings.Contains(got, "/root/a.crt") {
		t.Errorf("证书安装的意图行不符: %q", got)
	}
	if got = highRiskConfigIntent(withCert, base, "ssh"); !strings.Contains(got, "移除外部证书文件引用") {
		t.Errorf("证书移除的意图行不符: %q", got)
	}
}
