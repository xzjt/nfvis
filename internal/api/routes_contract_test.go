package api

// W9：契约↔路由一致性测试（M2 收尾任务清单 W9）。
// 断言 server.go 注册的每条路由都在 docs/NFViS-openapi.yaml 中声明，
// 防止实现与契约漂移（AGENTS.md 契约先行规则的机械守护）。

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

type apiRoute struct {
	Method string
	Path   string // 去 /api/v1 前缀后的契约路径
}

// registeredRoutes 解析 server.go 源码中的 mux.Handle("METHOD "+APIPrefix+"/...") 注册。
func registeredRoutes(t *testing.T) []apiRoute {
	t.Helper()
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("读取 server.go: %v", err)
	}
	re := regexp.MustCompile(`mux\.Handle\("([A-Z]+) "\+APIPrefix\+"([^"]+)"`)
	out := []apiRoute{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		out = append(out, apiRoute{Method: m[1], Path: m[2]})
	}
	if len(out) < 40 {
		t.Fatalf("路由解析过少（%d），解析器可能失效", len(out))
	}
	return out
}

// contractRoutes 解析 openapi yaml 的 paths（method + path）。
func contractRoutes(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile("../../docs/NFViS-openapi.yaml")
	if err != nil {
		t.Fatalf("读取契约: %v", err)
	}
	out := map[string]bool{}
	pathRe := regexp.MustCompile(`^  (/\S+?):\s*$`)
	methodRe := regexp.MustCompile(`^    (get|post|put|delete):\s*$`)
	lines := strings.Split(string(data), "\n")
	curPath := ""
	for _, line := range lines {
		if m := pathRe.FindStringSubmatch(line); m != nil {
			curPath = m[1]
			continue
		}
		if m := methodRe.FindStringSubmatch(line); m != nil && curPath != "" {
			out[strings.ToUpper(m[1])+" "+curPath] = true
		}
	}
	if len(out) < 60 {
		t.Fatalf("契约端点解析过少（%d）", len(out))
	}
	return out
}

// normalizePath 规整实现路由到契约形态：{tail...} → 任意段（含冒号后缀）。
func (r apiRoute) contractKeys() []string {
	p := r.Path
	if strings.Contains(p, "{tail...}") {
		// {tail...} 捕获含冒号后缀的尾段：对应契约中的 {name}:action 形态
		base := strings.TrimSuffix(p, "{tail...}")
		return []string{r.Method + " " + base + "{name}:change-password"}
	}
	return []string{r.Method + " " + p}
}

func TestRoutesRegisteredInContract(t *testing.T) {
	contract := contractRoutes(t)
	missing := []string{}
	for _, r := range registeredRoutes(t) {
		found := false
		for _, key := range r.contractKeys() {
			if contract[key] {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, r.Method+" "+r.Path)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("以下路由未在 docs/NFViS-openapi.yaml 声明（契约漂移，先改契约再写代码，AGENTS.md 规则 1）:\n  %s",
			strings.Join(missing, "\n  "))
	}
}
