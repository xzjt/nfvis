package api

// 控制台路由表的守护（IA 骨架刀）。
//
// 背景：控制台从"单页 16 张卡"改成"资源域一级 + 对象详情二级"后，**路由表成为取数范围的唯一真源**
// （每条路由声明自己要拉的端点）。故把它做成**数据文件** `ui/routes.json`：前端 JSON.parse、
// 守护用 encoding/json 真解析（决策 #116 的教训——结构化数据别用正则），两边共用同一份。
//
// 本文件只查"形状与一致性"：路径前缀/唯一性、必填字段、**每条路由必须声明至少一个端点**、
// view 必须在 app.js 的 VIEWS 注册表里存在（否则运行期渲染不出来）。
// "端点必须在契约里"由 ui_coverage_test.go 一起查（那里有契约路径集合）。

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type uiRoute struct {
	Path       string   `json:"path"`
	Title      string   `json:"title"`
	View       string   `json:"view"`
	Breadcrumb []string `json:"breadcrumb"`
	Endpoints  []string `json:"endpoints"`
	Poll       int      `json:"poll"`
}

func loadUIRoutes(t *testing.T) []uiRoute {
	t.Helper()
	b, err := os.ReadFile("ui/routes.json")
	if err != nil {
		t.Fatalf("读取 ui/routes.json: %v", err)
	}
	var doc struct {
		Routes []uiRoute `json:"routes"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("ui/routes.json 不是合法 JSON（守护要求真解析器，不用正则）: %v", err)
	}
	if len(doc.Routes) == 0 {
		t.Fatal("ui/routes.json 里没有任何路由")
	}
	return doc.Routes
}

func TestUIRoutesAreConsistent(t *testing.T) {
	routes := loadUIRoutes(t)
	seen := map[string]bool{}
	for _, r := range routes {
		if !strings.HasPrefix(r.Path, "#/") {
			t.Errorf("路由 %q 必须以 #/ 开头（hash 路由，服务端不新增路径）", r.Path)
		}
		if seen[r.Path] {
			t.Errorf("路由 %q 重复", r.Path)
		}
		seen[r.Path] = true
		if r.Title == "" || r.View == "" {
			t.Errorf("路由 %q 缺少 title 或 view", r.Path)
		}
		if len(r.Breadcrumb) == 0 {
			t.Errorf("路由 %q 缺少面包屑（导航要靠它分组）", r.Path)
		}
		if len(r.Endpoints) == 0 {
			t.Errorf("路由 %q 没有声明任何端点——每页必须自报取数范围（动作类端点不算页面取数，写进该页的交互里）", r.Path)
		}
		if r.Poll < 0 {
			t.Errorf("路由 %q 的 poll 不能为负", r.Path)
		}
	}
	if !seen["#/"] {
		t.Error("缺少总览路由 #/（未知路由与登录后都要回落到它）")
	}

	app, err := os.ReadFile("ui/app.js")
	if err != nil {
		t.Fatalf("读取 ui/app.js: %v", err)
	}
	for _, r := range routes {
		if !strings.Contains(string(app), "'"+r.View+"':") {
			t.Errorf("路由 %q 的 view %q 在 app.js 的 VIEWS 注册表里找不到（键名要写成带引号的形式）", r.Path, r.View)
		}
	}
}
