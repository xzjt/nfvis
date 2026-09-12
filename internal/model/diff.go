package model

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Statement 扁平化后的一条配置语句：Path 为从根到叶的路径 token，
// Value 为标量值。同名路径可重复出现（如多条 ssh-key），diff 按多重集比较。
type Statement struct {
	Path  []string
	Value string
}

// identityKeys 具名数组元素的键优先级：对象数组按其身份字段展开为路径，
// 与 CLI 命令树中「实例名入路径」的语义一致（见 AGENTS.md 常见错误第 4 条）。
var identityKeys = []string{"name", "interface", "prefix", "seq", "node", "server", "page_size"}

// Flatten 将整份配置扁平化为语句列表。键序与数组序均确定，输出可作 diff 与审计的稳定输入。
func Flatten(c Config) []Statement {
	b, err := json.Marshal(&c)
	if err != nil {
		return nil // Config 结构恒可序列化
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		return nil
	}
	var out []Statement
	walkMap(root, nil, "", &out)
	return out
}

// walkMap 遍历对象；skip 为需跳过的身份字段名（已作为路径 token，不重复作为叶子）。
// 键名转换为 CLI 风格（snake → 连字符），使 diff 输出与命令树/ show configuration 一致。
func walkMap(m map[string]any, path []string, skip string, out *[]Statement) {
	keys := make([]string, 0, len(m))
	for k := range m {
		if k == skip {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		walkValue(m[k], appendToken(path, strings.ReplaceAll(k, "_", "-")), out)
	}
}

// appendToken 复制出新切片再追加，避免多个语句共享底层数组被后续写入覆盖。
func appendToken(path []string, tok string) []string {
	p := make([]string, len(path)+1)
	copy(p, path)
	p[len(path)] = tok
	return p
}

func walkValue(v any, path []string, out *[]Statement) {
	switch x := v.(type) {
	case map[string]any:
		field, _ := identityOf(x)
		walkMap(x, path, field, out)
	case []any:
		// 标量数组：每元素一条语句，路径止于字段名（JunOS 的可重复语句）。
		allScalar := true
		for _, e := range x {
			switch e.(type) {
			case map[string]any, []any:
				allScalar = false
			}
		}
		if allScalar {
			for _, e := range x {
				*out = append(*out, Statement{Path: path, Value: scalarString(e)})
			}
			return
		}
		for i, e := range x {
			m, ok := e.(map[string]any)
			if !ok {
				walkValue(e, appendToken(path, strconv.Itoa(i)), out)
				continue
			}
			if field, id := identityOf(m); id != "" {
				walkMap(m, appendToken(path, id), field, out)
			} else {
				walkMap(m, appendToken(path, strconv.Itoa(i)), "", out)
			}
		}
	default:
		*out = append(*out, Statement{Path: path, Value: scalarString(v)})
	}
}

// identityOf 返回元素的身份字段名与其值（如 "name"/"vs-app"）；无身份字段时值为 ""（按索引展开）。
func identityOf(m map[string]any) (string, string) {
	for _, k := range identityKeys {
		if v, ok := m[k]; ok {
			switch v.(type) {
			case string, float64, bool:
				return k, scalarString(v)
			}
		}
	}
	return "", ""
}

func scalarString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", x)
	}
}

// Diff 输出 old ⇄ new 的 JunOS 风格差异（FR-CFG-006）：
// 以 [edit <路径>] 分组，删除行前缀 -，新增行前缀 +，无差异返回空串。
func Diff(old, new Config) string {
	oldIdx := indexStatements(Flatten(old))
	newIdx := indexStatements(Flatten(new))

	type line struct{ text string }
	paths := make([]string, 0, len(oldIdx)+len(newIdx))
	seen := map[string]bool{}
	for p := range oldIdx {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for p := range newIdx {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	var b strings.Builder
	lastParent := ""
	changed := false
	for _, p := range paths {
		tokens := strings.Split(p, " ")
		parent := strings.Join(tokens[:len(tokens)-1], " ")
		leaf := tokens[len(tokens)-1]

		removed, added := multisetDiff(oldIdx[p], newIdx[p])
		if len(removed) == 0 && len(added) == 0 {
			continue
		}
		if parent != lastParent {
			if parent == "" {
				fmt.Fprintf(&b, "[edit]\n")
			} else {
				fmt.Fprintf(&b, "[edit %s]\n", parent)
			}
			lastParent = parent
		}
		for _, v := range removed {
			fmt.Fprintf(&b, "-   %s %s;\n", leaf, v)
			changed = true
		}
		for _, v := range added {
			fmt.Fprintf(&b, "+   %s %s;\n", leaf, v)
			changed = true
		}
	}
	if !changed {
		return ""
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func indexStatements(stmts []Statement) map[string][]string {
	idx := make(map[string][]string)
	for _, s := range stmts {
		idx[strings.Join(s.Path, " ")] = append(idx[strings.Join(s.Path, " ")], s.Value)
	}
	return idx
}

// multisetDiff 返回 (old 有而 new 无, new 有而 old 无)，各自升序。
func multisetDiff(oldVals, newVals []string) (removed, added []string) {
	count := map[string]int{}
	for _, v := range oldVals {
		count[v]++
	}
	for _, v := range newVals {
		count[v]--
	}
	for v, n := range count {
		for i := 0; i < n; i++ {
			removed = append(removed, v)
		}
		for i := 0; i < -n; i++ {
			added = append(added, v)
		}
	}
	sort.Strings(removed)
	sort.Strings(added)
	return removed, added
}
