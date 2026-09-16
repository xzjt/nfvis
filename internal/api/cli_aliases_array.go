package api

// 数组型标量字段的语句别名（缺陷驱动，见 docs/reviews/2026-09-13.md 第六轮）：
//   system dns server <ip>              → system.dns_servers[]（此前写入被类型拒绝或静默丢弃）
//   system kernel params <param>       → system.kernel.params[]
//   bonds <name> members [<seq>] <if>  → bonds[].members[]（此前报"配置不完整，缺少取值"）
//
// 这些字段在模型里是 []string，而通用遍历按标量写入（首值能过、多值或类型不符时失败），
// 故用显式别名保证追加/按值删除语义正确。
//
// ⚠️ 别名 pattern 必须与命令树**同时**存在，否则是死规则：曾经这里有一条
// `system dns secondary *`，注释还写着「等价 set system dns secondary <ip>」——但树里没有
// `dns.secondary` 节点（只有 `dns.server.secondary`），于是那个写法实测是「% 无效命令」，
// 规则永远匹配不到（附录 A #91）。新增别名时请确认树里有对应的关键字路径。

import (
	"fmt"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

// statementAliasesArray：数组型标量字段的语句（追加 / 按值删除）。
var statementAliasesArray = []aliasRule{
	// system dns server <ip>（备用地址写法：`dns server <ip> secondary <ip>`，见命令树）
	{pattern: []string{"system", "dns", "server", "*"}, apply: dnsServerApply},
	// system kernel params <param>
	{pattern: []string{"system", "kernel", "params", "*"}, apply: kernelParamsApply},
	// bonds <name> members [<seq>] <ifname>
	{pattern: []string{"bonds", "*", "members", "*"}, apply: bondMembersApply},
	{pattern: []string{"bonds", "*", "members", "*", "*"}, apply: bondMembersApply},
}

func dnsServerApply(tree map[string]any, t []string, isSet bool) error {
	sys, err := ensureObjErr(tree, "system")
	if err != nil {
		return err
	}
	return appendOrRemove(sys, "dns_servers", t[3], isSet)
}

func kernelParamsApply(tree map[string]any, t []string, isSet bool) error {
	sys, err := ensureObjErr(tree, "system")
	if err != nil {
		return err
	}
	k, err := ensureObjErr(sys, "kernel")
	if err != nil {
		return err
	}
	return appendOrRemove(k, "params", t[3], isSet)
}

// bondMembersApply：bonds <name> members [<seq>] <ifname>。
// 有带序号形态时取末位 token 为接口名（序号仅作书写顺序，模型 members 为有序数组）。
func bondMembersApply(tree map[string]any, t []string, isSet bool) error {
	bond, err := ensureElemByFieldErr(tree, "bonds", "name", t[1])
	if err != nil {
		return err
	}
	ifname := t[len(t)-1]
	return appendOrRemove(bond, "members", ifname, isSet)
}

// appendOrRemove 字符串数组字段的追加/按值删除（保留其余项与顺序）。
func appendOrRemove(container map[string]any, key, val string, isSet bool) error {
	arr, _ := container[key].([]any)
	if isSet {
		for _, e := range arr {
			if s, ok := e.(string); ok && s == val {
				return nil // 幂等：已存在则视为无变化，由 Diff 兜底提示
			}
		}
		container[key] = append(arr, val)
		return nil
	}
	out := make([]any, 0, len(arr))
	hit := false
	for _, e := range arr {
		if s, ok := e.(string); ok && s == val {
			hit = true
			continue
		}
		out = append(out, e)
	}
	if !hit {
		return fmt.Errorf("无匹配配置: %s", val)
	}
	if len(out) == 0 {
		delete(container, key)
		return nil
	}
	container[key] = out
	return nil
}

// ensureObjErr 取（或建）map 子对象，并保证其为对象类型。
func ensureObjErr(m map[string]any, key string) (map[string]any, error) {
	if sub, ok := m[key].(map[string]any); ok {
		return sub, nil
	}
	if v, exists := m[key]; exists && v != nil {
		return nil, fmt.Errorf("%s 不是对象（配置冲突）", key)
	}
	sub := map[string]any{}
	m[key] = sub
	return sub, nil
}

// ensureElemByFieldErr 在具名数组中按身份字段取（或建）元素。
func ensureElemByFieldErr(tree map[string]any, arrKey, field, ident string) (map[string]any, error) {
	arr, _ := tree[arrKey].([]any)
	if em, _ := selectElement(arr, field, ident); em != nil {
		return em, nil
	}
	em := map[string]any{field: ident}
	tree[arrKey] = append(arr, em)
	return em, nil
}

// 编译期确认 model 包仍被使用（数组字段语义参考 model.Bond/SystemConfig）。
var _ = model.Bond{}
var _ = strings.TrimSpace
