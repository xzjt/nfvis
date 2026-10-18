package api

// R39-1（round39 真机）：Web 控制台的 CSP 是 `default-src 'self'`（无 unsafe-inline），
// 因此**任何内联 style 属性都会被浏览器拦掉**——而前端恰恰用 `el('span', { style: … })`
// 给「历史趋势」图例的色块上色，结果色块退化成空白方块：图例只剩文字、看不出哪条折线
// 对应哪个序列（真机实测：`getComputedStyle(swatch).backgroundColor = rgba(0,0,0,0)`）。
//
// 守护两层：
//  ① 自有前端 JS 里**不得**再出现内联 style 属性写法（`style:` 键 / `setAttribute('style')`），
//     只允许操作 `el.style.foo` 属性（DOM 属性赋值不走 CSP 的样式内联路径）；
//  ② 图例色块的类名与 style.css 的配色类必须成对存在（改一处漏另一处即红）。

import (
	"os"
	"strings"
	"testing"
)

// ① 前端自有 JS 不得用内联 style 属性（CSP 会静默拦掉，界面只丢样式、控制台报 violation）。
func TestUIFrontendHasNoInlineStyleAttributes(t *testing.T) {
	for _, name := range uiJSSortedNames(uiJSFiles(t)) {
		src := uiJSFile(t, name)
		for _, bad := range []string{"setAttribute('style'", `setAttribute("style"`, "setAttribute(`style`"} {
			if strings.Contains(src, bad) {
				t.Errorf("%s: 不得用 %s（CSP default-src 'self' 会拦掉内联 style）", name, bad)
			}
		}
		// `style: '…'` 形式（el() 的 attrs 键）：逐行扫，跳过注释行。
		for i, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
				continue
			}
			if strings.Contains(trimmed, "style:") && !strings.Contains(trimmed, ".style.") {
				t.Errorf("%s:%d 内联 style 属性（CSP 会拦掉）：%s", name, i+1, trimmed)
			}
		}
	}
}

// ①b index.html 里同样不得有内联 style 属性（R39-1 同族：容器 exec 的「超时」输入框
// 曾写 style="width:5em"，CSP 拦掉后宽度失效——样式丢失是可见的，但控制台报 violation）。
func TestUIIndexHtmlHasNoInlineStyleAttributes(t *testing.T) {
	b, err := os.ReadFile("ui/index.html")
	if err != nil {
		t.Fatalf("读 ui/index.html: %v", err)
	}
	html := string(b)
	for i, line := range strings.Split(html, "\n") {
		if strings.Contains(line, "style=") && !strings.Contains(line, "<!--") {
			t.Errorf("ui/index.html:%d 内联 style 属性（CSP 会拦掉）：%s", i+1, strings.TrimSpace(line))
		}
	}
}

// ② 图例色块的类名 ↔ 配色类成对（改一处漏另一处即红）。
func TestUIMetricsLegendSwatchClassesPaired(t *testing.T) {
	js := uiJSFile(t, "ui/js/views/system.js")
	cssBytes, err := os.ReadFile("ui/style.css")
	if err != nil {
		t.Fatalf("读 ui/style.css: %v", err)
	}
	css := string(cssBytes)
	// JS 侧的类名清单（MH_SWATCH_CLASSES）。
	start := strings.Index(js, "const MH_SWATCH_CLASSES")
	if start < 0 {
		t.Fatal("system.js 找不到 MH_SWATCH_CLASSES（图例色块类名清单）")
	}
	end := strings.Index(js[start:], "];")
	if end < 0 {
		t.Fatal("MH_SWATCH_CLASSES 定义不完整")
	}
	decl := js[start : start+end]
	for _, cls := range []string{"mh-sw-0", "mh-sw-1", "mh-sw-2", "mh-sw-3", "mh-sw-4", "mh-sw-5"} {
		if !strings.Contains(decl, "'"+cls+"'") {
			t.Errorf("MH_SWATCH_CLASSES 缺 %q", cls)
		}
		if !strings.Contains(css, "."+cls+" {") {
			t.Errorf("style.css 缺配色类 .%s（色块会退化成空白）", cls)
		}
	}
	// 色块本体仍带基础类，否则宽高/圆角丢失。
	if !strings.Contains(js, "'mh-swatch ' + MH_SWATCH_CLASSES") {
		t.Error("图例色块应同时带 mh-swatch 基础类与配色类")
	}
}
