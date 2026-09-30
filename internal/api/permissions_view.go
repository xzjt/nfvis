package api

// 决策 #304：生效权限视图（CLI `show configuration permissions <class> [detail]` 与
// REST `GET /configuration/permissions?class=<name>`）。
//
// 判定逻辑的**单一事实源**在 `internal/aaa`（`ResolveClass` + `ClassDefView.Evaluate`，
// 与运行期 `Authorize` 同一套函数）。本文件只做「枚举命令树路径 → 调用 aaa 判定 → 组装视图」，
// CLI 三形态与 REST 响应共用同一 `buildPermissionView`，保证两侧是同一份事实。

import (
	"errors"
	"fmt"
	"strings"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/schema"
)

// permissionResolver 生效权限视图所需的最小能力（*aaa.Service 实现）。
// CLI 执行器与 REST handler 都经它取 class 定义——判定不在本层复制。
type permissionResolver interface {
	ResolveClass(name string) (aaa.ClassDefView, bool)
}

// errUnknownClass 未知 class（非预置、配置里也没有）的哨兵错误。
var errUnknownClass = errors.New("未知 class")

// permPathVerdict 一条命令路径的判定结果。
type permPathVerdict struct {
	Path     string
	Required schema.Class
	Allow    bool
	Reason   string
}

// permFamily 顶层命令族的允许路径汇总（默认视图按族分组）。
type permFamily struct {
	Name    string
	Allowed []string
	Denied  int
}

// permView 某 class 的生效权限视图（CLI 三形态与 REST 响应共用）。
type permView struct {
	Class      string
	Source     aaa.ClassSource
	RulesAllow []string
	RulesDeny  []string
	Paths      []permPathVerdict
	Allowed    int
	Denied     int
	Families   []permFamily
}

// buildPermissionView 组装生效权限视图：枚举命令树路径 + 逐路径调用 aaa 判定。
// class 未知返回 errUnknownClass（调用方据此明确报错，不拿「默认拒绝」冒充真实 class）。
func buildPermissionView(r permissionResolver, class string) (*permView, error) {
	def, ok := r.ResolveClass(class)
	if !ok {
		return nil, errUnknownClass
	}
	v := &permView{
		Class:      def.Name,
		Source:     def.Source,
		RulesAllow: append([]string{}, def.Allow...),
		RulesDeny:  append([]string{}, def.Deny...),
	}
	famIdx := map[string]int{}
	for _, cp := range schema.OperCommandPaths() {
		path := strings.Join(cp.Path, " ")
		allow, reason := def.Evaluate(cp.Required, cp.Path...)
		v.Paths = append(v.Paths, permPathVerdict{Path: path, Required: cp.Required, Allow: allow, Reason: reason})
		if allow {
			v.Allowed++
		} else {
			v.Denied++
		}
		fam := cp.Path[0]
		i, seen := famIdx[fam]
		if !seen {
			i = len(v.Families)
			famIdx[fam] = i
			v.Families = append(v.Families, permFamily{Name: fam})
		}
		if allow {
			v.Families[i].Allowed = append(v.Families[i].Allowed, path)
		} else {
			v.Families[i].Denied++
		}
	}
	return v, nil
}

// allowedPaths 视图里的允许路径（REST 响应与 Web 摘要用）。
func (v *permView) allowedPaths() []string {
	out := make([]string, 0, v.Allowed)
	for _, p := range v.Paths {
		if p.Allow {
			out = append(out, p.Path)
		}
	}
	return out
}

// sourceLabel 来源的中文标签（CLI 渲染用）。
func sourceLabel(src aaa.ClassSource) string {
	if src == aaa.ClassSourceCustom {
		return "自定义"
	}
	return "预置"
}

// renderPermViewDefault 默认形态：按顶层命令族列出**允许路径** + 末行汇总。
// 不自行分页——`show` 族可能很长，由既有管道族（match/except/count/last/display …）承接。
func renderPermViewDefault(v *permView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "class %s（%s）生效权限\n", v.Class, sourceLabel(v.Source))
	for _, f := range v.Families {
		if len(f.Allowed) == 0 {
			fmt.Fprintf(&b, "  %s（无允许路径；拒绝 %d 条）\n", f.Name, f.Denied)
			continue
		}
		fmt.Fprintf(&b, "  %s（允许 %d / 拒绝 %d）\n", f.Name, len(f.Allowed), f.Denied)
		for _, p := range f.Allowed {
			fmt.Fprintf(&b, "    %s\n", p)
		}
	}
	fmt.Fprintf(&b, "允许 %d / 拒绝 %d（共 %d 条命令路径）\n", v.Allowed, v.Denied, v.Allowed+v.Denied)
	return b.String()
}

// renderPermViewDetail `detail` 子形态：逐路径附判定依据列。
func renderPermViewDetail(v *permView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "class %s（%s）生效权限（逐路径）\n", v.Class, sourceLabel(v.Source))
	fam := ""
	for _, p := range v.Paths {
		f := p.Path
		if i := strings.IndexByte(f, ' '); i >= 0 {
			f = f[:i]
		}
		if f != fam {
			fam = f
			fmt.Fprintf(&b, "  %s\n", fam)
		}
		mark := "拒绝"
		if p.Allow {
			mark = "允许"
		}
		fmt.Fprintf(&b, "    [%s] %s（%s）\n", mark, p.Path, p.Reason)
	}
	fmt.Fprintf(&b, "允许 %d / 拒绝 %d（共 %d 条命令路径）\n", v.Allowed, v.Denied, v.Allowed+v.Denied)
	return b.String()
}

// permClassTree 把自定义 class 定义装成配置 JSON 形状（system.login.classes[]），
// 供 `| display set` 经**既有**反推机制（决策 #155）输出等价 set 语句——不另写渲染。
func permClassTree(def aaa.ClassDefView) map[string]any {
	elem := map[string]any{"name": def.Name}
	if len(def.Allow) > 0 {
		elem["allow"] = stringSliceToAny(def.Allow)
	}
	if len(def.Deny) > 0 {
		elem["deny"] = stringSliceToAny(def.Deny)
	}
	return map[string]any{
		"system": map[string]any{
			"login": map[string]any{"classes": []any{elem}},
		},
	}
}

func stringSliceToAny(xs []string) []any {
	out := make([]any, 0, len(xs))
	for _, x := range xs {
		out = append(out, x)
	}
	return out
}

// presetDisplaySetNote 预置 class 的 `| display set` 说明：预置档由等级判定、没有
// allow/deny 路径表，故不编造 set 语句，如实给出基等级说明（决策 #304）。
func presetDisplaySetNote(name string) string {
	return "# 预置 class " + name + " 由权限等级判定（show 族全可读；request 生命周期/镜像/接口需 operator 及以上；\n" +
		"# configure、clear、start shell 需 super-user），不是 allow/deny 路径表——没有等价的 set 语句。\n" +
		"# 逐路径的允许/拒绝见不带 | display set 的默认输出。\n"
}
