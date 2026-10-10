package api

// 交换机详情页端口表的**字段级**守护（决策 #444，收口 R7-2 的 Web 侧）。
//
// 背景：页面运行态列此前只能从 `statistics.ports` 合并（宿主端按决策 #441 被有意过滤 ⇒
// 内核数据面下容器派生条目恒「—」，而 CLI 同行显示 up/up）；本决策后
// `GET /virtual-switches/{name}/ports` 逐行自带 admin/link/rx_packets/tx_packets（与 CLI
// 同一实现按展示名合并），页面改为「行内字段优先、statistics 回退」。与 R110-1（QoS 卡）、
// round3（SPAN 卡）**同一类风险**：端点一直是对的，错的是**消费的字段名**（拼错或契约没声明
// ⇒ 列恒「—」）。故用静态守护把「端口表消费的字段 ⊆ 契约 SwitchPortView schema」机器化
// （写法参照 ui_qos_card_fields_test.go / ui_span_card_fields_test.go，复用其 jsObjFields）。

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// switchPortViewSchemaPropsFromOpenapi 解析契约 SwitchPortView schema：属性名集合与 required 行。
// 解析结果缺任一已知字段即失败（排版变了守护必须跟着改，而不是静默空转）。
func switchPortViewSchemaPropsFromOpenapi(t *testing.T) (props map[string]bool, requiredLine string) {
	t.Helper()
	data, err := os.ReadFile("../../docs/NFViS-openapi.yaml")
	if err != nil {
		t.Fatalf("读取契约: %v", err)
	}
	lines := strings.Split(string(data), "\n")
	start := -1
	for i, l := range lines {
		if l == "    SwitchPortView:" {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("契约里找不到 SwitchPortView schema（排版变了？）")
	}
	props = map[string]bool{}
	reProp := regexp.MustCompile(`^ {8}([a-z_]+):`)
	for _, l := range lines[start+1:] {
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "      ") {
			break // 下一个顶层 schema（4 空格缩进）
		}
		if m := reProp.FindStringSubmatch(l); m != nil {
			props[m[1]] = true
			continue
		}
		if strings.HasPrefix(l, "      required:") {
			requiredLine = l
		}
	}
	for _, f := range []string{"source", "port", "interface", "vnf", "container", "admin", "link", "rx_packets", "tx_packets"} {
		if !props[f] {
			t.Fatalf("SwitchPortView schema 没解析到 %q（实际 %v）——排版变了请同步本守护", f, props)
		}
	}
	return props, requiredLine
}

// TestUISwitchPortTableFieldsExistInContract：端口表（行内优先判断 + 表渲染块）消费的
// 运行态字段必须都在契约里，且是**条件字段**（取不到不出现，不得进 required）。
func TestUISwitchPortTableFieldsExistInContract(t *testing.T) {
	props, requiredLine := switchPortViewSchemaPropsFromOpenapi(t)
	src, err := os.ReadFile("ui/app.js")
	if err != nil {
		t.Fatalf("读取 ui/app.js: %v", err)
	}
	text := string(src)

	// ① 行内运行态列优先判断（决策 #444）：`const hasRowRT = p.admin !== undefined || …`。
	prioStart := strings.Index(text, "const hasRowRT")
	if prioStart < 0 {
		t.Fatalf("在 ui/app.js 里找不到行内运行态列优先判断（const hasRowRT）——决策 #444 的 Web 优先序被改动？")
	}
	prioEnd := strings.Index(text[prioStart:], "rt: hasRowRT ? p : rtByName[label] });")
	if prioEnd < 0 {
		t.Fatalf("优先判断与 rows.push 的衔接变了（找不到 rt: hasRowRT ? p : rtByName[label]）——请同步本守护")
	}
	prioFields := jsObjFields(text[prioStart:prioStart+prioEnd], "p")

	// ② 端口表渲染块（行内字段与 statistics 回退共用同一渲染，消费 q.*）。
	tableStart := strings.Index(text, "$('vsd-port-table')")
	if tableStart < 0 {
		t.Fatalf("在 ui/app.js 里找不到端口表渲染块（$('vsd-port-table')）")
	}
	tableEnd := strings.Index(text[tableStart:], "}));")
	if tableEnd < 0 {
		t.Fatalf("端口表渲染块结构变了（找不到收尾 }));）——请同步本守护")
	}
	tableFields := jsObjFields(text[tableStart:tableStart+tableEnd], "q")

	has := func(xs []string, want string) bool {
		for _, x := range xs {
			if x == want {
				return true
			}
		}
		return false
	}
	// 自校准：两处都必须提取到全部四个运行态字段（字段名拼错即失败，不允许空转通过）。
	for _, f := range []string{"admin", "link", "rx_packets", "tx_packets"} {
		if !has(prioFields, f) {
			t.Fatalf("行内优先判断没提取到 p.%s（提取到的：%v；字段名拼错或结构变了）", f, prioFields)
		}
		if !has(tableFields, f) {
			t.Fatalf("端口表渲染块没提取到 q.%s（提取到的：%v；字段名拼错或结构变了）", f, tableFields)
		}
		if !props[f] {
			t.Errorf("ui/app.js 端口表消费了 %s，但契约 SwitchPortView schema 没有该属性——照契约/照页面开发的一方会取空", f)
		}
	}
	// 四个运行态字段是条件字段（取不到不出现），不得进 required。
	for _, f := range []string{"admin", "link", "rx_packets", "tx_packets"} {
		if regexp.MustCompile(`\b` + f + `\b`).MatchString(requiredLine) {
			t.Errorf("契约 SwitchPortView.required 不应含 %s（运行态取不到时该键不出现——条件字段）: %q", f, requiredLine)
		}
	}
	// 全量双向：提取到的每个字段都必须在契约里（防将来加了新字段却漏改契约）。
	for _, f := range append(append([]string{}, prioFields...), tableFields...) {
		if !props[f] {
			t.Errorf("ui/app.js 端口表消费了 %q，但契约 SwitchPortView schema 没有该属性", f)
		}
	}
}
