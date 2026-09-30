package api

// 决策 #304：Web「用户与权限」页「权限类（class）」表「生效权限」按钮的**渲染守护**（源码形状）。
//
// 服务端行为由 permissions_test.go 覆盖；这里钉界面真的把它显示出来、按钮真的通了：
//  ① index.html：权限类表有「生效权限」列 + 摘要容器；
//  ② app.js：渲染按钮、点击调 GET /configuration/permissions、非 super 只看得到自己所属 class；
//  ③ currentClass 从登录响应设置、退出登录清空。
//
// 判据是「源码里有没有这条线」，与 TestUIConsoleRoleGating / 登录横幅守护同风格；
// **真行为由浏览器验收**（决策 #141：界面交付要在浏览器里真的操作一遍，本用例替代不了它）。

import (
	"os"
	"strings"
	"testing"
)

func TestUIPermissionsViewRenderWiring(t *testing.T) {
	htmlB, err := os.ReadFile("ui/index.html")
	if err != nil {
		t.Fatalf("读取 ui/index.html: %v", err)
	}
	html := string(htmlB)
	jsB, err := os.ReadFile("ui/app.js")
	if err != nil {
		t.Fatalf("读取 ui/app.js: %v", err)
	}
	js := string(jsB)

	// ① 表头多一列「生效权限」+ 摘要容器
	if !strings.Contains(html, "<th>生效权限</th>") {
		t.Error(`index.html 权限类表缺少「生效权限」列`)
	}
	if !strings.Contains(html, `id="usr-perm-out"`) {
		t.Error(`index.html 缺摘要容器 id="usr-perm-out"`)
	}

	// ② app.js 的接线
	for _, want := range []string{
		"function renderPermClassTable(",
		"function usrPermLoad(",
		"'/configuration/permissions?class='",
		"renderPermClassTable(classes)",
		"currentClass === 'super-user' || currentClass === c.name",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 缺少生效权限的接线：%q", want)
		}
	}

	// ③ currentClass 从登录响应设置、退出登录清空
	if !strings.Contains(js, "currentClass = (user && user.class) || ''") {
		t.Error("app.js 未从登录响应设置 currentClass")
	}
	if !strings.Contains(js, "currentClass = ''") {
		t.Error("app.js 未在退出登录时清空 currentClass")
	}
}
