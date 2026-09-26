package api

// display set：配置 JSON 树 → set 语句（决策 #155，推翻 #84 的搁置）。
//
// 三层机制：
//  1. 通用逆走器（emitMechanical）：沿 ConfigPathTree 与 JSON 树同步下钻。关键字⇄JSON 键
//     是机械双射（jsonKeyOf = 关键字名横线转下划线），配合 identityFields（数组身份字段）、
//     IVK 身份取值、ScalarParam 标量参数、valueTransforms 的逆格式化，覆盖全部无别名家族。
//     树里没有对应关键字的 JSON 键（别名家族，如 login user ⇄ users）会被**跳过**——
//     绝不猜着生成语句，缺失由回放自校验暴露。
//  2. 家族发射器（subtreeEmitters）：别名家族按 schema **关键字路径**接管（不含身份取值），
//     与 cli_aliases_*.go 的别名 apply 一一对照维护（见 setstmt_families.go）。
//  3. 回放自校验（verifySetStatements）：生成的语句（掩码前）经真实 applyStatement
//     回放进空配置，导航到同一层级后与原子树深比较——不等即报内部错误。display set
//     在结构上不可能输出还原不了的语句（#84 担心的「复制配置静默错误」被排除）。
//
// 敏感值不输出（w.note 注释说明，回放校验两侧剥离）；引号在输出拼接时统一加；输出确定性：
// 关键字按名排序、数组保持声明序，同配置两次生成逐字相同。

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/schema"
)

// setStmt 一条生成的 set 语句（原值 token；引号在输出拼接时统一加）。
type setStmt struct {
	toks []string
}

// stmtWriter 语句收集器：家族发射器与通用逆走器共用。
type stmtWriter struct {
	out   []setStmt
	notes []string // 语句不可表达区域的注释（输出为 # 行，如口令哈希）
}

func (w *stmtWriter) add(toks []string) {
	w.out = append(w.out, setStmt{toks: toks})
}

// note 登记一条不可表达区域的注释。口令哈希是典型：语句语法没有直设哈希的形态，
// 输出 `password «已隐藏»` 会让复制者把占位符哈希成真口令——比不输出更危险，
// 故一律不输出、只注释（决策 #155；回放自校验两侧同步剥离该类字段）。
func (w *stmtWriter) note(format string, args ...any) {
	w.notes = append(w.notes, fmt.Sprintf(format, args...))
}

// stripUnreplayable 递归剥离「语句不可表达」的敏感叶（IsSensitiveKey）：回放比较
// 两侧同口径，display set 输出以注释代替。
func stripUnreplayable(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			if model.IsSensitiveKey(k) {
				continue
			}
			out[k] = stripUnreplayable(vv)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = stripUnreplayable(vv)
		}
		return out
	default:
		return v
	}
}

// subtreeEmitters 别名家族逆映射发射器：键 = 从配置根起的关键字路径（空格连接、
// 不含身份取值，如 "system login"）。命中规则 = 注册键是当前 keyPath 的**路径前缀**，
// 命中后整棵子树由发射器接管（通用逆走器不再下钻）。发射器可能在注册层级或其下
// 任一层级被进入（配置模式从 edit 层级起步时）：val 为进入节点对应的 JSON 值，
// keyPath 供发射器判断进入深度；需要处理机械子区域时调 emitMechanicalInner。
// 未注册而树里又没有机械路径的键，回放自校验会如实报错而不是少输出。
var subtreeEmitters = map[string]func(w *stmtWriter, node *schema.Node, val any, prefix, keyPath []string) error{}

// rootFallbackEmitters 顶层 JSON 键的回落发射器：命令树没有对应顶层关键字、
// 但别名语句可表达的家族（决策 #155：vrfs 由 vs type l3 派生、qos_policies 由
// qos policies 别名落库——两者模型键都不是树关键字，通用逆走不可达）。
// 仅在根层、关键字子节点未消费的 JSON 键上触发。
var rootFallbackEmitters = map[string]func(w *stmtWriter, val any) error{}

// emitterFor 返回 keyPath 命中的家族发射器（注册键 = keyPath 的路径前缀）。
func emitterFor(keyPath []string) (func(w *stmtWriter, node *schema.Node, val any, prefix, keyPath []string) error, bool) {
	if len(keyPath) == 0 {
		return nil, false
	}
	kp := strings.Join(keyPath, " ")
	for k, em := range subtreeEmitters {
		if kp == k || strings.HasPrefix(kp, k+" ") {
			return em, true
		}
	}
	return nil, false
}

// renderSetStatements 配置 JSON 树 → set 语句行（可直接输出）。
// tree 为待渲染（子）树，prefix 为其在整配置中的绝对路径（顶层为空）。
// 敏感值不出现在输出中：以 # 注释行说明（见 stmtWriter.note）。
func renderSetStatements(tree map[string]any, prefix []string) ([]string, error) {
	w, err := generateSetStmts(tree, prefix)
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(w.out)+len(w.notes))
	for _, s := range w.out {
		lines = append(lines, joinStatementTokens(append([]string{"set"}, s.toks...)))
	}
	for _, n := range w.notes {
		lines = append(lines, "# "+n)
	}
	return lines, nil
}

// generateSetStmts 生成 + 回放自校验（原始语句，不含掩码/引号——引号在输出拼接时加）。
func generateSetStmts(tree map[string]any, prefix []string) (*stmtWriter, error) {
	// 前缀非空（配置模式层级 show）：按语句语法 Match 到起始节点与纯关键字路径——
	// 子树的键相对该节点而言，不能从根遍历（根的子键在子树里不存在）
	start, kp := cfgPathRoot(), []string{}
	if len(prefix) > 0 {
		n, k, err := schema.MatchKeywordPath(cfgPathRoot(), prefix)
		if err != nil {
			return nil, fmt.Errorf("display set 内部错误：层级 %s 与语句树不匹配（%v）", strings.Join(prefix, " "), err)
		}
		start, kp = n, k
	}
	w := &stmtWriter{}
	if err := emitMechanical(w, start, any(tree), prefix, kp); err != nil {
		return nil, err
	}
	if err := verifySetStatements(tree, prefix, w.out); err != nil {
		return nil, err
	}
	return w, nil
}

// verifySetStatements 回放自校验：语句应用到空配置，导航到 prefix 层级后必须与
// 待渲染子树深比较一致（语句生成用原值，掩码发生在校验之后）。
func verifySetStatements(tree map[string]any, prefix []string, stmts []setStmt) error {
	var cfg model.Config
	for _, s := range stmts {
		if err := applyStatement(&cfg, s.toks); err != nil {
			return fmt.Errorf("display set 内部错误：生成语句回放失败（%v）——请改用 show configuration / | display json 并报告缺陷", err)
		}
	}
	got, err := navigateJSON(toJSONTree(cfg), prefix)
	if err != nil {
		return fmt.Errorf("display set 内部错误：回放结果缺少层级 %s——请改用 show configuration / | display json 并报告缺陷", strings.Join(prefix, " "))
	}
	if !reflect.DeepEqual(normalizeJSON(stripUnreplayable(stripDefaultTrue(got))),
		normalizeJSON(stripUnreplayable(stripDefaultTrue(any(tree))))) {
		return fmt.Errorf("display set 内部错误：生成语句未能完整还原配置——请改用 show configuration / | display json 并报告缺陷")
	}
	return nil
}

// stripDefaultTrue 剥离「显式 true = 缺省态」的语句不可表达字段：interfaces[].enabled=true
// 与缺席**语义相同**（Enabled 未显式置 false 即启用，interface_alarm 同口径），而语句语法
// 只有 disable（enabled=true 只由 request interfaces enable / REST 写入，无 set 形态）。
// 两侧同剥不损失回放等价性；LLDP 等其它 enabled 字段有真语句，不在此列（round81 真机实测：
// request interfaces ens224 enable 落库后 display set 曾被自校验如实拦下）。
func stripDefaultTrue(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	if arr, ok := m["interfaces"].([]any); ok {
		for _, el := range arr {
			if em, ok := el.(map[string]any); ok {
				if b, ok := em["enabled"].(bool); ok && b {
					delete(em, "enabled")
				}
			}
		}
	}
	return m
}

// normalizeJSON 深拷贝并规整比较口径（两侧来源同为 toJSONTree，此处仅防御容器类型差异）。
func normalizeJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			out[k] = normalizeJSON(vv)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = normalizeJSON(vv)
		}
		return out
	default:
		return v
	}
}

// emitMechanical 通用逆走器。node = 当前 schema 节点；val = 该节点对应 JSON 值
// （容器位置应为 map）；prefix = 语句已到达的绝对路径（含身份取值）；
// keyPath = 纯关键字路径（家族发射器注册键，不含身份取值）。
func emitMechanical(w *stmtWriter, node *schema.Node, val any, prefix, keyPath []string) error {
	// 家族发射器命中（注册键 = keyPath 的路径前缀）：整棵子树交给别名家族的
	// 逆映射；发射器内部处理机械子区域时用 emitMechanicalInner 避免二次命中
	if em, ok := emitterFor(keyPath); ok {
		return em(w, node, val, prefix, keyPath)
	}
	return emitMechanicalInner(w, node, val, prefix, keyPath)
}

// emitMechanicalInner 通用逆走器本体（不做发射器命中检查，供家族发射器
// 处理自己的机械子区域时复用）。
func emitMechanicalInner(w *stmtWriter, node *schema.Node, val any, prefix, keyPath []string) error {
	m, ok := val.(map[string]any)
	if !ok {
		return nil // 容器位置不是 map：跳过，交由回放自校验暴露缺失
	}
	// 关键字按名排序：输出确定（同配置两次生成逐字相同）
	kids := make([]*schema.Node, 0, len(node.Children))
	for _, c := range node.Children {
		if c.Kind == schema.Keyword {
			kids = append(kids, c)
		}
	}
	sort.Slice(kids, func(i, j int) bool { return kids[i].Name < kids[j].Name })

	for _, c := range kids {
		p2 := append(append([]string{}, prefix...), c.Name)
		kp2 := append(append([]string{}, keyPath...), c.Name)

		// 标量参数透明层（applyTokens 分支 2a 镜像）：关键字下的 SP 参数取值
		// 写在父容器的 ScalarJSONKey 字段，取值 token 紧跟关键字之后
		if fp := firstParamOf(c); fp != nil && fp.ScalarParam {
			v, ok := m[fp.ScalarJSONKey]
			if !ok {
				continue
			}
			if fp.ScalarIsArray {
				arr, ok := v.([]any)
				if !ok {
					continue
				}
				for _, el := range arr {
					p3 := append(append([]string{}, p2...), formatScalar(el))
					w.add(p3)
					if len(fp.Children) > 0 {
						if err := emitMechanical(w, fp, m, p3, kp2); err != nil {
							return err
						}
					}
				}
				continue
			}
			p3 := append(p2, formatScalar(v))
			w.add(p3)
			if len(fp.Children) > 0 {
				if err := emitMechanical(w, fp, m, p3, kp2); err != nil {
					return err
				}
			}
			continue
		}

		// disable 旗标（applyTokens 特判镜像）：语句 `disable` 映射 enabled=false
		if c.Name == "disable" {
			if v, ok := m["enabled"]; ok && v == false {
				w.add(p2)
			}
			continue
		}

		k := jsonKeyOf(c)
		v, ok := m[k]
		if !ok {
			continue // 树里有、JSON 里没有：未配置，跳过
		}

		// IVK 身份取值容器（如 `resource-pools hugepages page-size 1G count 32`）：
		// 身份关键字 token 在语句中透明存在，取值即元素身份（applyTokens 分支 1 镜像）
		if len(c.Children) > 0 && c.Children[0].IdentityValue {
			ivk := c.Children[0]
			arr, ok := v.([]any)
			if !ok {
				continue
			}
			ident := identityFields[k]
			if ident == "" {
				ident = "name"
			}
			for _, el := range arr {
				em, ok := el.(map[string]any)
				if !ok {
					continue
				}
				iv, ok := em[ident]
				if !ok {
					continue
				}
				p3 := append(append([]string{}, p2...), ivk.Name, formatScalar(iv))
				// 元素内子语句（如 count）是 IVK 关键字的子节点
				if err := emitMechanical(w, ivk, em, p3, append(kp2, ivk.Name)); err != nil {
					return err
				}
			}
			continue
		}

		// 具名数组容器（applyTokens 分支 3 镜像）
		if fp := firstParamOf(c); fp != nil && !fp.ScalarParam {
			arr, ok := v.([]any)
			if !ok {
				continue
			}
			ident := identityFields[k]
			if ident == "" {
				ident = "name"
			}
			for _, el := range arr {
				em, ok := el.(map[string]any)
				if !ok {
					continue
				}
				iv, ok := em[ident]
				if !ok {
					continue
				}
				p3 := append(append([]string{}, p2...), formatScalar(iv))
				mark := len(w.out)
				// 裸声明本身有意义的节点先发裸语句（决策 #72）；RequireSub 节点
				// 不许裸成句（附录 A #90），只发子语句
				if !fp.RequireSub {
					w.add(append([]string{}, p3...))
				}
				if err := emitMechanical(w, fp, em, p3, kp2); err != nil {
					return err
				}
				if fp.RequireSub && len(w.out) == mark {
					return fmt.Errorf("display set 内部错误：%s 须有子语句但无法反推（RequireSub）", strings.Join(p3, " "))
				}
			}
			continue
		}

		// 值叶子关键字（applyTokens 分支 4 镜像）
		if sv := singleValueOf(c); sv != nil && len(c.Children) == 1 {
			if model.IsSensitiveKey(k) {
				// 敏感值不输出（无「直设哈希/秘密」的语句形态，占位符回放会造成
				// 静默的凭据替换）：注释说明 + 回放校验两侧剥离（stripUnreplayable）
				w.note("%s：敏感值不入 display set（复制配置后需重新 set %s）",
					strings.Join(p2, " "), c.Name)
				continue
			}
			w.add(append(p2, formatValue(k, v)))
			continue
		}

		// 对象容器（applyTokens 分支 5 镜像）
		if err := emitMechanical(w, c, v, p2, kp2); err != nil {
			return err
		}
	}

	// 根层回落：没有任何关键字子节点消费的顶层 JSON 键交给回落发射器
	// （键排序保证输出确定；家族发射器在关键字循环里已跑完，回放顺序在前）
	if len(keyPath) == 0 {
		consumed := map[string]bool{}
		for _, c := range node.Children {
			if c.Kind == schema.Keyword {
				consumed[jsonKeyOf(c)] = true
			}
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if consumed[k] {
				continue
			}
			if em, ok := rootFallbackEmitters[k]; ok {
				if err := em(w, m[k]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// formatValue 值叶子取值的逆格式化：产出能被 valueTransforms/scalarForNode
// 重新解析为同一 JSON 值的 token。
func formatValue(jsonKey string, v any) string {
	// valueTransforms 的逆（cliexec.go）：核列表 [0,1,2,3] ⇄ "0,1,2,3"
	switch jsonKey {
	case "isolated_cores", "cores":
		if arr, ok := v.([]any); ok {
			parts := make([]string, 0, len(arr))
			for _, el := range arr {
				parts = append(parts, formatScalar(el))
			}
			return strings.Join(parts, ",")
		}
	}
	return formatScalar(v)
}

// formatScalar 标量的逆格式化：bool/数字按 typedScalar 的解析口径输出；
// 字符串**原样**返回——引号由 joinStatementTokens 在输出拼接时统一加
// （token 里提前带引号会被分词器当成值的一部分，回放即不等）。
func formatScalar(v any) string {
	switch x := v.(type) {
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case string:
		return x
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// quoteStatementToken 取值 token 的引号规则与语句分词器 splitFieldsQuoted 对偶：
// 含空白/引号/反斜杠或为空的 token 必须加引号（内部 " 与 \ 转义）。
func quoteStatementToken(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"\\") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	b.WriteByte('"')
	return b.String()
}

// joinStatementTokens 语句行拼接：首个 token（固定前缀 set）原样，其余按引号规则。
func joinStatementTokens(toks []string) string {
	parts := make([]string, 0, len(toks))
	for i, t := range toks {
		if i > 0 {
			t = quoteStatementToken(t)
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, " ")
}

// ifaceListRowFmt 运行态清单行格式（比单口视图多一列「来源」标注）。
const ifaceListRowFmt = "%-14s %-7s %-7s %-10s %-12s %-10s %-12s %-20s %s\n"
