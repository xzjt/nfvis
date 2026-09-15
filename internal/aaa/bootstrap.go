package aaa

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/model"
)

// EnsureBootstrapAdmin 首次启动引导（附录 A #25）：committed 配置尚无本地用户
// 时，以 system 会话经事务引擎创建 admin（super-user），口令以加盐哈希入配置。
//
// password 为空时随机生成；随机口令经 oneTimePassword 返回，调用方负责展示
// 一次（不落日志，FR-SEC-007）。已存在用户时不做任何事（返回 false, "", nil）。
// 引导经正常 commit 流程，入审计。
func EnsureBootstrapAdmin(eng *config.Engine, s *Service, password string) (created bool, oneTimePassword string, err error) {
	has, err := s.HasUsers()
	if err != nil {
		return false, "", err
	}
	if has {
		return false, "", nil
	}
	if password == "" {
		if password, err = randomPassword(); err != nil {
			return false, "", err
		}
		oneTimePassword = password
	}
	if bad := CheckPasswordPolicy(password, nil); len(bad) > 0 {
		return false, "", fmt.Errorf("引导口令不满足策略：%s", strings.Join(bad, "；"))
	}
	hash, err := HashPassword(password)
	if err != nil {
		return false, "", err
	}

	sess := config.Session{User: "system", Source: "console"}
	if err := eng.Edit(sess); err != nil {
		return false, "", err
	}
	cfg, err := eng.Committed()
	if err != nil {
		return false, "", err
	}
	if cfg.System == nil {
		cfg.System = &model.SystemConfig{}
	}
	if cfg.System.Login == nil {
		cfg.System.Login = &model.SystemLogin{}
	}
	cfg.System.Login.Users = append(cfg.System.Login.Users, model.LoginUserConfig{
		Name:         "admin",
		PasswordHash: hash,
		Class:        ClassSuperUser,
	})
	if err := eng.UpdateCandidate(sess, cfg); err != nil {
		return false, "", err
	}
	if _, err := eng.Commit(context.Background(), sess, config.CommitOpts{Message: "首次启动引导 admin 用户"}); err != nil {
		return false, "", err
	}
	_ = eng.Release(sess) // 引导是一次性动作，释放会话锁
	return true, oneTimePassword, nil
}

// randomPassword 生成满足默认策略的随机口令（12 字节 base64url + 保证
// 4 类字符齐全的后缀）。
//
// 后缀用 `@` 而非 `!`：`!` 在交互式 bash 下触发**历史展开**
// （`nfvis-cli -p Xxx!Aa1` → `-bash: !Aa1: event not found`），
// 而首启口令正是要用户从 journalctl 复制粘贴的。`@` 同样满足复杂度策略里
// 的「特殊字符」（unicode.IsPunct），且在 shell 中无特殊含义、无需引号。
func randomPassword() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成随机口令: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b) + "@Aa1", nil
}
