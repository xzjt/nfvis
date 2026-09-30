package api

// 决策 #303：登录横幅两个展示面的**渲染守护**（源码形状）。
//
// 服务端行为由 login_banner_test.go 覆盖（未认证可达、只回 banner、未设置省略、512 上限）；
// 这里钉的是"界面真的把它显示出来、写入口真的通了"：
//  ① 登录页：横幅块在登录表单**之前**（设计口径：表单上方），默认 `hidden`（无横幅不占位）；
//  ② 用户与权限页：横幅卡有「当前值 + 单行输入 + 保存 + 清除」四件，保存/清除都接到了处理器；
//  ③ app.js：登录视图出现时真的去取（showLogin → loadLoginBanner），写卡读的是**同一个**未认证端点
//     （卡片显示的就是登录者会看到的）；横幅文本用 textContent 落值（外部文本不进 innerHTML）。
//
// 判据是"源码里有没有这条线"，与 TestUIConsoleRoleGating 同风格；**真行为由浏览器/pty 验收**
// （决策 #141：界面交付要在浏览器里真的操作一遍，本用例替代不了它）。

import (
	"os"
	"strings"
	"testing"
)

func TestUILoginBannerRenderWiring(t *testing.T) {
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

	// ① 登录页横幅块：必须在登录表单之前（表单上方），否则改文案顺序会静默把它挪到表单下。
	bi := strings.Index(html, `id="login-banner"`)
	fi := strings.Index(html, `id="login-form"`)
	if bi < 0 {
		t.Fatal(`index.html 缺少 id="login-banner"（登录页横幅块）`)
	}
	if fi < 0 {
		t.Fatal(`index.html 缺少 id="login-form"（登录表单，守护锚点变了本用例要跟着改）`)
	}
	if bi > fi {
		t.Error("登录横幅块应在登录表单之前（设计口径：横幅在表单上方）")
	}

	// ② 用户与权限页的横幅卡：当前值 + 单行输入 + 保存 + 清除。
	for _, id := range []string{
		`id="usr-banner-current"`, `id="usr-banner-text"`,
		`id="usr-banner-save-btn"`, `id="usr-banner-clear-btn"`,
	} {
		if !strings.Contains(html, id) {
			t.Errorf("index.html 缺少登录横幅卡的 %s", id)
		}
	}

	// ③ app.js 的线：登录页取横幅、卡片渲染、保存与清除接线、文本安全落值。
	for _, want := range []string{
		"function loadLoginBanner(",
		"function renderBannerCard(",
		"function usrBannerSave(",
		"function usrBannerClear(",
		"renderBannerCard(d['/login-banner'])",
		"$('usr-banner-save-btn').addEventListener('click', usrBannerSave)",
		"$('usr-banner-clear-btn').addEventListener('click', usrBannerClear)",
		"box.textContent = body.banner;",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 缺少登录横幅的接线：%q", want)
		}
	}

	// ④ 登录视图出现时必须真的去取横幅（否则登录页那块永远空白，且设置/清除后要刷新浏览器才变）。
	si := strings.Index(js, "function showLogin(")
	if si < 0 {
		t.Fatal("app.js 缺少 showLogin（登录视图入口，守护锚点变了本用例要跟着改）")
	}
	body := js[si:]
	if end := strings.Index(body, "\n}"); end >= 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "loadLoginBanner()") {
		t.Error("showLogin 里没有调用 loadLoginBanner（登录页不会显示横幅）")
	}
}
