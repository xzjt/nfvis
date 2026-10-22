package api

// BUG-12（真机验证）：部署向导第 ③ 步「网络映射」加一块网卡后**不点名字输入框**，
// 写进候选的是 `{"name": "", "type": "memif", "virtual_switch": "vs-lan"}`——界面却报
// 「部署候选已写入」，服务端预检随后判 `vNIC名称缺失`，这份候选**永远提交不了**。
//
// 根因三层，缺一层就会复发：
//  ① 新行的输入框只有 placeholder、没有 value（placeholder 不是值，不点就留空）；
//  ② 本地预校验只看名称/镜像/vCPU/内存，**不看网卡名**，于是「空名」一路走到写候选；
//  ③ 全靠服务端兜底——而那已经太晚（候选已写入、提示已报成功）。
//
// 这是静态守护：仓库没有 JS 测试运行器，故与既有 UI 守护同法——解析前端源码文本、断言
// 修好后的性质（见 ui_tabs_test.go / ui_qos_card_fields_test.go 的同款做法）。

import (
	"regexp"
	"strings"
	"testing"
)

// deployJS 取部署向导的源码（这一族守护只关心这一个文件）。
func deployJS(t *testing.T) string {
	t.Helper()
	return uiJSFile(t, "ui/js/views/deploy.js")
}

// jsFuncBodyFrom 提取 `function <sig>…` 到首个行首 `}` 的函数体（deploy.js 顶层缩进为 0，
// 与 ui_tabs/system_status_dataplane 等既有守护同一提取法）。
func jsFuncBodyFrom(t *testing.T, src, sig string) string {
	t.Helper()
	start := strings.Index(src, sig)
	if start < 0 {
		t.Fatalf("deploy.js 里找不到 %q（函数签名或排版变了？本守护需同步）", sig)
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("%q 函数体没有预期收尾（\\n}\\n）", sig)
	}
	return src[start : start+end]
}

// ① 新网卡行必须带**值**，不能只有 placeholder。
//
// 断言点：创建网卡输入框的那一行同时出现 placeholder 与 value，且 value 是「算出来的」
// （不是写死的空串）——写死空串等价于没修。
func TestDeployNicRowHasDefaultValue(t *testing.T) {
	src := deployJS(t)
	body := jsFuncBodyFrom(t, src, "function depAddNicRow() {")

	re := regexp.MustCompile(`el\('input',\s*\{[^}]*\}\)`)
	m := re.FindString(body)
	if m == "" {
		t.Fatalf("depAddNicRow 里没找到网卡输入框的创建语句（el('input', {…})）——排版变了？本守护需同步")
	}
	if !strings.Contains(m, "placeholder: 'eth0'") {
		t.Errorf("网卡输入框应保留 placeholder: 'eth0'（提示写法），实际：%s", m)
	}
	vm := regexp.MustCompile(`value:\s*([^,}]+)`).FindStringSubmatch(m)
	if vm == nil {
		t.Fatalf("网卡输入框**没有 value**——placeholder 不是值，用户不点就留空，写进候选就是空 vNIC 名（BUG-12 本身）。实际：%s", m)
	}
	if strings.TrimSpace(strings.Trim(vm[1], `'"`)) == "" {
		t.Fatalf("网卡输入框的 value 是空串（等价于没修）。实际：%s", vm[1])
	}
	// 缺省名必须按「已占用」算出来（同表内唯一），不能恒为同一个常量。
	if !strings.Contains(vm[1], "depNextNicName()") {
		t.Errorf("网卡缺省值应取 depNextNicName()（按同表已占用名递增，保证行内唯一），实际：%s", vm[1])
	}
	// 缺省名生成器本身：取未占用的 eth<N>。
	gen := jsFuncBodyFrom(t, src, "function depNextNicName() {")
	for _, want := range []string{"dep-nic-table", "'eth' + i", "used.has(name)"} {
		if !strings.Contains(gen, want) {
			t.Errorf("depNextNicName 应据当前表格已占用名挑 `eth<N>`（缺 %q）：\n%s", want, gen)
		}
	}
}

// ② 本地预校验必须拦空/空白/重名/非法形状的网卡名，并在 depSubmit 里生效。
func TestDeployPrecheckRejectsEmptyAndDuplicateNicNames(t *testing.T) {
	src := deployJS(t)
	body := jsFuncBodyFrom(t, src, "function depPrecheck(body) {")

	// 预校验必须把网卡名检查接进来（此前完全不看网卡名，空名一路写到候选）。
	if !strings.Contains(body, "depNicNameIssues()") {
		t.Fatalf("depPrecheck 必须调用 depNicNameIssues()（此前完全不看网卡名，空名一路写到候选）：\n%s", body)
	}

	// 网卡名检查本身。
	nic := jsFuncBodyFrom(t, src, "function depNicNameIssues() {")
	// 查的是表格里的实际行：depNicRows 会把「名字空且没挂交换机」的行丢掉，那样一块网卡
	// 就无声消失了——操作者明明加过它。
	if !strings.Contains(nic, "querySelectorAll('tbody tr')") {
		t.Errorf("网卡名检查必须遍历表格实际行（dep-nic-table 的 tbody tr），否则空名行会被 depNicRows 静默丢掉：\n%s", nic)
	}
	for _, want := range []string{
		"'的名称必填",                              // 空名
		"'重名",                                 // 同对象内重名
		`/^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/`, // 与服务端 nameRe 同形
		".value.trim()",                       // 纯空白视同未填
	} {
		if !strings.Contains(nic, want) {
			t.Errorf("网卡名校验应含 %q（缺则那一类坏输入会走到服务端）：\n%s", want, nic)
		}
	}
	// 报错文案点名是第几块网卡（操作者要能定位到行）。
	if !strings.Contains(nic, "'第 ' + (i + 1) + ' 块网卡'") {
		t.Errorf("网卡名报错应点名「第 N 块网卡」，否则操作者不知道改哪一行：\n%s", nic)
	}

	// depSubmit 必须**先**跑预校验并中止（有错就不写候选）。
	sub := jsFuncBodyFrom(t, src, "async function depSubmit() {")
	pre := strings.Index(sub, "depPrecheck(body)")
	post := strings.Index(sub, "await api(")
	if pre < 0 {
		t.Fatalf("depSubmit 必须在写候选前调用 depPrecheck：\n%s", sub)
	}
	if post < 0 {
		t.Fatalf("depSubmit 里找不到写候选的 api( 调用（本守护需同步）：\n%s", sub)
	}
	if pre > post {
		t.Fatalf("depPrecheck 必须在写候选（await api）之前执行，否则坏输入照样写入候选")
	}
	if !strings.Contains(sub[pre:post], "if (errs.length)") || !strings.Contains(sub[pre:post], "return;") {
		t.Fatalf("预校验有错时必须中止提交（if (errs.length) { … return; }）：\n%s", sub[pre:post])
	}
}

// ③ 步骤条必须**真的绑定点击**（BUG-13：此前这些按钮没有任何处理器，点了只动高亮、
// 主区不动），且跳转受「已走到过的最远一步」约束（不能跳过必填步）。
func TestDeployStepButtonsAreBoundAndGuarded(t *testing.T) {
	src := deployJS(t)

	// 绑定：dep-steps 下的按钮逐个 addEventListener('click'…)。
	bindRe := regexp.MustCompile(`for \(const b of \$\('dep-steps'\)\.querySelectorAll\('button'\)\) \{\s*\n\s*b\.addEventListener\('click'`)
	if !bindRe.MatchString(src) {
		t.Fatalf("deploy.js 必须为 #dep-steps 的按钮绑定 click（此前完全没有处理器：点了只改 opacity，主区不动）")
	}
	if !strings.Contains(src, "depStepClick(Number(b.dataset.step))") {
		t.Errorf("按钮点击必须走 depStepClick(dataset.step)（同一套跳转判据），实际未找到")
	}

	// 上限守卫：depMaxStep 存在、由 depShow 推进、且 depStepClick 拒绝超出它的跳转。
	if !regexp.MustCompile(`let depMaxStep = 1`).MatchString(src) {
		t.Errorf("必须有 `let depMaxStep = 1`（已走到过的最远步骤）")
	}
	show := jsFuncBodyFrom(t, src, "function depShow(step) {")
	if !strings.Contains(show, "depMaxStep") {
		t.Errorf("depShow 必须推进 depMaxStep（否则走完第 ④ 步后回不去）：\n%s", show)
	}
	click := jsFuncBodyFrom(t, src, "function depStepClick(step) {")
	if !strings.Contains(click, "step <= depMaxStep") {
		t.Fatalf("depStepClick 必须拒绝超过 depMaxStep 的跳转（否则能跳过必填步）：\n%s", click)
	}
	if !strings.Contains(click, "depRequireKind()") {
		t.Errorf("depStepClick 必须复用 depRequireKind（与「下一步」同一套守卫）：\n%s", click)
	}
	// 重新进入向导＝新表单：上限要归零，否则上一次的「已到第 4 步」会泄漏到下一次。
	// render 是对象里的缩进方法（收尾是 `    },`），故按方法起点到 `async render(d) {` 之后的
	// 首个 `\n    },\n` 取体，而不是顶层函数的 `\n}\n`。
	renderStart := strings.Index(src, "async render(d) {")
	if renderStart < 0 {
		t.Fatalf("deploy.js 里找不到 async render(d) {（本守护需同步）")
	}
	renderEnd := strings.Index(src[renderStart:], "\n    },\n")
	if renderEnd < 0 {
		t.Fatalf("render 方法体没有预期收尾（\\n    },\\n）")
	}
	render := src[renderStart : renderStart+renderEnd]
	if !strings.Contains(render, "depMaxStep = 1") {
		t.Errorf("render 必须重置 depMaxStep（否则重进向导后步骤条仍可跳到上次的最远步）：\n%s", render)
	}
}

// ④ index.html 的步骤条与 JS 用同一组 data-step（HTML 改了/加了步而 JS 的 1..4 循环没跟，
// 那一格就永远是隐藏的——同 ui_tabs 的「两处清单」漂移族）。
func TestDeployStepsHTMLMatchesJS(t *testing.T) {
	html := uiFile(t, "index.html")
	src := deployJS(t)

	barRe := regexp.MustCompile(`(?s)id="dep-steps".*?</div>`)
	bar := barRe.FindString(html)
	if bar == "" {
		t.Fatal("index.html 里找不到 #dep-steps（步骤条被改名/删掉了？本守护需同步）")
	}
	var steps []string
	for _, m := range regexp.MustCompile(`data-step="(\d+)"`).FindAllStringSubmatch(bar, -1) {
		steps = append(steps, m[1])
	}
	if len(steps) == 0 {
		t.Fatalf("步骤条解析到空的 data-step 清单——解析器失效：%s", bar)
	}
	if strings.Join(steps, ",") != "1,2,3,4" {
		t.Fatalf("步骤条的 data-step 应为 1,2,3,4（先后顺序即界面顺序），实际 %v", steps)
	}
	// JS 的逐步面板循环必须覆盖同样的 1..4。
	if !strings.Contains(src, "for (let i = 1; i <= 4; i++)") {
		t.Errorf("deploy.js 的逐步面板循环应与步骤条一致（for (let i = 1; i <= 4; i++)）")
	}
	for _, n := range steps {
		if !strings.Contains(html, `id="dep-step-`+n+`"`) {
			t.Errorf("index.html 缺第 %s 步的面板（id=dep-step-%s）——点该步会切到空白", n, n)
		}
	}
}

// ⑤ 诊断页 VRF 输入框的说明与**新的解析口径**一致：填配置对象名即可（L2 交换机的网关域
// 由服务端解析成 vr-<名>），不再把「vr-…」写成一律不能填的东西。
func TestDiagVrfPlaceholderMatchesResolution(t *testing.T) {
	html := uiFile(t, "index.html")
	re := regexp.MustCompile(`<input id="diag-vrf"[^>]*placeholder="([^"]*)"`)
	m := re.FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("index.html 里找不到 #diag-vrf 的 placeholder（本守护需同步）")
	}
	ph := m[1]
	if !strings.Contains(ph, "配置对象名") {
		t.Errorf("placeholder 应说明填**配置对象名**（如 vs-lan），实际：%q", ph)
	}
	if !strings.Contains(ph, "自动解析") || !strings.Contains(ph, "vr-") {
		t.Errorf("placeholder 应说明 L2 交换机的网关域会被自动解析为 vr-<名>（否则操作者不知道两种写法都通），实际：%q", ph)
	}
	if strings.Contains(ph, "不是内核设备名") {
		t.Errorf("placeholder 不应再把 vr-… 说成不能填（服务端现在接受并解析它），实际：%q", ph)
	}
}
