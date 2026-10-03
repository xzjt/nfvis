package api

// 决策 #357 的副产品守护：**详情页分栏的 HTML 与 JS 常量必须一致**。
//
// 由来（真机实测）：给容器详情页加第三个分栏「执行命令」时，index.html 的按钮与面板都加了，
// 但 app.js 的 `CT_TABS` 常量忘了加 `'exec'`——`ctTabShow` 对不在清单里的 tab **静默回落
// 「概览」**，于是点分栏没反应。所有既有守护（含 UI 覆盖/门禁守护）都看不见：它们只看
// 端点、写控件与角色门禁，不看「点了有没有用」。Browser Use 真机点一遍才抓到（决策 #141
// 的价值再次变现）。此守护把这类「两处清单」的漂移钉死在 CI 里。

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// tabPair 详情页分栏的两处清单：HTML 的 tab 条 id ⇄ JS 的常量名。
var tabPair = []struct {
	barID  string // index.html 里 <div class="tabs" id="…">
	constN string // app.js 里 const <名> = [...]
	page   string // 面板 id 前缀（<前缀>-tab-<tab>）
}{
	{barID: "vmd-tabs", constN: "VM_TABS", page: "vm"},
	{barID: "ctd-tabs", constN: "CT_TABS", page: "ct"},
}

func uiFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("ui", name))
	if err != nil {
		t.Fatalf("读 ui/%s: %v", name, err)
	}
	return string(b)
}

func TestDetailTabsConsistentBetweenHTMLAndJS(t *testing.T) {
	html := uiFile(t, "index.html")
	js := uiFile(t, "app.js")

	for _, p := range tabPair {
		// HTML：取该 tab 条到下一个 </div> 之间的 data-tab 值。
		reBar := regexp.MustCompile(`(?s)id="` + p.barID + `".*?</div>`)
		m := reBar.FindString(html)
		if m == "" {
			t.Fatalf("index.html 里找不到 tab 条 id=%q（分栏被改名/删掉了？本守护需同步）", p.barID)
		}
		var htmlTabs []string
		for _, mt := range regexp.MustCompile(`data-tab="([a-z-]+)"`).FindAllStringSubmatch(m, -1) {
			htmlTabs = append(htmlTabs, mt[1])
		}
		sort.Strings(htmlTabs)

		// JS：常量数组里的取值。
		reConst := regexp.MustCompile(`const ` + p.constN + ` = \[([^\]]*)\]`)
		cm := reConst.FindStringSubmatch(js)
		if cm == nil {
			t.Fatalf("app.js 里找不到 const %s（常量被改名/删掉了？本守护需同步）", p.constN)
		}
		var jsTabs []string
		for _, mt := range regexp.MustCompile(`'([a-z-]+)'`).FindAllStringSubmatch(cm[1], -1) {
			jsTabs = append(jsTabs, mt[1])
		}
		sort.Strings(jsTabs)

		if len(htmlTabs) == 0 || len(jsTabs) == 0 {
			t.Fatalf("%s：解析到空的 tab 清单（HTML %v / JS %v）——解析器可能已失效", p.barID, htmlTabs, jsTabs)
		}
		if strings.Join(htmlTabs, ",") != strings.Join(jsTabs, ",") {
			t.Errorf("%s 的分栏清单两处不一致：\n"+
				"    index.html data-tab = %v\n"+
				"    app.js %s = %v\n"+
				"    漏加常量的后果：点该分栏会**静默回落「概览」**（`ctTabShow`/`vmTabShow` 对不在清单里的 tab 一律回退），\n"+
				"    而面板 id 与按钮都在，静态看不出问题——请把两处补成一致。",
				p.barID, htmlTabs, p.constN, jsTabs)
		}

		// 每个分栏都要有对应面板（`<前缀>-tab-<tab>`），否则切过去是空白。
		for _, tab := range htmlTabs {
			if !strings.Contains(html, `id="`+p.page+`-tab-`+tab+`"`) {
				t.Errorf("%s 的分栏 %q 在 index.html 里没有面板 id=%q（切过去会空白）",
					p.barID, tab, p.page+"-tab-"+tab)
			}
		}
	}
}
