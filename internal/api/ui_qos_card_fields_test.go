package api

// 网络对象页 QoS 策略卡的**字段级**守护（R110-1，决策 #332）。
//
// 缺陷：卡片「类型/目标」两列取 `q.type`/`q.target`/`q.interface` —— 这些字段在
// `/qos/policies` 的响应里**从来不存在**（#331 前是 {name,cir,cbs}，#331 后加
// bound_interfaces/bindings），卡片自决策 #128 引入起两列恒显示「—」，策略明明有
// 方向绑定、界面上却看不出绑到了哪个接口哪个方向。路径级覆盖（ui_coverage_test.go）
// 挡不住这类缺陷——端点一直是对的，错的是**消费的字段**。
//
// 本守护把「卡片消费的字段 ⊆ 契约 schema」机器化：从 ui/app.js 提取 QoS 卡行与其
// 绑定 formatter 引用的全部对象属性，逐个对照 openapi 的 QosPolicy schema
// （含 bindings.items）——契约里没有的字段一出现即红。

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// schemaPropsFromOpenapi 从 openapi yaml 里取 QosPolicy schema 的属性名：
// 顶层（name/cir/cbs/bound_interfaces/bindings）与 bindings.items 下的
// （interface/direction）。缩进按契约文件的现行排版；解析结果少于已知字段数即失败
// （排版变了守护必须跟着改，而不是静默空转）。
func schemaPropsFromOpenapi(t *testing.T) (top, bindingItem map[string]bool) {
	t.Helper()
	data, err := os.ReadFile("../../docs/NFViS-openapi.yaml")
	if err != nil {
		t.Fatalf("读取契约: %v", err)
	}
	lines := strings.Split(string(data), "\n")
	start := -1
	for i, l := range lines {
		if l == "    QosPolicy:" {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("契约里找不到 QosPolicy schema（排版变了？）")
	}
	top, bindingItem = map[string]bool{}, map[string]bool{}
	inBindings := false
	for _, l := range lines[start+1:] {
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "      ") {
			break // 下一个 schema 开始
		}
		if m := regexp.MustCompile(`^        ([a-z_]+):`).FindStringSubmatch(l); m != nil {
			top[m[1]] = true
			inBindings = m[1] == "bindings"
			continue
		}
		if m := regexp.MustCompile(`^              ([a-z_]+):`).FindStringSubmatch(l); m != nil && inBindings {
			bindingItem[m[1]] = true
		}
	}
	for _, f := range []string{"name", "cir", "cbs", "bound_interfaces", "bindings"} {
		if !top[f] {
			t.Fatalf("QosPolicy schema 顶层没解析到 %q（解析规则失效或契约改动）", f)
		}
	}
	for _, f := range []string{"interface", "direction"} {
		if !bindingItem[f] {
			t.Fatalf("QosPolicy.bindings.items 没解析到 %q（解析规则失效或契约改动）", f)
		}
	}
	return top, bindingItem
}

// jsObjFields 提取一段 JS 里 `x.<field>` 形式的属性访问。
func jsObjFields(src, obj string) []string {
	re := regexp.MustCompile(regexp.QuoteMeta(obj) + `\.([A-Za-z_][A-Za-z0-9_]*)`)
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

func TestUIQosCardFieldsExistInContract(t *testing.T) {
	src, err := os.ReadFile("ui/app.js")
	if err != nil {
		t.Fatalf("读取 ui/app.js: %v", err)
	}
	text := string(src)

	// 卡片行：NET_OBJECT_VIEWS 里锚定 'QoS 策略' 的一行（保持单行书写；
	// 将来折行需同步这里的提取规则）。
	rowRe := regexp.MustCompile(`(?m)^\s*\['QoS 策略'.*$`)
	row := rowRe.FindString(text)
	if row == "" {
		t.Fatalf("在 ui/app.js 里找不到 QoS 卡行（锚点 'QoS 策略' 变了？）")
	}
	rowFields := jsObjFields(row, "q")
	if len(rowFields) == 0 {
		t.Fatalf("QoS 卡行没提取到任何 q.<字段> 访问（提取规则失效）")
	}

	// 绑定 formatter（列表卡与详情页共用）：q.* 对 QosPolicy 顶层、b.* 对 bindings.items。
	helperStart := strings.Index(text, "function qosBindingsText(q) {")
	if helperStart < 0 {
		t.Fatalf("在 ui/app.js 里找不到 qosBindingsText（卡片与详情页的绑定文本必须共用它）")
	}
	helperEnd := strings.Index(text[helperStart:], "\n}\n")
	if helperEnd < 0 {
		t.Fatalf("qosBindingsText 函数体没有预期收尾（\\n}\\n）")
	}
	helper := text[helperStart : helperStart+helperEnd]

	top, bindingItem := schemaPropsFromOpenapi(t)

	for _, f := range rowFields {
		if !top[f] {
			t.Errorf("QoS 卡行消费了 q.%s，但契约 QosPolicy schema 没有该属性——这就是 R110-1 的复刻（字段写了、响应里没有，列恒「—」）", f)
		}
	}
	for _, f := range jsObjFields(helper, "q") {
		if !top[f] {
			t.Errorf("qosBindingsText 消费了 q.%s，但契约 QosPolicy schema 没有该属性", f)
		}
	}
	for _, f := range jsObjFields(helper, "b") {
		if !bindingItem[f] {
			t.Errorf("qosBindingsText 消费了 b.%s，但契约 QosPolicy.bindings.items 没有该属性", f)
		}
	}
}
