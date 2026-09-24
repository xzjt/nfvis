package model

// FR-SEC-007 / 决策 #25、#149：敏感字段不得出现在任何回显/导出/归档里。
//
// 本文件是**脱敏的实现**（规则的唯一落点是 IsSensitiveKey，见 diff.go）。
// 原先实现只存在于 internal/api（配置视图回显），2026-09-24 下移到 model：
// 诊断归档（internal/system）也要脱敏同一条 `password_hash`，而它是 api 的**下游**
// （api 已 import system，反向 import 会成环）——两条路径必须共用同一份实现，
// 否则「同一份配置在一个出口被摘掉、在另一个出口原样交出」会重演（决策 #25 的成因）。

import "encoding/json"

// RedactSensitive 返回任意配置视图的**脱敏副本**（敏感字段整个移除，而非置为占位符）。
//
// 移除而非打码：客户端若把占位符回写，会写成非法哈希导致该账号无法登录；
// 字段缺失至少是可察觉的。口令的新增/修改请走专用端点
// （PUT /system/login-users/{name}、request system password change）。
func RedactSensitive(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v // 配置结构恒可序列化；兜底返回原值
	}
	var tree any
	if err := json.Unmarshal(b, &tree); err != nil {
		return v
	}
	RedactTree(tree)
	return tree
}

// RedactTree 就地递归移除敏感键（键名判定与 diff 渲染共用 IsSensitiveKey）。
func RedactTree(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if IsSensitiveKey(k) {
				delete(t, k)
				continue
			}
			RedactTree(val)
		}
	case []any:
		for _, e := range t {
			RedactTree(e)
		}
	}
}
