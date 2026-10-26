package api

// 接口统计卡的**字段级**守护（决策 #504，收口 R47-2 的 Web 侧）。
//
// 由来：接口详情页「Rx / Tx 丢弃」一行取 `st.rx_drops`/`st.tx_drops`，概览接口卡的
// 最后一列取 `r.stat.tx_drops`——而 VPP 数据面的 statistics **没有这两个键**（VPP 的
// `/interfaces/<if>/drops` 不分方向），两处于是恒显示「—」，而同一时刻真有丢弃。
// 与 R110-1（QoS 卡）、R7-2（交换机端口表）、round3（SPAN 卡）**同一类风险**：端点一直是对的，
// 错的是**消费的字段名**——路径级覆盖（ui_coverage_test.go）挡不住。
//
// 本守护把「两处丢弃渲染消费的字段 ⊆ 契约 Interface.statistics」机器化：契约里没有的字段
// 一出现即红；并钉住两个口径都在消费面覆盖（VPP 的 drops 与内核的 rx_drops/tx_drops）。

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// interfaceStatisticsSchemaPropsFromOpenapi 解析契约 Interface.statistics 的属性名集合。
// 解析结果缺任一已知字段即失败（排版变了守护必须跟着改，而不是静默空转）。
func interfaceStatisticsSchemaPropsFromOpenapi(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile("../../docs/NFViS-openapi.yaml")
	if err != nil {
		t.Fatalf("读取契约: %v", err)
	}
	lines := strings.Split(string(data), "\n")
	start := -1
	for i, l := range lines {
		// Interface schema 下的 statistics 块（8 空格缩进，紧随 sriov.vfs.items 之后）。
		if l == "        statistics:" {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("契约里找不到 Interface.statistics（排版变了？）")
	}
	props := map[string]bool{}
	// statistics 块里先有 type/description（10 空格键），再有 `properties:`（10 空格），
	// 属性本身在 12 空格。故先定位 properties:，再逐行取 12 空格键——
	// 只看 12 空格行会在 `properties:`（10 空格）处提前结束（首版就是这么空转的）。
	reProp := regexp.MustCompile(`^ {12}([a-z_]+):`)
	inProps := false
	for _, l := range lines[start+1:] {
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "          ") {
			break // statistics 块结束（缩进回到 8 空格或更浅）
		}
		if l == "          properties:" {
			inProps = true
			continue
		}
		if !inProps {
			continue
		}
		if m := reProp.FindStringSubmatch(l); m != nil {
			props[m[1]] = true
		}
	}
	for _, f := range []string{"rx_packets", "tx_packets", "rx_bytes", "tx_bytes",
		"rx_errors", "tx_errors", "drops", "rx_drops", "tx_drops"} {
		if !props[f] {
			t.Fatalf("Interface.statistics 没解析到 %q（实际 %v）——排版变了请同步本守护", f, props)
		}
	}
	return props
}

// TestUIInterfaceDropsFieldsExistInContract 两处丢弃渲染消费的字段都在契约里，
// 且**两种口径**（VPP 的 drops / 内核的 rx_drops+tx_drops）都在消费面被覆盖。
func TestUIInterfaceDropsFieldsExistInContract(t *testing.T) {
	props := interfaceStatisticsSchemaPropsFromOpenapi(t)
	text := jsAll(t)

	// ① 接口详情页（system.js）的统计块：`fill($('ifd-stat'), st ? [...]`。
	const detailAnchor = "fill($('ifd-stat'), st ? ["
	di := strings.Index(text, detailAnchor)
	if di < 0 {
		t.Fatalf("找不到接口详情统计块（锚点 %q 变了？）", detailAnchor)
	}
	detailEnd := strings.Index(text[di:], "] : [['读取失败'")
	if detailEnd < 0 {
		t.Fatalf("接口详情统计块结构变了（找不到收尾 \"…] : [['读取失败'\"）——请同步本守护")
	}
	detailBlock := text[di : di+detailEnd]

	// ② 概览接口卡（overview.js）的行渲染：`renderInterfaces` 里 `ifaceRows.forEach(...)` 的
	// 单元格数组（注意「（无）」空分支也有一处 tbody.appendChild(tr)，故锚点取 forEach 之后）。
	const cardAnchor = "ifaceRows.forEach((r) => {"
	ci := strings.Index(text, cardAnchor)
	if ci < 0 {
		t.Fatalf("找不到概览接口卡的行渲染（锚点 %q 变了？）", cardAnchor)
	}
	cardEnd := strings.Index(text[ci:], "tbody.appendChild(tr);")
	if cardEnd < 0 {
		t.Fatalf("接口卡渲染块结构变了（找不到 tbody.appendChild(tr)）——请同步本守护")
	}
	cardBlock := text[ci : ci+cardEnd]

	// 丢弃口径的**唯一实现**在 core/format.js（ifaceDropsText / ifaceDropsCell）：
	// 两处视图必须经它渲染，否则会出现「一处改了、另一处漏了」的漂移。
	const helperAnchor = "export function ifaceDropsText"
	hi := strings.Index(text, helperAnchor)
	if hi < 0 {
		t.Fatalf("找不到 ifaceDropsText（丢弃计数的统一渲染）——决策 #504 的单源实现被改动了？")
	}
	helperBlock := text[hi:]
	// helper 段落到下一个 export 之前
	if next := strings.Index(helperBlock[10:], "\nexport function "); next > 0 {
		helperBlock = helperBlock[:10+next]
	}

	// 判定 1：两处视图都经单源 helper（不各自写字段名）。
	for name, block := range map[string]string{"接口详情页": detailBlock, "概览接口卡": cardBlock} {
		if !strings.Contains(block, "ifaceDropsText") && !strings.Contains(block, "ifaceDropsCell") {
			t.Errorf("%s 的丢弃列没有经 ifaceDropsText/ifaceDropsCell 渲染（决策 #504 的单源口径）——"+
				"各写一份必然漂移（R110-1/R7-2 同一类缺陷）", name)
		}
		// 旧口径的字段不得再被这两处直接消费
		for _, legacy := range []string{".rx_drops", ".tx_drops"} {
			if strings.Contains(block, legacy) {
				t.Errorf("%s 仍直接消费 %q——VPP 数据面下该键缺席，会恒显示「—」（R47-2 的原缺陷）",
					name, legacy)
			}
		}
	}

	// 判定 2：单源 helper 覆盖**两种口径**，且消费的字段都在契约里。
	consumed := jsObjFields(helperBlock, "st")
	if len(consumed) == 0 {
		t.Fatalf("ifaceDropsText/ifaceDropsCell 里没提取到任何 st.<field>（提取规则失效）")
	}
	has := func(want string) bool {
		for _, f := range consumed {
			if f == want {
				return true
			}
		}
		return false
	}
	for _, f := range []string{"drops", "rx_drops", "tx_drops"} {
		if !has(f) {
			t.Errorf("丢弃渲染没消费 st.%s（VPP 与内核是两种口径，两种都要认）：提取到的 %v", f, consumed)
		}
		if !props[f] {
			t.Errorf("丢弃渲染消费了 %s，但契约 Interface.statistics 没有该属性——照契约开发的一方会取空", f)
		}
	}
}

// TestUIInterfaceTableHeaderSaysDrops 概览接口卡的列标题不得再写「Tx 丢弃」：
// VPP 口径给的是**方向无关总数**，标成 Tx 就是把一个说不清方向的数说成发送方向。
func TestUIInterfaceTableHeaderSaysDrops(t *testing.T) {
	data, err := os.ReadFile("ui/index.html")
	if err != nil {
		t.Fatalf("读取 index.html: %v", err)
	}
	html := string(data)
	i := strings.Index(html, `<table id="iface-table">`)
	if i < 0 {
		t.Fatal("找不到 iface-table（锚点变了？）")
	}
	j := strings.Index(html[i:], "</thead>")
	if j < 0 {
		t.Fatal("iface-table 的表头结构变了（找不到 </thead>）")
	}
	head := html[i : i+j]
	if strings.Contains(head, "Tx 丢弃") {
		t.Errorf("接口卡列标题仍写「Tx 丢弃」——VPP 的 drops 不分方向，标题须为「丢弃」：%q", head)
	}
	if !strings.Contains(head, "<th>丢弃</th>") {
		t.Errorf("接口卡应有「丢弃」列：%q", head)
	}
}
