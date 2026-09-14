// Package schema 定义 NFViS CLI 命令树 schema（FR-CLI-001~007）。
//
// 命令树被 nfvis-cli（?/Tab 补全与本地语法提示）与 nfvisd（API 校验与
// cli_bridge 端点）编译期共享——两个二进制同 deb 同版本发布，无漂移风险
// （骨架 §3.3）。命令树与执行器必须同源（AGENTS.md 常见错误）。
//
// 节点三种：
//   - Keyword 固定关键字（可缩写，无歧义前缀即合法，§5.5）；
//   - Value   取值叶子（值不入路径，如 hostname <string>；支持枚举补全）；
//   - Param   实例参数（实例名入路径，可携带子树，如 <name> 下挂资源配置，
//     与「参数节点双重语义」约定一致——AGENTS.md 常见错误第 4 条）。
//
// 匹配语义：值叶子与无子树参数消耗一个 token 后回到父关键字层继续匹配
// （如 `static-routes <prefix> next-hop <ip> distance <n>`）；连续无子树参数
// （cross-connect <a> <b>）按首参重复匹配，参数个数与取值合法性由执行期校验。
package schema

import (
	"fmt"
	"sort"
	"strings"
)

// Kind 节点类别。
type Kind uint8

const (
	Keyword Kind = iota // 固定关键字
	Value               // 取值叶子（值不入路径）
	Param               // 实例参数（实例名入路径）
)

func (k Kind) String() string {
	switch k {
	case Keyword:
		return "keyword"
	case Value:
		return "value"
	default:
		return "param"
	}
}

// Class 执行该节点所需的最低 login class（§4 预置权限矩阵；O 含 R，S 含全部）。
type Class uint8

const (
	ClassReadOnly  Class = iota // R：仅 show
	ClassOperator               // O：show + request 生命周期/镜像/接口 + ping
	ClassSuperUser              // S：全部（含 configure）
)

func (c Class) String() string {
	switch c {
	case ClassReadOnly:
		return "R"
	case ClassOperator:
		return "O"
	default:
		return "S"
	}
}

// 动态候选来源（§5.3：实时向 nfvisd 查询，失败退化为仅关键字）。
const (
	DynIfnames    = "ifnames"      // 接口清单
	DynVSwitches  = "vswitches"    // 虚拟交换机清单
	DynVrfs       = "vrfs"         // VRF（L3 交换机）清单
	DynVMs        = "vmnames"      // VM VNF 清单
	DynContainers = "ctnames"      // 容器 VNF 清单
	DynImages     = "images"       // 镜像清单
	DynClasses    = "classes"      // login class 清单
	DynRevisions  = "revisions"    // 配置快照编号
	DynVppPlugins = "vppplugins"   // VPP 插件名
	DynAcls       = "acls"         // ACL 清单
	DynQos        = "qos-policies" // 限速策略清单
)

// PipeKeywords 通用管道关键字（FR-CLI-005，对一切 show 输出可用）。
// schema 单一来源，CLI 前端据此解析管道段。
var PipeKeywords = []string{"match", "except", "count", "last", "begin", "display"}

// Node 命令树节点。
type Node struct {
	Kind      Kind
	Name      string   // 关键字名 / 参数占位符（如 "<name>"）/ 值类型提示
	Desc      string   // 帮助描述（? 显示）
	MinClass  Class    // 所需最低 class
	Children  []*Node  // 参数节点可直接携带子树
	ParamType string   // Param/Value 的类型（name|ifname|uint|ip-prefix|...，§0 阅读约定）
	Enum      []string // Value 的枚举候选（如 l2|l3；Tab 可补全）
	Dynamic   string   // Param 的动态候选来源 kind
	Optional  bool     // [] 可选（如 ports [<seq>]、confirmed [minutes]）

	// ScalarParam 标量参数：该参数的取值在配置模型中是标量字段（而非具名数组
	// 元素的实例名），ScalarJSONKey 为其 JSON 键（如 ports 下 interface <ifname>
	// → VSwitchPort.interface）。cli_bridge 执行期翻译据此区分两类参数。
	ScalarParam   bool
	ScalarJSONKey string
	// ScalarIsArray：该标量参数在模型中是**数组**字段（如 dns_servers/params/
	// bond members）。执行期翻译据此追加取值而非覆盖，并支持按值删除。
	ScalarIsArray bool

	// IdentityValue 身份取值关键字：其取值是父层具名数组的元素身份
	//（如 hugepages page-size <2M|1G> 的 1G 即 HPool 元素的 page_size 身份）。
	IdentityValue bool

	parent *Node // finalize 时回填，用于值/无子树参数消耗后的层级回退
}

// ---------- 构造辅助 ----------

// K 固定关键字节点。
func K(name, desc string, children ...*Node) *Node {
	return &Node{Kind: Keyword, Name: name, Desc: desc, Children: children}
}

// V 取值叶子（值不入路径）。
func V(typ, desc string) *Node {
	return &Node{Kind: Value, Name: "<" + typ + ">", Desc: desc, ParamType: typ}
}

// VE 带枚举候选的取值叶子。
func VE(typ, desc string, enums ...string) *Node {
	n := V(typ, desc)
	n.Enum = enums
	return n
}

// P 实例参数节点（实例名入路径，可携带子树）。
func P(placeholder, desc, dynamic string, children ...*Node) *Node {
	return &Node{Kind: Param, Name: placeholder, Desc: desc, Dynamic: dynamic, Children: children, ParamType: "name"}
}

// SP 标量参数节点：取值不进路径、作为父关键字下的标量字段存储
// （如 `ports 1 interface ens2f0` 的 ens2f0 → VSwitchPort.interface）。
func SP(placeholder, jsonKey, desc string) *Node {
	return &Node{Kind: Param, Name: placeholder, Desc: desc, ScalarParam: true, ScalarJSONKey: jsonKey, ParamType: "name"}
}

// SPA：标量参数，但模型字段是数组（dns_servers / params / bond members）——
// 取值追加、按值删除；执行期翻译与 SP 区分处理。
func SPA(placeholder, jsonKey, desc string) *Node {
	n := SP(placeholder, jsonKey, desc)
	n.ScalarIsArray = true
	return n
}

// IV 标记身份取值关键字（其值是父层具名数组的元素身份，见 IdentityValue）。
func IV(n *Node) *Node { n.IdentityValue = true; return n }

// PT 指定类型的实例参数节点（如 <vlan>、<seq>）。
func PT(placeholder, typ, desc string, children ...*Node) *Node {
	n := P(placeholder, desc, "", children...)
	n.ParamType = typ
	return n
}

// Op 标记需要 operator 及以上 class。
func Op(n *Node) *Node { n.MinClass = ClassOperator; return n }

// Su 标记需要 super-user class。
func Su(n *Node) *Node { n.MinClass = ClassSuperUser; return n }

// Opt 标记可选 token（[]）。
func Opt(n *Node) *Node { n.Optional = true; return n }

// finalize 回填父指针（树构建完成后由各 Root 调用一次）。
func finalize(n *Node, parent *Node) {
	n.parent = parent
	for _, c := range n.Children {
		finalize(c, n)
	}
}

// RequiredClass 返回该节点在树路径上实际所需的最低 class：
// 取从根到本节点的 MinClass 最大值（授权沿路径继承，如 request 内的
// 破坏性动作逐节点升为 S）。
func (n *Node) RequiredClass() Class {
	c := n.MinClass
	for p := n.parent; p != nil; p = p.parent {
		if p.MinClass > c {
			c = p.MinClass
		}
	}
	return c
}

// ---------- 查找与匹配 ----------

// childExact 按名精确查找子节点。
func (n *Node) childExact(name string) *Node {
	for _, c := range n.Children {
		if c.Kind != Value && c.Name == name {
			return c
		}
	}
	return nil
}

// childAbbrev 无歧义前缀匹配子关键字（FR-CLI-004）。ambiguous 表示多义，
// matches 列出全部命中候选（供歧义报错展示，§5.5）。
func (n *Node) childAbbrev(word string) (child *Node, matches []string, ambiguous bool) {
	for _, c := range n.Children {
		if c.Kind == Value || !strings.HasPrefix(c.Name, word) {
			continue
		}
		matches = append(matches, c.Name)
		if child != nil {
			return nil, matches, true
		}
		child = c
	}
	return child, matches, false
}

func (n *Node) firstParam() *Node {
	for _, c := range n.Children {
		if c.Kind == Param {
			return c
		}
	}
	return nil
}

func (n *Node) singleValue() *Node {
	for _, c := range n.Children {
		if c.Kind == Value {
			return c
		}
	}
	return nil
}

// consumesToken 报告节点消耗一个 token 后是否需要回退到父级继续匹配
// （值叶子与无子树参数：语句在该 token 后对父级关键字层开放，如
// `next-hop <ip> distance <n>` 的 distance 与 `cross-connect <a> <b>` 的 <b>）。
func (n *Node) consumesToken() bool {
	return n.Kind == Value || (n.Kind == Param && len(n.Children) == 0)
}

// Match 沿树匹配完整 token 序列，返回最终节点与已消耗 token 数。
// 支持无歧义缩写；Param 按任意非空 token 消耗（存在性校验属运行期）。
func Match(root *Node, tokens []string) (*Node, int, error) {
	n := root
	depth := 0
	for _, tk := range tokens {
		if n.consumesToken() {
			n = n.parent // 该 token 由父关键字层继续匹配
		}
		c := n.childExact(tk)
		if c == nil {
			var matches []string
			var ambiguous bool
			c, matches, ambiguous = n.childAbbrev(tk)
			if ambiguous {
				return n, depth, fmt.Errorf("%q 存在歧义匹配: %s（需更长前缀）", tk, strings.Join(matches, ", "))
			}
		}
		if c == nil {
			c = n.firstParam()
		}
		if c == nil {
			c = n.singleValue()
		}
		if c == nil {
			return n, depth, fmt.Errorf("未知命令: %q", tk)
		}
		n = c
		depth++
	}
	return n, depth, nil
}

// Canonicalize 将 token 序列按树规整为规范关键字名（FR-CLI-004：无歧义前缀
// 即可执行）。关键字按 childExact/childAbbrev 解析后替换为节点名；Param/Value
// 位置保留原 token（取值不做转换）。遍历规则与 Match 一致。
//
// 歧义前缀直接返回错误（列出候选，§5.5）；树未建模的 token 及之后内容原样
// 保留（执行器 self-check 后再报错），以兼容执行器支持而树未完整建模的语法
// （如 `show configuration compare rollback <n>`）。
func Canonicalize(root *Node, tokens []string) ([]string, error) {
	n := root
	out := make([]string, 0, len(tokens))
	for i, tk := range tokens {
		if n.consumesToken() {
			n = n.parent
		}
		c := n.childExact(tk)
		if c == nil {
			var matches []string
			var ambiguous bool
			c, matches, ambiguous = n.childAbbrev(tk)
			if ambiguous {
				return nil, fmt.Errorf("%q 存在歧义匹配: %s（需更长前缀）", tk, strings.Join(matches, ", "))
			}
		}
		switch {
		case c != nil:
			out = append(out, c.Name) // 关键字规范名
			n = c
		case n.firstParam() != nil:
			n = n.firstParam()
			out = append(out, tk) // 参数取值原样
		case n.singleValue() != nil:
			n = n.singleValue()
			out = append(out, tk) // 值叶子原样
		default:
			return append(out, tokens[i:]...), nil // 树未建模：其余原样
		}
	}
	return out, nil
}

// Find 按完整关键字路径查找节点（测试与 cli_bridge 使用）。
func Find(root *Node, names ...string) (*Node, error) {
	n := root
	for _, name := range names {
		c := n.childExact(name)
		if c == nil {
			return nil, fmt.Errorf("路径不存在: %s", strings.Join(names, " "))
		}
		n = c
	}
	return n, nil
}

// ---------- 补全（FR-CLI-001/002/003/007，§5 行为细则） ----------

// Candidate 一条补全候选。
type Candidate struct {
	Token string // 候选文本
	Desc  string // 描述（? 列表展示）
}

// DynamicValues 动态候选来源：按 kind 返回实时清单（CLI 向 nfvisd 查询）。
type DynamicValues func(kind string) []string

// Candidates 返回已完成 tokens 后、输入 partial 时的全部候选（已按前缀过滤）。
// 含：关键字（名称+描述）、参数占位符的动态值、取值枚举。
// 动态来源查询失败（dyn 为 nil 或返回空）时退化为占位符提示（§5.3）。
func Candidates(root *Node, tokens []string, partial string, dyn DynamicValues) []Candidate {
	n, _, err := Match(root, tokens)
	if err != nil {
		return nil
	}
	return candidatesAt(n, partial, dyn)
}

func candidatesAt(n *Node, partial string, dyn DynamicValues) []Candidate {
	var out []Candidate

	// 取值位置：枚举候选（§5.2 枚举型参数值可 Tab 补全）
	if n.Kind == Value || (n.Kind == Keyword && n.singleValue() != nil && len(n.Children) == 1) {
		vn := n
		if vn.Kind == Keyword {
			vn = vn.Children[0]
		}
		for _, e := range vn.Enum {
			if strings.HasPrefix(e, partial) {
				out = append(out, Candidate{Token: e, Desc: vn.Desc})
			}
		}
		return sortedCandidates(out)
	}

	for _, c := range n.Children {
		switch c.Kind {
		case Param:
			// 动态候选（查询失败退化为占位符提示）
			offered := false
			if dyn != nil && c.Dynamic != "" {
				for _, v := range dyn(c.Dynamic) {
					if strings.HasPrefix(v, partial) {
						out = append(out, Candidate{Token: v, Desc: c.Desc})
						offered = true
					}
				}
			}
			if !offered && strings.HasPrefix(c.Name, partial) {
				out = append(out, Candidate{Token: c.Name, Desc: c.Desc + "（" + c.ParamType + "）"})
			}
		case Value:
			// 由取值位置分支处理，不与关键字混列
		default:
			if strings.HasPrefix(c.Name, partial) {
				out = append(out, Candidate{Token: c.Name, Desc: c.Desc})
			}
		}
	}
	return sortedCandidates(out)
}

func sortedCandidates(cs []Candidate) []Candidate {
	sort.Slice(cs, func(i, j int) bool { return cs[i].Token < cs[j].Token })
	return cs
}

// ---------- 树校验（gen：schema 一致性） ----------

// ValidateTree 检查树的结构一致性：节点必有名称与描述、兄弟关键字不重名、
// Param/Value 节点必有类型或枚举/动态来源。root（Name 为空）豁免。
func ValidateTree(root *Node) []error {
	var errs []error
	var walk func(n *Node, path string)
	walk = func(n *Node, path string) {
		if n != root {
			if n.Name == "" {
				errs = append(errs, fmt.Errorf("%s: 节点缺少名称", path))
			}
			if n.Desc == "" {
				errs = append(errs, fmt.Errorf("%s: 节点 %q 缺少描述", path, n.Name))
			}
			if (n.Kind == Param || n.Kind == Value) && n.ParamType == "" && len(n.Enum) == 0 && n.Dynamic == "" {
				errs = append(errs, fmt.Errorf("%s: %s 节点 %q 缺少类型/枚举/动态来源", path, n.Kind, n.Name))
			}
		}
		seen := map[string]bool{}
		for _, c := range n.Children {
			if c.Kind != Value && seen[c.Name] {
				errs = append(errs, fmt.Errorf("%s: 子节点 %q 重复定义", path, c.Name))
			}
			seen[c.Name] = true
			walk(c, path+" "+c.Name)
		}
	}
	walk(root, "")
	return errs
}
