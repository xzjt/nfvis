package api

// 决策 #304：REST 等价端点 `GET /api/v1/configuration/permissions?class=<name>`。
//
// 与 CLI `show configuration permissions <class>` **同一实现单源**：都调 `buildPermissionView`
// （枚举命令树路径 + internal/aaa 判定）。权限边界同样落在实现处：非 super-user 查他人 class
// 返回 403（不泄露他人规则）；未知 class 返回 404。

import (
	"errors"
	"net/http"

	"github.com/xzjt/nfvis/internal/aaa"
)

// classPermissionsJSON 契约 ClassPermissions（GET /configuration/permissions）。
type classPermissionsJSON struct {
	Class        string   `json:"class"`
	Source       string   `json:"source"`
	AllowedCount int      `json:"allowed_count"`
	DeniedCount  int      `json:"denied_count"`
	AllowedPaths []string `json:"allowed_paths"`
	AllowRules   []string `json:"allow_rules"`
	DenyRules    []string `json:"deny_rules"`
}

// handleGetPermissions GET /api/v1/configuration/permissions：某 class 的生效权限视图。
// `class` 缺省 = 调用者自己的 class。
func (s *Server) handleGetPermissions(w http.ResponseWriter, r *http.Request) {
	ident, _ := Identity(r)
	name := r.URL.Query().Get("class")
	if name == "" {
		name = ident.Class
	}
	// 边界判定在解析之前：非 super-user 只能查自己所属 class（未知/他人一视同仁，
	// 不借错误差异探测他人 class 是否存在）。
	if ident.Class != aaa.ClassSuperUser && name != ident.Class {
		writeError(w, http.StatusForbidden, "FORBIDDEN",
			"仅 super-user 可查看任意 class 的生效权限；其他 class 只能查看自己所属的 class", nil)
		return
	}
	v, err := buildPermissionView(s.aaa, name)
	if err != nil {
		if errors.Is(err, errUnknownClass) {
			writeError(w, http.StatusNotFound, "NOT_FOUND",
				"未知 class: "+name+"（可用：super-user/operator/read-only 或自定义 class 名）", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, classPermissionsJSON{
		Class:        v.Class,
		Source:       string(v.Source),
		AllowedCount: v.Allowed,
		DeniedCount:  v.Denied,
		AllowedPaths: v.allowedPaths(),
		AllowRules:   nonNilStrings(v.RulesAllow),
		DenyRules:    nonNilStrings(v.RulesDeny),
	})
}

// nonNilStrings 保证 JSON 里数组字段是 `[]` 而不是 `null`（契约声明为数组）。
func nonNilStrings(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}
