package api

// 决策 #324：控制台 operator 粒度的守护（补决策 #145 只做"只读 vs 有写"两档的缺口）。
//
// 口径（本决策）：**非 super-user**（operator / 自定义 class）看不到声明为 super-user 的
// 写入口——避免"列了但一用就 403"。判据取服务端权威的登录 class（app.js applyRole 切
// <body> 的 role-nonsuper），由 CSS 隐藏未带 data-op 的 [data-write]；
// 少数 O 级入口（连通性测试、core dump 清单导出）显式带 data-op 由 :not() 放行。
//
// 本守护查**形状与存在性**（真行为由浏览器验收负责）：
//   ① CSS 的 role-nonsuper 规则与 app.js 的 applyRole 切换真的存在；
//   ② index.html 的 data-op 集合与"期望集合"逐条一致（多标/漏标都要报错）；
//   ③ #account-btn（只读/operator 都要能自助改密）不带 data-write、不带 data-op。

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// uiDataOpTagRe 取带 data-op 属性的整个标签（用于收集被放行给非 super-user 的入口）。
var uiDataOpTagRe = regexp.MustCompile(`<[^>]*\bdata-op\b[^>]*>`)

// uiIDInTagRe 从标签里取 id 属性值。
var uiIDInTagRe = regexp.MustCompile(`\bid="([^"]+)"`)

func readUITestAsset(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("ui/" + name)
	if err != nil {
		t.Fatalf("读取 ui/%s: %v", name, err)
	}
	return string(b)
}

func TestUIConsoleOperatorGating(t *testing.T) {
	css := readUITestAsset(t, "style.css")
	app := readUITestAsset(t, "app.js")
	html := readUITestAsset(t, "index.html")

	// ① 机制三件套：CSS 规则、applyRole 切类、role-nonsuper 字样。
	if !strings.Contains(css, "body.role-nonsuper [data-write]:not([data-op])") {
		t.Error("style.css 缺少 body.role-nonsuper [data-write]:not([data-op]) 隐藏规则（operator 会看到 super-user 入口）")
	}
	if !strings.Contains(app, "role-nonsuper") || !strings.Contains(app, "function applyRole(") {
		t.Error("app.js 缺少 applyRole 里对 role-nonsuper 的切换（operator 不会按 class 收敛入口）")
	}

	// ② data-op 期望集合：只有这几个 O 级入口可被非 super-user 看见。
	want := map[string]string{
		"diag-host":      "ping 目标（连通性测试，REST 端点 O 级）",
		"diag-count":     "ping 次数（O 级）",
		"diag-source":    "ping 源地址（O 级）",
		"diag-vrf":       "ping VRF（连通性测试，O 级，决策 #402）",
		"diag-ipv6":      "IPv6 选择（连通性测试，O 级）",
		"diag-ping-btn":  "执行 ping（O 级）",
		"diag-trace-btn": "执行 traceroute（O 级）",
		"ops-export-url": "core dump 清单导出地址（O 级）",
		"ops-export-btn": "导出 core dump 清单（O 级）",
	}
	got := uiDataOpIDs(t, html)
	for id := range got {
		if _, ok := want[id]; !ok {
			t.Errorf("index.html 里 #%s 被标了 data-op，但不在期望集合里——若确实该放行给 operator，请连同理由更新本用例", id)
		}
	}
	for id, why := range want {
		if !got[id] {
			t.Errorf("index.html 里 #%s 缺少 data-op（%s）——operator 会看不到这个本可用的入口", id, why)
		}
	}

	// ③ 「我的账号」：只读与 operator 都要能自助改密，故既不带 data-write 也不带 data-op。
	btn := uiAccountBtnTagRe.FindString(html)
	if btn == "" {
		t.Error("index.html 缺少 #account-btn（顶栏「我的账号」入口，决策 #147）")
	} else {
		if strings.Contains(btn, "data-write") {
			t.Errorf("顶栏「我的账号」入口被打上 data-write：%s", btn)
		}
		if strings.Contains(btn, "data-op") {
			t.Errorf("顶栏「我的账号」入口被打上 data-op：%s", btn)
		}
	}
}

// uiDataOpIDs 收集 index.html 里带 data-op 的元素的 id 集合。
func uiDataOpIDs(t *testing.T, html string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, tag := range uiDataOpTagRe.FindAllString(html, -1) {
		m := uiIDInTagRe.FindStringSubmatch(tag)
		if m == nil {
			t.Errorf("带 data-op 的标签没有 id，无法核对：%s", tag)
			continue
		}
		if out[m[1]] {
			t.Errorf("id=%s 重复带 data-op", m[1])
		}
		out[m[1]] = true
	}
	return out
}
