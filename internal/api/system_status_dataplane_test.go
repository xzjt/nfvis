package api

// R2-17：数据面（装配事实）进 REST 与 Web 的读视图。
//
// ① `GET /system/status` 增 `dataplane` 字段（来源=装配事实，与 `/vpp/status.dataplane` 同源）；
// ② Web 总览「数据面」卡按它渲染：kernel 下显示「Linux 内核网络（未使用 VPP）」，
//    不再把 VPP 整组读数渲染成「未连接」；字段级守护见下（契约里没有的字段一出现即红）。

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

func TestSystemStatusReportsDataplaneField(t *testing.T) {
	// 缺省装配（未注入 VppController）：按 committed 缺省报 vpp
	ts := newTestServerOpts(t, Options{})
	token := loginAdmin(t, ts)
	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/status", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /system/status: %d %s", status, body)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if got["dataplane"] != model.DataPlaneVPP {
		t.Fatalf("缺省装配应报 vpp，实得 %v（响应：%s）", got["dataplane"], body)
	}

	// 内核数据面装配：装配事实优先，如实报 kernel
	ts2 := newTestServerOpts(t, Options{VPP: fakeVppCtl{st: VppStatus{
		Mode:      model.DataPlaneKernel,
		LastError: "当前数据面为 Linux 内核网络，未使用 VPP",
	}}})
	token2 := loginAdmin(t, ts2)
	status, _, body = cfgRequest(t, http.MethodGet, ts2.URL+APIPrefix+"/system/status", token2, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /system/status（内核装配）: %d %s", status, body)
	}
	got = map[string]any{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if got["dataplane"] != model.DataPlaneKernel {
		t.Fatalf("内核数据面装配应报 kernel，实得 %v（响应：%s）", got["dataplane"], body)
	}
}

// jsFuncBody 提取 `function <name>(…) {` 到首个行首 `}` 的函数体（JS 顶层缩进为 0）。
func jsFuncBody(t *testing.T, src, sig string) string {
	t.Helper()
	start := strings.Index(src, sig)
	if start < 0 {
		t.Fatalf("ui/app.js 里找不到 %s（函数签名或排版变了？）", sig)
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("%s 函数体没有预期收尾（\\n}\\n）", sig)
	}
	return src[start : start+end]
}

// 数据面卡的字段级守护：卡片读的字段必须都在契约 VppStatus 里（字段写了、响应里没有
// 会让整行恒「—」——R110-1 的复刻），且必须真的按 `dataplane` 分流内核语义。
func TestUIDataplaneCardReadsAssemblyFact(t *testing.T) {
	src, err := os.ReadFile("ui/app.js")
	if err != nil {
		t.Fatalf("读取 ui/app.js: %v", err)
	}
	text := string(src)
	body := jsFuncBody(t, text, "function renderVPP(vpp) {")

	if !strings.Contains(body, "vpp.dataplane") {
		t.Fatalf("数据面卡必须读装配事实 vpp.dataplane（否则内核数据面下仍渲染 VPP 整组读数）")
	}
	for _, want := range []string{"Linux 内核网络（未使用 VPP）", "不适用（未使用 VPP）"} {
		if !strings.Contains(body, want) {
			t.Fatalf("数据面卡内核分支应含 %q", want)
		}
	}

	spec := loadEmbeddedSpec(t)
	props := componentProps(t, spec, "VppStatus")
	if !props["dataplane"] {
		t.Fatalf("契约 VppStatus 必须声明 dataplane（卡片按它分流）")
	}
	for _, f := range jsObjFields(body, "vpp") {
		if strings.HasPrefix(f, "_") {
			continue // `__err` 是客户端统一的「读取失败」约定，不属于契约字段
		}
		if !props[f] {
			t.Errorf("数据面卡消费了 vpp.%s，但契约 VppStatus schema 没有该属性（列会恒「—」）", f)
		}
	}

	// /system/status 的客户端分流字段也必须在契约里（Web 诊断页说明按它切换）。
	if !componentProps(t, spec, "SystemStatus")["dataplane"] {
		t.Fatalf("契约 SystemStatus 必须声明 dataplane（/system/status 客户端按它分流）")
	}
}

// 诊断页说明：按装配事实切换（内核数据面下 ping 走宿主网络栈），且页面有承载节点、
// 路由取数范围含 /system/status。
func TestUIDiagPlaneNoteFollowsDataplane(t *testing.T) {
	app, err := os.ReadFile("ui/app.js")
	if err != nil {
		t.Fatalf("读取 ui/app.js: %v", err)
	}
	body := jsFuncBody(t, string(app), "function renderDiagPlaneNote(st) {")
	if !strings.Contains(body, "st.dataplane") {
		t.Fatalf("诊断页说明必须按 st.dataplane 切换")
	}
	if !strings.Contains(body, "内核数据面下 ping 由宿主网络栈发出") {
		t.Fatalf("内核数据面下应说明 ping 走宿主网络栈")
	}
	if !strings.Contains(body, "diag-plane-note") {
		t.Fatalf("说明函数应写入 #diag-plane-note 节点")
	}
	// 诊断视图必须真调用它（只有函数没有接线 = 静态提示照旧）
	if !regexp.MustCompile(`render\(d\)\s*\{\s*renderDiagPlaneNote\(d\['/system/status'\]\)`).MatchString(string(app)) {
		t.Fatalf("诊断视图 render 应调用 renderDiagPlaneNote(d['/system/status'])")
	}

	html, err := os.ReadFile("ui/index.html")
	if err != nil {
		t.Fatalf("读取 ui/index.html: %v", err)
	}
	if !strings.Contains(string(html), `id="diag-plane-note"`) {
		t.Fatalf("index.html 的说明段应有 id=diag-plane-note")
	}

	routes, err := os.ReadFile("ui/routes.json")
	if err != nil {
		t.Fatalf("读取 ui/routes.json: %v", err)
	}
	m := regexp.MustCompile(`"view": "diagnostics"[\s\S]{0,200}?"endpoints": \[([^\]]*)\]`).FindStringSubmatch(string(routes))
	if m == nil {
		t.Fatalf("routes.json 里找不到诊断路由（排版变了？）")
	}
	if !strings.Contains(m[1], "/system/status") {
		t.Fatalf("诊断路由的取数范围应含 /system/status（数据面说明的来源）：%s", m[1])
	}
}
