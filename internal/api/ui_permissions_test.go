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
	// 搜索面 = 全部自有 JS（拆分后 users 域在 views/system.js、登录/退出在 js/boot.js）。
	jsFiles := uiJSFiles(t)
	hasSomewhere := func(needle string) bool {
		for _, src := range jsFiles {
			if strings.Contains(src, needle) {
				return true
			}
		}
		return false
	}

	// ① 表头多一列「生效权限」+ 摘要容器
	if !strings.Contains(html, "<th>生效权限</th>") {
		t.Error(`index.html 权限类表缺少「生效权限」列`)
	}
	if !strings.Contains(html, `id="usr-perm-out"`) {
		t.Error(`index.html 缺摘要容器 id="usr-perm-out"`)
	}

	// ② 权限页的接线（views/system.js）
	for _, want := range []string{
		"function renderPermClassTable(",
		"function usrPermLoad(",
		"'/configuration/permissions?class='",
		"renderPermClassTable(classes)",
	} {
		if !hasSomewhere(want) {
			t.Errorf("前端 js 缺少生效权限的接线：%q", want)
		}
	}
	// ③ 权限类判定：getCurrentClass() 访问器（原 currentClass，拆分后经 state.js 访问器读写）。
	if !hasSomewhere("getCurrentClass() === 'super-user' || getCurrentClass() === c.name") {
		t.Error("前端 js 缺少「非 super 只看自己 class」的判定（getCurrentClass 访问器口径）")
	}

	// ④ currentClass 从登录响应设置、退出登录清空（js/boot.js 的 enterApp/signOut）
	if !hasSomewhere("setCurrentClass((user && user.class) || '')") {
		t.Error("前端 js 未从登录响应设置 currentClass（setCurrentClass 口径）")
	}
	if !hasSomewhere("setCurrentClass('')") {
		t.Error("前端 js 未在退出登录时清空 currentClass（setCurrentClass 口径）")
	}
}
