package api

// BUG-07（真机验证）：`show vrfs <n> routes` 的 Distance 列逐行打「未下发」。
//
// 「未下发」紧挨着前缀列，读起来像「**这条路由**没有下发」——而真机现场该路由就在 FIB 里
// （`vppctl show ip fib table <id>` 查得到），只是 VPP 的路由 API 不接收 distance 这个
// 属性。运维据此怀疑路由丢失，属**假运维信号**。
//
// 修法（形状 b）：列头改成 `Distance(不可用)`、单元格用「—」，把不可用的事实放进表尾注，
// 措辞明确到「VPP 路由 API 不接收 distance 参数，该列恒不可用」，并点名「上面每条路由均已
// 下发」。`routeDistanceText` 仍是这一口径的**唯一实现**（列头/表尾注不各写一份）。

import (
	"os"
	"strings"
	"testing"
)

// cliNetRuntimeShowSrc 取被守护的 CLI 读视图源码（这一族守护只关心这一个文件）。
func cliNetRuntimeShowSrc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("cli_net_runtime_show.go")
	if err != nil {
		t.Fatalf("读 cli_net_runtime_show.go: %v", err)
	}
	return string(b)
}

// goFuncBodyFrom 提取 `func … {` 到首个行首 `}`（这些 Go 源文件的函数体缩进为 1 tab，
// 收尾 `}` 在行首——与 JS 守护 jsFuncBodyFrom 同法）。
func goFuncBodyFrom(t *testing.T, src, sig string) string {
	t.Helper()
	start := strings.Index(src, sig)
	if start < 0 {
		t.Fatalf("找不到 %q（函数签名或排版变了？本守护需同步）", sig)
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("%q 函数体没有预期收尾（\\n}\\n）", sig)
	}
	return src[start : start+end]
}

// ① 源码形状：单元格渲染只走 routeDistanceText，且该函数给「—」而不是「未下发」。
func TestVrfRoutesDistanceCellNeverSaysUndeployed(t *testing.T) {
	src := cliNetRuntimeShowSrc(t)

	render := goFuncBodyFrom(t, src, "func (x *cliExecutor) showVrfRoutes(name string) string {")
	// 列头把「不可用」前置（不改其余列名）。
	if !strings.Contains(render, `"Distance(不可用)"`) {
		t.Errorf("列表头应为 `Distance(不可用)`（把含义前置，避免逐行「未下发」被读成路由丢失）：\n%s", render)
	}
	for _, keep := range []string{`"Prefix"`, `"NextHop"`} {
		if !strings.Contains(render, keep) {
			t.Errorf("其余列名必须保持逐字不变（缺 %s）", keep)
		}
	}
	// 单元格值只由 routeDistanceText 决定（单源）。
	if !strings.Contains(render, "routeDistanceText(r.Distance)") {
		t.Errorf("单元格必须走 routeDistanceText（措辞单源，避免列头/单元格各写一份）：\n%s", render)
	}
	// 表尾注必须说清「API 不接收」与「该列恒不可用」，并排除「路由丢失」的误读。
	for _, want := range []string{"不接收 distance 参数", "该列恒不可用", "均已下发"} {
		if !strings.Contains(render, want) {
			t.Errorf("表尾注应含 %q（说明是**属性**不可用，不是路由缺失）：\n%s", want, render)
		}
	}

	helper := goFuncBodyFrom(t, src, "func routeDistanceText(d int) string {")
	if !strings.Contains(helper, `return "—"`) {
		t.Errorf("distance 不可得时应渲染「—」（无可展示的值），不得是「未下发」：\n%s", helper)
	}
	if strings.Contains(helper, "未下发") {
		t.Errorf("routeDistanceText 不得再返回「未下发」（紧挨前缀会被读成「这条路由没下发」）：\n%s", helper)
	}
}

// ② 真跑一次：前缀/下一跳逐字不变、逐行不再有「未下发」、注在。
func TestVrfRoutesRenderKeepsPrefixAndNextHop(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setNetRuntime(nil, &fakeL3Runtime{rows: []RouteRow{
		{Prefix: "10.88.0.0/24", NextHop: "10.99.0.254"},
		{Prefix: "10.88.1.0/24", NextHop: "10.99.0.254"},
	}}, nil, nil, nil)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set virtual-switches vs-l3 type l3",
		"commit",
		"exit",
	)
	out := x.Execute("admin", aaaClassSU, "ssh", "show vrfs vs-l3 routes").Output

	// 逐行：`<prefix>  <next-hop>  —`（前缀与下一跳两列逐字不变，只有最后一列变「—」）。
	for _, want := range []string{
		"10.88.0.0/24         10.99.0.254",
		"10.88.1.0/24         10.99.0.254",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("前缀/下一跳两列必须逐字不变（缺 %q）：\n%s", want, out)
		}
	}
	if strings.Contains(out, "未下发") {
		t.Errorf("输出里不得再出现「未下发」（逐行读成路由缺失）：\n%s", out)
	}
	if !strings.Contains(out, "Distance(不可用)") {
		t.Errorf("列头应带「不可用」标注：\n%s", out)
	}
}
