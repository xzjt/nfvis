package api

// FR-SEC-007 / 决策 #25（口令哈希不得回显于任何 show/API 输出）。
//
// 背景：配置视图（candidate/rollback 回显）此前直接序列化 model.Config，
// 其中 `system.login.users[].password_hash` 为 PBKDF2 哈希原文——
// 而 commit 的审计详情是完整 diff，导致只读账号可经 GET /audit-logs 取得他人哈希。
// 本文件负责**配置视图**的脱敏；审计/diff 文本的脱敏在 internal/model 渲染层完成。
//
// 实现已于 2026-09-24（决策 #149）下移到 internal/model：同一个 `password_hash`
// 还要在**诊断归档**（internal/system）里被摘掉，两边必须共用一份实现——
// 唯一规则仍是 model.IsSensitiveKey，本文件不自带任何判据。

import (
	"github.com/xzjt/nfvis/internal/model"
)

// redactConfigView 返回配置视图的脱敏副本（敏感字段整个移除，而非置为占位符）。
func redactConfigView(cfg model.Config) any { return redactView(cfg) }

// redactView 返回**任意配置视图**（整配置或单个配置段）的脱敏副本。
//
// 分段端点（如 GET /system 返回 model.SystemConfig）与整配置端点（GET /configuration）
// 必须同口径——否则同一个 `system.login.users[].password_hash` 从一个端点被摘掉、
// 从另一个端点原样回显（该缺陷 2026-09-23 由真机实测发现，见 redact_test.go 的全路径守护）。
func redactView(v any) any { return model.RedactSensitive(v) }
