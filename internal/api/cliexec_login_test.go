package api

import (
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
)

// 决策 #82：实例名位置的解析优先级、别名层的子关键字守卫、错误消息口径。
//
// 由来：真机反馈 `set system login user password Admin@123` 只得到
// `%% 语句未映射到模型（键名不匹配或类型不符）: json: unknown field "password"`。

// ① 实例名位置上的 token 必须先按**实例名**消费，不得先按子关键字解释。
// 修复前解析器先做子关键字匹配，于是与子关键字同名的实例名会被短路，且因为
// cur 仍停在祖先容器上，取值被写到祖先层级——产出模型无法接受的树。
func TestInstanceNamePositionConsumesNameNotKeyword(t *testing.T) {
	// 与子关键字同名的实例名（virtual-switches 的合法名字里就有 ports 这个子关键字）
	tree := toJSONTree(model.Config{})
	if err := applyTokens(cfgPathRoot(), tree, []string{"virtual-switches", "ports"}, true); err != nil {
		t.Fatalf("applyTokens 不应报错: %v", err)
	}
	arr, _ := tree["virtual_switches"].([]any)
	if len(arr) != 1 {
		t.Fatalf("应建出 1 个交换机元素，实际: %v", tree)
	}
	el, _ := arr[0].(map[string]any)
	if el == nil || el["name"] != "ports" {
		t.Fatalf("ports 应作为实例名被消费，实际: %v", tree)
	}

	// 取值不得被写到祖先容器（修复前是 {"system":{"login":{"password":"Admin@123","user":[]}}}）
	tree2 := toJSONTree(model.Config{})
	if err := applyTokens(cfgPathRoot(), tree2, []string{"system", "login", "user", "password", "Admin@123"}, true); err == nil {
		t.Fatalf("多余 token 应报错，实际: %v", tree2)
	}
	sys, _ := tree2["system"].(map[string]any)
	login, _ := sys["login"].(map[string]any)
	if _, bad := login["password"]; bad {
		t.Fatalf("取值不得写进 login 层级（修复前的错位树）: %v", tree2)
	}
	users, _ := login["user"].([]any)
	if len(users) != 1 {
		t.Fatalf("用户名应被消费进数组元素: %v", tree2)
	}
	if u, _ := users[0].(map[string]any); u["name"] != "password" {
		t.Fatalf("实例名应为 password: %v", tree2)
	}
}

// ②③ 子关键字落在实例名位置：显式拒绝，且**不得静默建出无口令账号**。
func TestLoginUserRejectsKeywordAsName(t *testing.T) {
	x, engine := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", "configure")

	for _, line := range []string{
		"set system login user password",           // 4 token：子关键字即名字位
		"set system login user class operator",     // 5 token：子关键字 + 多余取值
		"set system login user password Admin@123", // 5 token：本次真机反馈的写法
	} {
		res := x.Execute("admin", aaa.ClassSuperUser, "ssh", line)
		if !strings.Contains(res.Output, "%%") {
			t.Fatalf("%q 应被拒绝，实际: %q", line, res.Output)
		}
		if strings.Contains(res.Output, "[ok]") {
			t.Fatalf("%q 不得返回成功（会静默建号）: %q", line, res.Output)
		}
		if strings.Contains(res.Output, "json:") {
			t.Fatalf("%q 不得泄露 Go json 原文: %q", line, res.Output)
		}
		if !strings.Contains(res.Output, "set system login user <name> password") {
			t.Fatalf("%q 应给出正确写法，实际: %q", line, res.Output)
		}
	}

	// candidate 里不得出现名为 password / class 的用户
	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatalf("读 candidate: %v", err)
	}
	if cfg.System != nil && cfg.System.Login != nil {
		for _, u := range cfg.System.Login.Users {
			if u.Name == "password" || u.Name == "class" {
				t.Fatalf("不得建出以子关键字命名的用户: %+v", cfg.System.Login.Users)
			}
		}
	}
}

// ② 正确写法仍必须可用（修复不得误伤）。
func TestLoginUserCorrectFormsStillWork(t *testing.T) {
	x, engine := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set system login user bob password Bob@12345",
		"set system login user bob class operator",
	)
	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatalf("读 candidate: %v", err)
	}
	var got *model.LoginUserConfig
	for i := range cfg.System.Login.Users {
		if cfg.System.Login.Users[i].Name == "bob" {
			got = &cfg.System.Login.Users[i]
		}
	}
	if got == nil {
		t.Fatalf("应建出用户 bob: %+v", cfg.System.Login.Users)
	}
	if got.Class != "operator" {
		t.Fatalf("class 应落地: %+v", got)
	}
	if !strings.HasPrefix(got.PasswordHash, "pbkdf2$") {
		t.Fatalf("口令应以哈希落库: %q", got.PasswordHash)
	}
}

// ③ 守卫仅限 set：delete 仍须能清理历史上误建的账号。
func TestLoginUserDeleteNotBlockedByGuard(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", "configure")

	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "delete system login user password")
	if strings.Contains(res.Output, "语句不完整") {
		t.Fatalf("delete 不应被子关键字守卫拦截: %q", res.Output)
	}
	if !strings.Contains(res.Output, "无匹配配置") {
		t.Fatalf("应为「无匹配配置」（该用户不存在）: %q", res.Output)
	}
}

// ④ 两处「树 → 模型」解码点也要中文化：`login.user`（CLI 单数）与模型 `login.users`
// 不一致时，原先用户看到的是 `json: unknown field "user"`。
// 这两处（fromJSONTree / validateTreeJSON）是全仓唯一产出该文案的地方。
func TestTreeDecodeMessagesAreChinese(t *testing.T) {
	badTree := map[string]any{"system": map[string]any{"login": map[string]any{"user": []any{}}}}

	var cfg model.Config
	err := fromJSONTree(badTree, &cfg)
	if err == nil {
		t.Fatal("fromJSONTree 应报错（login.user 不是模型字段）")
	}
	if err2 := validateTreeJSON(badTree); err2 == nil {
		t.Fatal("validateTreeJSON 应报错")
	} else {
		err = err2
	}

	got := err.Error()
	if strings.Contains(got, "json:") {
		t.Fatalf("不得泄露 Go json 原文: %q", got)
	}
	if !strings.Contains(got, `"user"`) {
		t.Fatalf("应指出出问题的字段: %q", got)
	}
	if !strings.Contains(got, "配置中不存在字段") {
		t.Fatalf("应为中文口径: %q", got)
	}
	if !strings.Contains(got, "?") {
		t.Fatalf("应给出下一步（? 查看候选）: %q", got)
	}
}

// ④ 错误消息口径（NFR-005）：不得把 Go 的 json 报错原样抛给用户。
func TestDescribeTreeErrHidesRawJSON(t *testing.T) {
	got := describeTreeErr(errors.New(`json: unknown field "password"`)).Error()
	if strings.Contains(got, "json:") {
		t.Fatalf("不得原样透出 Go json 报错: %q", got)
	}
	if !strings.Contains(got, `"password"`) {
		t.Fatalf("应指出出问题的字段: %q", got)
	}
	if !strings.Contains(got, "?") {
		t.Fatalf("应给出下一步（? 查看候选）: %q", got)
	}

	// 其它解码错误：给中文指引，同时保留技术细节（开发者仍需）
	got2 := describeTreeErr(errors.New("cannot unmarshal string into Go struct field .x of type int")).Error()
	if !strings.Contains(got2, "配置模型") || !strings.Contains(got2, "cannot unmarshal") {
		t.Fatalf("类型错误应给中文指引并保留细节: %q", got2)
	}
}

// ③ 守卫的「保留名」取自 schema 树而非硬编码：子关键字增删时守卫随之变化。
func TestReservedChildKeywordComesFromSchema(t *testing.T) {
	path := []string{"system", "login", "user"}
	for _, kw := range []string{"password", "class"} {
		if got := reservedChildKeyword(path, kw); got != kw {
			t.Fatalf("%q 是子关键字，应被识别（实际 %q）", kw, got)
		}
	}
	if got := reservedChildKeyword(path, "bob"); got != "" {
		t.Fatalf("普通用户名不应被判为子关键字: %q", got)
	}
}
