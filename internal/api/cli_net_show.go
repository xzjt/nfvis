package api

// T0-1：`show acls [<name> [detail]]`、`show bonds [<name> [detail]]` 的 CLI 渲染。
// 读取 committed 配置构造 JunOS 风格配置树，x.structured 供 `| display json/xml`；
// ACL 逐规则命中为运行态（决策 #339，aclDetailView 注入）；bond 的 LACP actor/partner 运行态仍属后续。

import (
	"context"
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
		// 决策 #339：逐规则命中（运行态）。与 REST `GET /acls/{name}` 同一 ACLCountersRuntime、
		// 同一 aclDetailView，保证两面一致。取数失败仍渲染配置详情（配置读视图不因运行态失效而
		// 不可用），随后**如实**附一行原因——不静默省略、不把「取不到」当「零命中」。
		hits, herr := aclHitsFor(context.Background(), x.aclHits, name)
		view := aclDetailView(a, hits)
		x.structured = view
		out := RenderConfigJSON(view) + "\n"
		switch {
		case herr != nil:
			out += "命中计数不可用：" + herr.Error() + "\n"
		case hits != nil:
			// 总命中一行（决策 #339）：逐规则之和，便于一眼看整体。
			var total uint64
			for _, h := range hits {
				total += h
			}
			out += fmt.Sprintf("总命中：%d\n", total)
		}
		return out
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
