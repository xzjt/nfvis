package api

// round49（镜像名越界收口）：Web 控制台镜像名输入框的**前端校验**必须与服务端同源。
//
// 前端先挡一次的意义：非法名不该被发出去（服务端 400 要到网络往返之后才知道，且
// 「a/b.qcow2」这类名字在VM 镜像上是**永远**不可能成立的）。但前端校验最怕的是
// 「自己发明一套规则」——放宽了等于没挡，收紧了会把合法名（容器 Docker 引用 `library/alpine:3.20`）
// 挡在门外。故这里用 images.js 里的**自校准用例表**（`// @imgname ok|bad <type> <name>`）
// 逐条喂给服务端同名校验 images.ValidateImageName，要求两边结论一致：
// 前端悄悄改规则而服务端没跟上（或反之）即红。

import (
	"regexp"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/images"
)

func TestUIImageNameValidationMatchesServer(t *testing.T) {
	src := uiJSFile(t, "ui/js/views/images.js")

	// ① 校验函数与接线都在（否则「前端校验」只是注释里的承诺）
	if !strings.Contains(src, "function imgNameError(") {
		t.Fatal("images.js 缺 imgNameError：镜像名没有前端校验")
	}
	if !strings.Contains(src, "const bad = imgNameError(name, type);") {
		t.Fatal("imgCommon 未调用 imgNameError（校验写了却没接线）")
	}
	// ② 三种导入方式都走 imgCommon（校验是共同入口，别只挡其中一条路）
	for _, fn := range []string{"imgImportURL", "imgImportIncoming", "imgImportFile"} {
		body := imgJSFuncBody(src, fn)
		if !strings.Contains(body, "imgCommon()") {
			t.Errorf("%s 未经 imgCommon（该路径不受前端校验保护）", fn)
		}
		if !strings.Contains(body, "if (!c) return;") {
			t.Errorf("%s 在 imgCommon 判空后应立即返回（非法名不得发请求）", fn)
		}
	}

	// ③ 自校准表逐条与服务端对照
	rows := parseImgNameCases(t, src)
	if len(rows) < 8 {
		t.Fatalf("自校准用例表只有 %d 条（太少，等于没验）：前端判据可能已漂移", len(rows))
	}
	for _, r := range rows {
		r := r
		label := "bad"
		if r.ok {
			label = "ok"
		}
		t.Run(label+"/"+r.typ+"/"+r.name, func(t *testing.T) {
			err := images.ValidateImageName(r.name, r.typ)
			switch {
			case r.ok && err != nil:
				t.Fatalf("前端声明该名合法，服务端却拒绝: %v", err)
			case !r.ok && err == nil:
				t.Fatalf("前端声明该名非法，服务端却接受（前端放宽了，非法名可以发出去）")
			}
		})
	}
}

// imgNameCase 自校准表的一行。
type imgNameCase struct {
	ok   bool   // 前端认为合法
	typ  string // vm-image | container-image
	name string
}

// @imgname 表：`// @imgname ok|bad <type> <name>`（名字允许含空格之外的一切字符，按行尾取）。
var imgNameCaseRe = regexp.MustCompile(`(?m)^// @imgname (ok|bad) (\S+) (.+)$`)

func parseImgNameCases(t *testing.T, src string) []imgNameCase {
	t.Helper()
	var out []imgNameCase
	for _, m := range imgNameCaseRe.FindAllStringSubmatch(src, -1) {
		typ := m[2]
		if typ != images.TypeVM && typ != images.TypeContainer {
			t.Fatalf("@imgname 的 type 只能是 %s/%s，实得 %q", images.TypeVM, images.TypeContainer, typ)
		}
		out = append(out, imgNameCase{ok: m[1] == "ok", typ: typ, name: strings.TrimRight(m[3], " \t\r")})
	}
	return out
}

// imgJSFuncBody 截取 `function <name>(…) {` 起、到下一个顶层 `}` 为止的正文。
// 仅用于本文件的结构断言（JS 无需语法解析）。
func imgJSFuncBody(src, name string) string {
	head := "function " + name + "("
	i := strings.Index(src, head)
	if i < 0 {
		return ""
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n}"); j >= 0 {
		return rest[:j]
	}
	return rest
}
