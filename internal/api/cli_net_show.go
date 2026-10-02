package api

// T0-1：`show acls [<name> [detail]]`、`show bonds [<name> [detail]]` 的 CLI 渲染。
// 读取 committed 配置构造 JunOS 风格配置树，x.structured 供 `| display json/xml`；
// 运行态字段（ACL 命中计数、LACP actor/partner）属 M3，此处先呈现配置视图。

import (
	"encoding/json"
	"fmt"
)

// anyToTree 任意模型值 → JSON 树（与 toJSONTree 同法，供单资源详情复用）。
func anyToTree(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

func (x *cliExecutor) execShowAcls(args []string) string {
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	if len(args) == 0 { // 列表
		items := make([]any, 0, len(cfg.Acls))
		for _, a := range cfg.Acls {
			items = append(items, anyToTree(a))
		}
		if len(items) == 0 {
			return "（无 ACL）\n"
		}
		tree := map[string]any{"acls": items}
		x.structured = tree
		return RenderConfigJSON(tree) + "\n"
	}
	name := args[0]
	for _, a := range cfg.Acls {
		if a.Name != name {
			continue
		}
		m, _ := anyToTree(a).(map[string]any)
		x.structured = m
		return RenderConfigJSON(m) + "\n"
	}
	return fmt.Sprintf("%% ACL %s 不存在\n", name)
}

func (x *cliExecutor) execShowBonds(args []string) string {
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	if len(args) == 0 { // 列表
		items := make([]any, 0, len(cfg.Bonds))
		for _, b := range cfg.Bonds {
			items = append(items, anyToTree(b))
		}
		if len(items) == 0 {
			return "（无 bond）\n"
		}
		tree := map[string]any{"bonds": items}
		x.structured = tree
		return RenderConfigJSON(tree) + "\n"
	}
	name := args[0]
	for _, b := range cfg.Bonds {
		if b.Name != name {
			continue
		}
		m, _ := anyToTree(b).(map[string]any)
		x.structured = m
		return RenderConfigJSON(m) + "\n"
	}
	return fmt.Sprintf("%% bond %s 不存在\n", name)
}
