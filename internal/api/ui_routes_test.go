package api

// 控制台路由表的守护（IA 骨架刀 + 详情刀）。
//
// 背景：控制台从"单页 16 张卡"改成"资源域一级 + 对象详情二级"后，**路由表成为取数范围的唯一真源**
// （每条路由声明自己要拉的端点）。故把它做成**数据文件** `ui/routes.json`：前端 JSON.parse、
// 守护用 encoding/json 真解析（决策 #116 的教训——结构化数据别用正则），两边共用同一份。
//
// 本文件只查"形状与一致性"：路径前缀/唯一性、必填字段、**每条路由必须声明至少一个端点**、
// view 必须在 app.js 的 VIEWS 注册表里存在（否则运行期渲染不出来）、
// **参数化路由**（`#/compute/vms/:name`）的参数段形状与 detail 标记的对应关系。
// "端点必须在契约里（且必须是 GET）"由 ui_coverage_test.go 一起查（那里有契约路径与方法集合）。

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

type uiRoute struct {
	Path       string   `json:"path"`
	Title      string   `json:"title"`
	View       string   `json:"view"`
	Detail     bool     `json:"detail"`
	Breadcrumb []string `json:"breadcrumb"`
	Endpoints  []string `json:"endpoints"`
	Poll       int      `json:"poll"`
}

// uiParamSegRe 参数段：`:` 开头 + 标识符（前端按段匹配时用 `seg[0] === ':'`，名字要能当键用）。
var uiParamSegRe = regexp.MustCompile(`^:[A-Za-z_][A-Za-z0-9_]*$`)

// uiEndpointParamRe 端点里的 `{name}` 占位（契约写法）。
var uiEndpointParamRe = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)

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

	// 参数化路由（`#/compute/vms/:name`）的形状：
	//  ① 参数段必须以 `:` 开头且是合法标识符（`:` 后跟标识符，名字要能当 params 的键）；
	//  ② 同一条路由里同名参数只能出现一次（前端按段解出的 params 会被后者覆盖）；
	//  ③ **参数段与 detail 标记必须成对出现**——detail 是"对象详情页"的标记（不进主导航、
	//     导航高亮回落到它的列表页），带参数却不当详情页（或反过来）都会让前端行为说不清；
	//  ④ 端点里的 `{x}` 占位必须都能由该路由的参数段解出来：取数时按段展开占位，
	//     对不上就会把 `{x}` 原样发给服务端（页面只会静默空掉）。
	for _, r := range routes {
		params := map[string]bool{}
		for _, seg := range strings.Split(r.Path, "/") {
			if !strings.HasPrefix(seg, ":") {
				continue
			}
			if !uiParamSegRe.MatchString(seg) {
				t.Errorf("路由 %q 的参数段 %q 不合法（应为 :标识符，名字要能当参数键用）", r.Path, seg)
				continue
			}
			name := seg[1:]
			if params[name] {
				t.Errorf("路由 %q 的参数段 :%s 重复（同名参数只能出现一次）", r.Path, name)
			}
			params[name] = true
		}
		if (len(params) > 0) != r.Detail {
			t.Errorf("路由 %q：参数段 %d 个、detail=%v——对象详情页两者必须一致（前端据此决定进不进导航、怎么回列表页）",
				r.Path, len(params), r.Detail)
		}
		for _, p := range r.Endpoints {
			for _, m := range uiEndpointParamRe.FindAllStringSubmatch(p, -1) {
				if !params[m[1]] {
					t.Errorf("路由 %q 的端点 %s 用了占位 {%s}，但路径里没有对应的 :%s 参数段（取数时无法展开）",
						r.Path, p, m[1], m[1])
				}
			}
		}
	}

	// 详情页必须能回退到一条**已存在的列表路由**（前端沿路径逐段回退取第一个祖先路由：
	// 导航据此高亮、面包屑据此把末段做成回列表页的链接）。回退到的祖先不能是另一条详情页
	// （详情页的"上一级"是列表页），且**面包屑要与该列表页一致**——面包屑末段就是那个链接的
	// 文字，两者不一致会写成"标着 A 的链接跳到 B"。
	byPath := map[string]uiRoute{}
	for _, r := range routes {
		byPath[r.Path] = r
	}
	for _, r := range routes {
		if !r.Detail {
			continue
		}
		var list uiRoute
		found := false
		segs := strings.Split(r.Path, "/")
		for len(segs) > 1 {
			segs = segs[:len(segs)-1]
			if p, ok := byPath[strings.Join(segs, "/")]; ok && !p.Detail {
				list, found = p, true
				break
			}
		}
		if !found {
			t.Errorf("详情路由 %q 回退不到任何列表路由（前端无法高亮导航、面包屑也没有可点的回列表页链接）", r.Path)
			continue
		}
		if strings.Join(list.Breadcrumb, " › ") != strings.Join(r.Breadcrumb, " › ") {
			t.Errorf("详情路由 %q 的面包屑 %v 与它的列表页 %q 的 %v 不一致（末段是回列表页的链接文字，两者必须一致）",
				r.Path, r.Breadcrumb, list.Path, list.Breadcrumb)
		}
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
