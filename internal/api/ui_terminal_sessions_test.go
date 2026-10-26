package api

// 决策 #507（round49）：Web 控制台的两条「收尾路径」必须把**两类**终端都关掉。
//
// 由来：本轮修服务端「吊销即终止终端」时发现前端也漏了一半——`signOut`（登出）与
// `beforeunload`（关页面/刷新）只关了 VM 串口（`vmConsoleClose`），容器交互式终端
// （`ctShellClose`）没关：登出后服务端凭据已吊销，容器终端那条 WebSocket 却还留在后台。
// 两条路径都是「会话结束」，口径应一致。

import (
	"strings"
	"testing"
)

// uiBodyOfFunc 取 `export function <name>(` 起、到下一个顶格 `}` 为止的函数体文本。
func uiBodyOfFunc(t *testing.T, src, decl string) string {
	t.Helper()
	i := strings.Index(src, decl)
	if i < 0 {
		t.Fatalf("boot.js 里找不到 %q", decl)
	}
	rest := src[i:]
	j := strings.Index(rest, "\n}")
	if j < 0 {
		t.Fatalf("%q 未闭合（找不到顶格右括号）", decl)
	}
	return rest[:j]
}

func TestSignOutClosesBothTerminals(t *testing.T) {
	src := uiJSFile(t, "ui/js/boot.js")

	// ① 登出收尾：串口与容器终端都要关（stopStream/stopPolling 是既有收尾，不属本守护）
	body := uiBodyOfFunc(t, src, "export function signOut(")
	for _, want := range []string{"vmConsoleClose()", "ctShellClose()"} {
		if !strings.Contains(body, want) {
			t.Errorf("signOut 应收尾 %s（登出后台终端不该活着）:\n%s", want, body)
		}
	}

	// ② 页面卸载收尾：同一口径（关页面/刷新时两类终端都断）
	i := strings.Index(src, "addEventListener('beforeunload'")
	if i < 0 {
		t.Fatal("boot.js 里找不到 beforeunload 收尾")
	}
	rest := src[i:]
	j := strings.Index(rest, "\n")
	if j < 0 {
		j = len(rest)
	}
	unload := rest[:j]
	for _, want := range []string{"vmConsoleClose()", "ctShellClose()"} {
		if !strings.Contains(unload, want) {
			t.Errorf("beforeunload 应收尾 %s:\n%s", want, unload)
		}
	}
}
