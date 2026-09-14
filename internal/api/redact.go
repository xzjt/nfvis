package api

// FR-SEC-007 / 决策 #25（口令哈希不得回显于任何 show/API 输出）。
//
// 背景：配置视图（candidate/rollback 回显）此前直接序列化 model.Config，
// 其中 `system.login.users[].password_hash` 为 PBKDF2 哈希原文——
// 而 commit 的审计详情是完整 diff，导致只读账号可经 GET /audit-logs 取得他人哈希。
// 本文件负责**配置视图**的脱敏；审计/diff 文本的脱敏在 internal/model 渲染层完成。

import (
	"encoding/json"

	"github.com/xzjt/nfvis/internal/model"
)

// redactConfigView 返回配置视图的脱敏副本（敏感字段整个移除，而非置为占位符）。
//
// 移除而非打码：客户端若把占位符回写，会写成非法哈希导致该账号无法登录；
// 字段缺失至少是可察觉的。口令的新增/修改请走专用端点
// （PUT /system/login-users/{name}、request system password change）。
func redactConfigView(cfg model.Config) any {
	b, err := json.Marshal(cfg)
	if err != nil {
		return cfg // 结构恒可序列化；兜底返回原值
	}
	var tree any
	if err := json.Unmarshal(b, &tree); err != nil {
		return cfg
	}
	redactTree(tree)
	return tree
}

// redactTree 递归移除敏感键（键名判定与 diff 渲染共用 model.IsSensitiveKey）。
func redactTree(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if model.IsSensitiveKey(k) {
				delete(t, k)
				continue
			}
			redactTree(val)
		}
	case []any:
		for _, e := range t {
			redactTree(e)
		}
	}
}
