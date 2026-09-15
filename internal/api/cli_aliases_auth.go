package api

// 登录与 TLS 类语句别名（决策 #79，缺陷驱动）。
//
// 背景：CLI 语句树与模型 JSON 键**结构不一致**，而 `fromJSONTree` 现在用
// DisallowUnknownFields（同批收紧），因此这类不一致会显式报错而非静默丢弃：
//   - `login user <n> …`      → 模型 `system.login.users[]`（单数/复数）
//   - `login class <n> allow` → 模型 `system.login.classes[].allow`（同上）
//   - `login user … password` → 模型 `password_hash`（**必须哈希后再写**，不得存明文）
//   - `api tls cert-file … key-file …` → 模型 `system.api.{cert_file,key_file}`（CLI 多一层 tls）
//
// 注：单条 `api tls cert-file <p>` 已有 5-token 规则（cli_aliases_system.go）；
// 本条补的是契约里**一条语句同时给证书与私钥**的 7-token 形态。

import (
	"fmt"
	"strings"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
)

var statementAliasesAuth = []aliasRule{
	// system api tls cert-file <p> key-file <p>（契约 §2.2；与 5-token 规则各自独立）
	{pattern: []string{"system", "api", "tls", "cert-file", "*", "key-file", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			api := ensureObj(ensureObj(tree, "system"), "api")
			if !isSet {
				delete(api, "cert_file")
				delete(api, "key_file")
				return nil
			}
			api["cert_file"], api["key_file"] = t[4], t[6]
			return nil
		}},

	// system login user <n> password <pw> [class <c>]
	{pattern: []string{"system", "login", "user", "*", "password", "*", "class", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return loginUserApply(tree, t[3], t[5], t[7], isSet)
		}},
	{pattern: []string{"system", "login", "user", "*", "password", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return loginUserApply(tree, t[3], t[5], "", isSet)
		}},
	// system login user <n> class <c>
	{pattern: []string{"system", "login", "user", "*", "class", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return loginUserApply(tree, t[3], "", t[5], isSet)
		}},
	{pattern: []string{"system", "login", "user", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return loginUserApply(tree, t[3], "", "", isSet)
		}},

	// system login class <n> allow|deny <command-path>
	{pattern: []string{"system", "login", "class", "*", "allow", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return loginClassApply(tree, t[3], "allow", t[5], isSet)
		}},
	{pattern: []string{"system", "login", "class", "*", "deny", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return loginClassApply(tree, t[3], "deny", t[5], isSet)
		}},
	{pattern: []string{"system", "login", "class", "*", "allow"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return loginClassApply(tree, t[3], "allow", "", false)
		}},
	{pattern: []string{"system", "login", "class", "*", "deny"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return loginClassApply(tree, t[3], "deny", "", false)
		}},
	{pattern: []string{"system", "login", "class", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return loginClassApply(tree, t[3], "", "", isSet)
		}},
}

// loginUsersArr 定位（必要时创建）system.login.users 数组。
func loginUsersArr(tree map[string]any, create bool) (map[string]any, []any, error) {
	sys := ensureObj(tree, "system")
	login := ensureObj(sys, "login")
	arr, _ := login["users"].([]any)
	if arr == nil {
		if !create {
			return login, nil, fmt.Errorf("无匹配配置: system login user")
		}
		arr = []any{}
	}
	return login, arr, nil
}

// loginUserApply 维护 system.login.users[]（模型 LoginUserConfig：name / password_hash / class）。
//
// 口令**必须哈希**：模型字段是 `password_hash`，与 REST 建用户一致地
// 先过 `aaa.CheckPasswordPolicy` 再 `aaa.HashPassword`——不可把明文写进配置
// （也正因如此，本条不能只靠"改 JSON 键"解决）。
// 已哈希值（以 pbkdf2$ 开头）视为直接注入，便于 load/克隆场景幂等。
func loginUserApply(tree map[string]any, name, password, class string, isSet bool) error {
	login, arr, err := loginUsersArr(tree, isSet)
	if err != nil {
		return err
	}
	elem, idx := selectElement(arr, "name", name)
	if !isSet {
		if elem == nil {
			return fmt.Errorf("无匹配配置: system login user %s", name)
		}
		if password != "" {
			// 只删口令：保留用户
			if _, ok := elem["password_hash"]; !ok {
				return fmt.Errorf("无匹配配置: system login user %s password", name)
			}
			delete(elem, "password_hash")
			return nil
		}
		if class != "" {
			if _, ok := elem["class"]; !ok {
				return fmt.Errorf("无匹配配置: system login user %s class", name)
			}
			delete(elem, "class")
			return nil
		}
		// 整体删除（同时清理自定义 class 之外的内容）
		login["users"] = append(arr[:idx], arr[idx+1:]...)
		return nil
	}
	if elem == nil {
		elem = map[string]any{"name": name}
		arr = append(arr, elem)
		login["users"] = arr
	}
	if password != "" {
		hash, err := hashPasswordForConfig(tree, password)
		if err != nil {
			return err
		}
		elem["password_hash"] = hash
	}
	if class != "" {
		elem["class"] = class
	}
	return nil
}

// hashPasswordForConfig 生成本地用户口令哈希；与 REST 一致地先套用已配置的口令策略
// （策略在 system.login.password_policy，可能尚未提交到 candidate 之外——取当前树值）。
func hashPasswordForConfig(tree map[string]any, password string) (string, error) {
	if strings.HasPrefix(password, "pbkdf2$") {
		return password, nil // 已是哈希（load/克隆路径）：直接落库，避免二次哈希
	}
	if pol := passwordPolicyOf(tree); pol != nil {
		if bad := aaa.CheckPasswordPolicy(password, pol); len(bad) > 0 {
			return "", fmt.Errorf("口令不满足策略：%s", strings.Join(bad, "；"))
		}
	}
	return aaa.HashPassword(password)
}

// passwordPolicyOf 从配置树取口令策略（不存在则 nil，表示用 aaa 的内建缺省）。
func passwordPolicyOf(tree map[string]any) *model.PasswordPolicy {
	login, _ := ensureObj(tree, "system")["login"].(map[string]any)
	if login == nil {
		return nil
	}
	raw, _ := login["password_policy"].(map[string]any)
	if raw == nil {
		return nil
	}
	pol := &model.PasswordPolicy{}
	if v, ok := raw["min_length"].(float64); ok {
		pol.MinLength = int(v)
	}
	if v, ok := raw["complexity"].(bool); ok {
		pol.Complexity = v
	}
	if v, ok := raw["expire_days"].(float64); ok {
		pol.ExpireDays = int(v)
	}
	return pol
}

// loginClassApply 维护 system.login.classes[]（模型 ClassDef：name / allow[] / deny[]）。
// allow/deny 是**数组**（可多条），故按值追加/删除而非覆盖。
func loginClassApply(tree map[string]any, name, kind, path string, isSet bool) error {
	sys := ensureObj(tree, "system")
	login := ensureObj(sys, "login")
	arr, _ := login["classes"].([]any)
	elem, idx := selectElement(arr, "name", name)

	if !isSet && kind == "" {
		if elem == nil {
			return fmt.Errorf("无匹配配置: system login class %s", name)
		}
		login["classes"] = append(arr[:idx], arr[idx+1:]...)
		return nil
	}
	if elem == nil {
		if !isSet {
			return fmt.Errorf("无匹配配置: system login class %s", name)
		}
		elem = map[string]any{"name": name}
		arr = append(arr, elem)
		login["classes"] = arr
	}
	if kind == "" {
		return nil
	}
	cur, _ := elem[kind].([]any)
	if !isSet {
		// kind 给定但无取值 → 清空该列表
		if path == "" {
			delete(elem, kind)
			return nil
		}
		removed := false
		next := make([]any, 0, len(cur))
		for _, v := range cur {
			if s, _ := v.(string); s == path {
				removed = true
				continue
			}
			next = append(next, v)
		}
		if !removed {
			return fmt.Errorf("无匹配配置: system login class %s %s %s", name, kind, path)
		}
		elem[kind] = next
		return nil
	}
	for _, v := range cur {
		if s, _ := v.(string); s == path {
			return nil // 幂等：已存在
		}
	}
	elem[kind] = append(cur, path)
	return nil
}
