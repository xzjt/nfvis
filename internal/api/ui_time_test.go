package api

// Web 控制台的**记录类时间渲染**守护（round80 真机验收抓到的缺陷）。
//
// 缺陷形态：控制台里记录类时间（审计、告警、事件、提交历史、编辑锁会话、镜像/虚拟机创建、
// 归档、抓包开始、证书有效期）此前用 `toLocaleString()` 渲染——**浏览器本地时区**、且不带
// 任何时区标记；而 CLI（`show log audit`）与 `journalctl` 印的都是 UTC。控制台的设计目标之一
// 是「与 CLI 工单对照」（确认框里就回显等价的命令行语句），差一个时区偏移就对不上账。
//
// 判据（结构性，不跑浏览器）。与前端桩式自校准互补：
// `contrib/scripts/web-console-confirm-selftest.sh` 在 Node 里跑**真实现**（算出 `fmtTime` 的
// 返回值），但机器上没有 node 时它会如实跳过；本文件的断言任何环境都会跑，钉住的是
// 「时间口径不许退回本地时区」这条结构性事实：
//
//	① fmtTime 必须把时间换算成 UTC 再渲染，且带 `UTC` 标记；
//	② 全文件的 toLocale*String 只允许出现在 fmtClockHint()（纯 UI 提示，须写明理由）；
//	③ 本地时区取数（getHours/getFullYear…）不许散落在 fmtTimeOpt 之外；
//	④ 不带时区标记的时间戳按 UTC 字面量处理（交给 Date 会按浏览器时区解释，平白多一个偏移）；
//	⑤ 记录类时间的调用点不少于登记下限（把某处改回内联格式化时至少会被提醒）。

import (
	"os"
	"strings"
	"testing"
)

// uiFuncSpan 定位 app.js 里的顶层函数：返回函数体与起始偏移。
// app.js 的顶层函数都以行首 `}` 收尾，锚点唯一；锚点变了就让本守护报红（而不是静默空转）。
func uiFuncSpan(t *testing.T, src, name string) (body string, start int) {
	t.Helper()
	sig := "function " + name + "("
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatalf("ui/app.js 里找不到 %q：守护的锚点变了，本用例要跟着改", sig)
	}
	j := strings.Index(src[i:], "\n}")
	if j < 0 {
		t.Fatalf("%s 的函数体没有以行首 } 收尾：守护的锚点变了，本用例要跟着改", sig)
	}
	return src[i : i+j], i
}

// uiLineOf：字节偏移 → 行号（报错时指到具体行，方便直接去看那一行）。
func uiLineOf(src string, pos int) int {
	return strings.Count(src[:pos], "\n") + 1
}

// uiMaskComments 把 JS 注释**按原长度替换成空格**（偏移与行号都不变）。
// 判据是「代码怎么渲染时间」，而注释里解释"为什么不再用 toLocaleString"是合法的
// ——本仓库的时间口径注释就写到了它，不剥注释会把解释本身判成违规。
// 只处理 `//`、`/* */` 与三种引号；app.js 的正则字面量里没有引号，故不必做完整词法。
func uiMaskComments(src string) string {
	b := []byte(src)
	out := make([]byte, len(b))
	copy(out, b)
	blank := func(i int) {
		if i < len(b) && b[i] != '\n' {
			out[i] = ' '
		}
	}
	var quote byte
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case quote != 0:
			if c == '\\' {
				i += 2
				continue
			}
			if c == quote {
				quote = 0
			}
			i++
		case c == '\'' || c == '"' || c == '`':
			quote = c
			i++
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			for i < len(b) && b[i] != '\n' {
				blank(i)
				i++
			}
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			for i < len(b) {
				if b[i] == '*' && i+1 < len(b) && b[i+1] == '/' {
					blank(i)
					blank(i + 1)
					i += 2
					break
				}
				blank(i)
				i++
			}
		default:
			i++
		}
	}
	return string(out)
}

// uiFuncLines 返回某顶层函数**函数体**占的行号区间（闭区间），供逐行判定用。
func uiFuncLines(t *testing.T, masked, name string) (int, int) {
	t.Helper()
	body, start := uiFuncSpan(t, masked, name)
	first := uiLineOf(masked, start)
	return first, uiLineOf(masked, start+len(body))
}

// uiBadTokensOutside 逐行扫 masked，凡在 [lo,hi] 行区间之外出现 bad 里的记号就报错。
func uiBadTokensOutside(t *testing.T, masked string, lo, hi int, why string, bad ...string) {
	t.Helper()
	for i, line := range strings.Split(masked, "\n") {
		lineNo := i + 1
		if lineNo >= lo && lineNo <= hi {
			continue
		}
		for _, tok := range bad {
			if strings.Contains(line, tok) {
				t.Errorf("ui/app.js 第 %d 行出现了 %q：%s", lineNo, tok, why)
			}
		}
	}
}

func TestUITimeRenderingIsUTC(t *testing.T) {
	data, err := os.ReadFile("ui/app.js")
	if err != nil {
		t.Fatalf("读取 ui/app.js: %v", err)
	}
	masked := uiMaskComments(string(data))

	// —— ① fmtTime 的口径：换算成 UTC 渲染 + 带标记 ——
	body, _ := uiFuncSpan(t, masked, "fmtTime")
	if !strings.Contains(body, "toISOString") {
		t.Error("fmtTime 没有按 UTC 渲染：应经 toISOString 取 UTC 分量（记录类时间一律 UTC，与 CLI/journalctl 同口径）")
	}
	if !strings.Contains(body, "' UTC'") {
		t.Error("fmtTime 的输出没有带 ' UTC' 标记：不带标记的绝对时间无法判断口径，操作者会当成自己本机时钟读")
	}
	if strings.Contains(body, "toLocale") {
		t.Error("fmtTime 里出现了本地时区格式化（toLocale*String）：记录类时间不许随浏览器时区变（控制台要与 CLI 工单对账）")
	}
	// —— ④ 不带时区标记的时间戳：按 UTC 字面量处理 ——
	// 交给 Date 解析会按**浏览器本地时区**理解（如 UTC+8 上把 01:24:22 当成 09:24:22Z），
	// 于是无标记输入又差一个偏移——这是本缺陷的第二种进入方式，故单独钉住。
	if !strings.Contains(body, "FMT_TIME_NAIVE_RE") {
		t.Error("fmtTime 没有处理「不带时区标记」的时间戳：服务端时间戳都是 UTC，须按 UTC 字面量渲染（别交给 Date 按浏览器时区解析）")
	}
	if hint, _ := uiFuncSpan(t, masked, "fmtClockHint"); strings.TrimSpace(hint) == "" {
		t.Fatal("ui/app.js 里的 fmtClockHint() 是空的：纯 UI 提示的本地时间要集中在这一个函数里")
	}

	// —— ② 本地时区格式化只允许出现在 fmtClockHint() ——
	hintLo, hintHi := uiFuncLines(t, masked, "fmtClockHint")
	uiBadTokensOutside(t, masked, hintLo, hintHi,
		"记录类时间必须走 fmtTime()（UTC + 标记）；只有**纯 UI 提示**（与任何记录无关，如「已刷新 时刻」）"+
			"才允许本地时间，且必须经 fmtClockHint()",
		"toLocaleString(", "toLocaleTimeString(", "toLocaleDateString(")

	// 这个例外要写明理由（本仓库对例外的纪律：例外必须自带理由，否则下一个人只会照着加）。
	hintBody, hintStart := uiFuncSpan(t, masked, "fmtClockHint")
	if !strings.Contains(hintBody, "toLocale") {
		t.Error("fmtClockHint 现在不渲染本地时间了：要么恢复它（唯一的本地时间出口），要么连同本守护一起收口")
	}
	// 理由在**注释**里，而 masked 已把注释抹成空格——故这里回到原文找它。
	origPre := string(data)[:hintStart]
	if i := strings.LastIndex(origPre, "\n\n"); i >= 0 {
		origPre = origPre[i:]
	}
	if !strings.Contains(origPre, "纯 UI 提示") {
		t.Errorf("fmtClockHint 上方注释必须写明它只服务「纯 UI 提示」这个例外；当前注释：%q", strings.TrimSpace(origPre))
	}

	// —— ③ 本地时区取数不许散落：只允许 fmtTimeOpt 里那处零值判定（判断"是不是 0001 年"）——
	optLo, optHi := uiFuncLines(t, masked, "fmtTimeOpt")
	uiBadTokensOutside(t, masked, optLo, optHi,
		"本地时区取数只允许出现在 fmtTimeOpt 的零值判定里；渲染记录类时间请用 fmtTime()（UTC）",
		"getHours(", "getMinutes(", "getSeconds(", "getFullYear(", "getMonth(", "getDate(", "getDay(")

	// —— ⑤ 调用点下限：记录类时间的渲染点不许被悄悄删掉/改回内联格式化 ——
	// 只数**代码行**（注释已抹掉，故解释文字不计数）。
	calls := 0
	for _, line := range strings.Split(masked, "\n") {
		if strings.Contains(line, "function ") {
			continue
		}
		if strings.Contains(line, "fmtTime(") || strings.Contains(line, "fmtTimeOpt(") {
			calls++
		}
	}
	if calls < 15 {
		t.Errorf("app.js 里 fmtTime/fmtTimeOpt 的代码调用点只剩 %d 处（登记下限 15）：记录类时间被删掉或改回内联格式化了？", calls)
	}
	t.Logf("记录类时间渲染点：%d 处（唯一出口 fmtTime/fmtTimeOpt，UTC + 标记）", calls)
}
