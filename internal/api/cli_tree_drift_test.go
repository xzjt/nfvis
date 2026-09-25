package api

// 命令树 ⇄ 执行器同源收口（第二轮，FR-CLI-001、FR-CLI-002、FR-CLI-004）。
//
// 起因：`internal/api/cli_table_cover_test.go` 的 `TestCommandTableCoversOperTree`
//（**命令树 → 命令全表**方向）首轮跑出三条漂移——命令树 advertise 了、执行器实际不认
//（或与校验层的强制要求相反）的形态。《命令全表》与契约写的是对的，**漂移在树这一侧**，
// 故一律改树，不把漂移抄进参考表：
//
//	① `show system configuration candidate`：契约 §1.1 的 `show system` 下只列
//	   `configuration sessions`（`candidate` 的**等价且唯一有实现**的写法是顶层
//	   `show configuration candidate`）——它是重复且无实现的形态，执行器对它只会回
//	   通用 fallback → 从树里**删除该节点**（保留 `sessions`）。
//	② `request images delete <name>`（位置参数）：执行器 `imagesDelete` 走 `kvArgs(rest, "name")`
//	   （键值形态），《命令全表》与用户手册写 `delete name <n>` → 树改成 `delete name <name>`。
//	③ `request images download … sha256`：树标了 `Opt`，而 `images.ValidateDownloadOptions`
//	   **强制**要求（FR-SEC-004 默认强制校验，缺省即拒）、《命令全表》写必填 → 去掉 `Opt`。
//
// 判据一律取**两侧同源**：树里声明的形态 = 执行器认的形态 = 校验层的强制要求，三方一致；
// 被删掉的形态在树里匹配不到，且在执行器侧**如实报错**（不得静默换一种语义作答）。

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/images"
	"github.com/xzjt/nfvis/internal/schema"
)

// ---------- ① show system configuration candidate（重复且无实现） ----------

// TestShowSystemConfigurationCandidateRemovedFromTree：该节点已从树里删除
// （`show system configuration` 下只剩 `sessions`），`?` 不产生歧义；
// 唯一有实现的等价写法 `show configuration candidate` 仍在树里、class 仍 R。
func TestShowSystemConfigurationCandidateRemovedFromTree(t *testing.T) {
	root := schema.OperRoot()

	// 被删的形态：树里再也解析不到（`?`/Tab 也就补不出来）
	if n, _, err := schema.Match(root, []string{"show", "system", "configuration", "candidate"}); err == nil {
		t.Errorf("`show system configuration candidate` 仍在树里解析到 %q——它是重复形态（等价写法是 `show configuration candidate`），"+
			"删除后不应复活；若确实要保留两种写法，执行器必须认（详见本文件头注释）", n.Name)
	}

	// 保留的形态：`show system configuration sessions` 仍在，class 仍 R
	sess, _, err := schema.Match(root, []string{"show", "system", "configuration", "sessions"})
	if err != nil {
		t.Fatalf("`show system configuration sessions` 不该被删: %v", err)
	}
	if got := sess.RequiredClass().String(); got != "R" {
		t.Errorf("`show system configuration sessions` 的 class 应为 R，实得 %s", got)
	}

	// 该层只有 sessions 一个孩子——`?` 这里不会再问「sessions 还是 candidate」
	cfg, err := schema.Find(root, "show", "system", "configuration")
	if err != nil {
		t.Fatalf("`show system configuration` 节点应在树里: %v", err)
	}
	var kids []string
	for _, c := range cfg.Children {
		kids = append(kids, c.Name)
	}
	if len(kids) != 1 || kids[0] != "sessions" {
		t.Errorf("`show system configuration` 的子节点应只剩 [sessions]，实得 %v", kids)
	}

	got := map[string]bool{}
	for _, cand := range schema.Candidates(root, []string{"show", "system", "configuration"}, "", nil) {
		got[cand.Token] = true
	}
	if !got["sessions"] || got["candidate"] || len(got) != 1 {
		t.Errorf("`show system configuration ?` 候选应只有 sessions，实得 %v", got)
	}

	// 等价写法仍被树收录（删掉的是重复形态，不是功能）
	eq, _, err := schema.Match(root, []string{"show", "configuration", "candidate"})
	if err != nil {
		t.Fatalf("`show configuration candidate` 是唯一有实现的写法，必须在树里: %v", err)
	}
	if got := eq.RequiredClass().String(); got != "R" {
		t.Errorf("`show configuration candidate` 的 class 应为 R，实得 %s", got)
	}
}

// TestShowSystemConfigurationCandidateNotSilentlyAnswered：被删的形态在**执行器**侧
// 必须如实报错（通用 fallback 亦然）——**不得**静默回配置正文。
// 这是「删树」这条处置的安全底线：形态不再被 advertise，但真敲了也不会被骗。
func TestShowSystemConfigurationCandidateNotSilentlyAnswered(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure", "set system hostname drift-node", "commit", "exit",
	)
	if body := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration").Output; !strings.Contains(body, "drift-node") {
		t.Fatalf("前置：committed 应含 drift-node: %q", body)
	}

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system configuration candidate").Output
	if !strings.HasPrefix(out, "%") {
		t.Fatalf("该形态已不在契约里，应如实报错而不是作答: %q", out)
	}
	if strings.Contains(out, "drift-node") {
		t.Fatalf("报错时不得回配置正文（静默误答）: %q", out)
	}

	// 等价写法仍被识别（不是「无效命令」）——证明删的是重复形态而非功能
	eq := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration candidate").Output
	if strings.Contains(eq, "无效命令") || strings.Contains(eq, "未支持") {
		t.Fatalf("`show configuration candidate` 是保留的实现，不该被判无效: %q", eq)
	}
}

// ---------- ② request images delete 的形态 ----------

// TestImagesDeleteTreeFormMatchesExecutor：《命令全表》/用户手册/执行器三方一致，
// 树里也必须是 `delete name <name>`（关键字 `name` + 取值），**不是**位置参数 `delete <name>`。
func TestImagesDeleteTreeFormMatchesExecutor(t *testing.T) {
	root := schema.OperRoot()

	del, err := schema.Find(root, "request", "images", "delete")
	if err != nil {
		t.Fatalf("`request images delete` 节点应在树里: %v", err)
	}
	var kids, params []string
	for _, c := range del.Children {
		if c.Kind == schema.Param {
			params = append(params, c.Name)
			continue
		}
		kids = append(kids, c.Name)
	}
	if len(params) != 0 {
		t.Errorf("`request images delete` 下不应直接挂参数节点 %v——执行器按 `delete name <n>` 的键值形态解析（kvArgs），"+
			"《命令全表》与用户手册也写 `delete name <n>`", params)
	}
	if len(kids) != 1 || kids[0] != "name" {
		t.Errorf("`request images delete` 下应只有一个关键字 name，实得 %v", kids)
	}

	// 新形态：全 token 匹配、末节点 class 仍 S（删除镜像是破坏性动作）
	n, depth, err := schema.Match(root, []string{"request", "images", "delete", "name", "img1"})
	if err != nil {
		t.Fatalf("`request images delete name img1` 在树里解析不到（`?`/Tab 补不出来）: %v", err)
	}
	if depth != 5 {
		t.Errorf("`request images delete name img1` 应消耗 5 个 token，实得 %d", depth)
	}
	if got := n.RequiredClass(); got != schema.ClassSuperUser {
		t.Errorf("删除镜像的 class 仍应为 S，实得 %v", got)
	}

	// 旧形态（树曾经 advertise 的 `delete <name>`）必须已不在树里——否则树与执行器又是两套
	if _, _, err := schema.Match(root, []string{"request", "images", "delete", "img1"}); err == nil {
		t.Errorf("`request images delete img1` 仍能在树里解析——树又漂回位置参数形态了（执行器不认这种写法）")
	}

	// `?`：先补关键字 name，取值位置仍给动态镜像候选（DynImages）
	byTok := map[string]string{}
	for _, cand := range schema.Candidates(root, []string{"request", "images", "delete"}, "", nil) {
		byTok[cand.Token] = cand.Desc
	}
	if _, ok := byTok["name"]; !ok || len(byTok) != 1 {
		t.Errorf("`request images delete ?` 候选应只有关键字 name，实得 %v", byTok)
	}
	byTok = map[string]string{}
	for _, cand := range schema.Candidates(root, []string{"request", "images", "delete", "name"}, "", func(kind string) []string {
		if kind != schema.DynImages {
			return nil
		}
		return []string{"img1", "img2"}
	}) {
		byTok[cand.Token] = cand.Desc
	}
	for _, want := range []string{"img1", "img2"} {
		if _, ok := byTok[want]; !ok {
			t.Errorf("`request images delete name ?` 应给镜像动态候选 %q，实得 %v", want, byTok)
		}
	}

	// 执行器：旧形态如实报语法错（并给出新形态），新形态进入删除确认
	x, _ := newCLIKit(t)
	x.setComputeRuntime(nil, nil, nil, nil, newCLIImagesStore(t))

	oldForm := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request images delete img1").Output
	if !strings.HasPrefix(oldForm, "%") {
		t.Fatalf("`request images delete img1`（树/契约都不认的位置参数形态）应如实报错: %q", oldForm)
	}
	// 报错必须是可操作的：入口的树预校验回「输入 ? 查看可用命令」，而 `?` 在该位置给的正是
	// `name`（上面已断言）——两者合起来把操作者引到新形态，故这里接受这两种提示之一。
	if !strings.Contains(oldForm, "?") && !strings.Contains(oldForm, "delete name") {
		t.Fatalf("报错应给可操作提示（`?` 或直接给出 delete name <n>）: %q", oldForm)
	}
	newForm := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request images delete name img1").Output
	if !strings.Contains(newForm, "[yes,no]") {
		t.Fatalf("`request images delete name img1` 应进入删除确认: %q", newForm)
	}
}

// ---------- ③ request images download 的 sha256 必填 ----------

// TestImagesDownloadRequiredFieldsMatchValidator：树里 `download` 各参数的**必填标记**
// 必须与 `images.ValidateDownloadOptions`（FR-SEC-004，受理前同步校验）一致——
// 逐个字段置空探测：校验层拒绝 ⇔ 树里该字段不是 `Opt`。两边任何一侧单独改动都会红。
func TestImagesDownloadRequiredFieldsMatchValidator(t *testing.T) {
	dl, err := schema.Find(schema.OperRoot(), "request", "images", "download")
	if err != nil {
		t.Fatalf("`request images download` 节点应在树里: %v", err)
	}

	// 全字段取值：sha256 用合法形状（64 位十六进制），其余非空
	full := map[string]string{
		"name":   "img1",
		"type":   images.TypeVM,
		"url":    "https://example.invalid/a.img",
		"sha256": strings.Repeat("a", 64),
	}
	opts := func(m map[string]string) images.DownloadOptions {
		return images.DownloadOptions{Name: m["name"], Type: m["type"], URL: m["url"], SHA256: m["sha256"]}
	}
	if err := images.ValidateDownloadOptions(opts(full)); err != nil {
		t.Fatalf("前置：全字段应通过校验，实得 %v", err)
	}

	for _, c := range dl.Children {
		if c.Kind != schema.Keyword {
			t.Fatalf("`request images download` 下应是 `关键字 <取值>` 的参数组，实得非关键字节点 %q", c.Name)
		}
		if _, known := full[c.Name]; !known {
			t.Fatalf("树里出现未登记的参数 %q——新增参数请补本用例的取值表（否则两边的必填性无人核对）", c.Name)
		}
		blank := map[string]string{}
		for k, v := range full {
			blank[k] = v
		}
		blank[c.Name] = ""
		rejected := images.ValidateDownloadOptions(opts(blank)) != nil
		if rejected == c.Optional {
			t.Errorf("`request images download … %s`：树里标为%s，而校验层%s——树与校验必须同源"+
				"（缺 sha256 即拒是默认强制校验，不给操作者留「照树敲却被拒」的坑）",
				c.Name, optLabel(c.Optional), rejectLabel(rejected))
		}
	}
	// 回归保护：sha256 这一条是本次收口的对象，单独点名
	sha, err := schema.Find(schema.OperRoot(), "request", "images", "download", "sha256")
	if err != nil {
		t.Fatalf("`request images download … sha256` 节点应在树里: %v", err)
	}
	if sha.Optional {
		t.Errorf("sha256 在树里仍是可选（Opt）——校验层强制要求，契约与《命令全表》都写必填")
	}

	// 执行器：缺 sha256 的写法如实报错并指出必填（与树/校验同源）
	x, _ := newCLIKit(t)
	x.setComputeRuntime(nil, nil, nil, nil, newCLIImagesStore(t))
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh",
		"request images download name img1 type vm-image url https://example.invalid/a.img").Output
	if !strings.HasPrefix(out, "%") || !strings.Contains(out, "sha256") {
		t.Fatalf("缺 sha256 应如实报错并指出该参数必填: %q", out)
	}
}

func optLabel(optional bool) string {
	if optional {
		return "可选（Opt）"
	}
	return "必填"
}

func rejectLabel(rejected bool) string {
	if rejected {
		return "拒绝（视为必填）"
	}
	return "接受（视为可选）"
}
