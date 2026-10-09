package api

// 网络对象页 **SPAN（端口镜像）卡**的字段级守护（round3 修复批的 Web 收口发现）。
//
// 缺陷：卡片「源/目的」两列取 `s.sources || s.source`（对象直接过 `list()` → `[object Object]`）
// 与 `s.destination`（契约里字段名是 `analyzer`，该字段从来不存在 ⇒ 恒「—」）。与 R110-1
// （QoS 卡字段错位，决策 #332）**同一类**：端点一直是对的，错的是卡片消费的字段名。
//
// 本守护把「SPAN 卡消费的字段 ⊆ 契约 schema」机器化：从 ui/app.js 提取 SPAN 卡行与其
// 源文本 formatter（`spanSourceText`）引用的对象属性，逐个对照 openapi 的 PortMirroring
// schema（含 source 子对象）——契约里没有的字段一出现即红。

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// spanSchemaPropsFromOpenapi 取 openapi 里 PortMirroring 的顶层属性与 source 子属性。
// 解析结果少于已知字段数即失败（排版变了守护必须跟着改，而不是静默空转）。
func spanSchemaPropsFromOpenapi(t *testing.T) (top, src map[string]bool) {
	t.Helper()
	data, err := os.ReadFile("../../docs/NFViS-openapi.yaml")
	if err != nil {
		t.Fatalf("读取契约: %v", err)
	}
	lines := strings.Split(string(data), "\n")
	start := -1
	for i, l := range lines {
		if l == "    PortMirroring:" {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("契约里找不到 PortMirroring schema")
	}
	top, src = map[string]bool{}, map[string]bool{}
	// 该 schema 的固定缩进：`    PortMirroring:`(4) / `      properties:`(6) /
	// 顶层属性(8) / `        source:` 下 `          properties:`(10) / source 子属性(12)。
	reTopKey := regexp.MustCompile(`^ {8}([A-Za-z_][A-Za-z0-9_]*):`)
	reSrcKey := regexp.MustCompile(`^ {12}([A-Za-z_][A-Za-z0-9_]*):`)
	for i := start + 1; i < len(lines); i++ {
		l := lines[i]
		if strings.HasPrefix(l, "    ") && !strings.HasPrefix(l, "      ") && strings.TrimSpace(l) != "" {
			break // 下一个顶层 schema
		}
		if m := reTopKey.FindStringSubmatch(l); m != nil {
			top[m[1]] = true
			continue
		}
		if m := reSrcKey.FindStringSubmatch(l); m != nil {
			src[m[1]] = true
		}
	}
	for _, want := range []string{"name", "source", "analyzer"} {
		if !top[want] {
			t.Fatalf("契约 PortMirroring 顶层属性解析缺 %q（实际 %v）——排版变了请同步本守护", want, top)
		}
	}
	for _, want := range []string{"interface", "vnf", "vnf_interface", "direction"} {
		if !src[want] {
			t.Fatalf("契约 PortMirroring.source 属性解析缺 %q（实际 %v）——排版变了请同步本守护", want, src)
		}
	}
	return top, src
}

// spanCardSnippet 从 app.js 取 SPAN 卡行与其源 formatter 的实现片段。
func spanCardSnippet(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("ui/app.js")
	if err != nil {
		t.Fatalf("读取 app.js: %v", err)
	}
	src := string(data)
	// 卡片行：以「端口镜像（SPAN）」开头的那条表项（到下一行以 `  ['` 或 `  //` 开头为止）。
	start := strings.Index(src, "['端口镜像（SPAN）'")
	if start < 0 {
		t.Fatalf("app.js 里找不到 SPAN 卡行")
	}
	end := strings.Index(src[start:], "], '#/network/span/'")
	if end < 0 {
		t.Fatalf("SPAN 卡行结构变了（找不到结尾）——请同步本守护")
	}
	card := src[start : start+end+len("], '#/network/span/'")]
	// formatter：spanSourceText 函数体（取到下一个顶层 `}` 行）。
	fstart := strings.Index(src, "function spanSourceText(")
	if fstart < 0 {
		t.Fatalf("app.js 里找不到 spanSourceText")
	}
	fend := strings.Index(src[fstart:], "\n}\n")
	if fend < 0 {
		t.Fatalf("spanSourceText 结构变了——请同步本守护")
	}
	return card + "\n" + src[fstart:fstart+fend]
}

// TestUISpanCardFieldsExistInContract：SPAN 卡（含源 formatter）引用的对象属性必须都在契约里。
func TestUISpanCardFieldsExistInContract(t *testing.T) {
	top, srcProps := spanSchemaPropsFromOpenapi(t)
	snippet := spanCardSnippet(t)

	// 卡片行里的 `s.<字段>`（不含 formatter 内部形参）与 formatter 里的 `src.<字段>`。
	reTop := regexp.MustCompile(`\bs\.([A-Za-z_][A-Za-z0-9_]*)`)
	reSrc := regexp.MustCompile(`\bsrc\.([A-Za-z_][A-Za-z0-9_]*)`)
	cardOnly := snippet
	if i := strings.Index(snippet, "function spanSourceText("); i >= 0 {
		cardOnly = snippet[:i]
	}

	var bad []string
	for _, m := range reTop.FindAllStringSubmatch(cardOnly, -1) {
		f := m[1]
		if !top[f] {
			bad = append(bad, "卡片行引用了契约 PortMirroring 里不存在的字段 s."+f)
		}
	}
	seen := map[string]bool{}
	for _, m := range reSrc.FindAllStringSubmatch(snippet, -1) {
		f := m[1]
		if seen[f] {
			continue
		}
		seen[f] = true
		if !srcProps[f] {
			bad = append(bad, "spanSourceText 引用了契约 source 里不存在的字段 src."+f)
		}
	}
	if len(bad) > 0 {
		t.Fatalf("SPAN 卡字段与契约不一致：\n  - %s", strings.Join(bad, "\n  - "))
	}
	// 自校准：断言确实取到了字段（解析空转时不许「通过」）。
	if len(reTop.FindAllStringSubmatch(cardOnly, -1)) == 0 || len(seen) == 0 {
		t.Fatalf("字段提取为空（卡片行或 formatter 结构变了）——守护必须跟着改，不允许空转通过")
	}
}
