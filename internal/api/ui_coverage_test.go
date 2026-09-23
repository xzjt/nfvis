package api

// Web 控制台的**界面覆盖**守护（与 CLI⇄REST 的 `cli_rest_coverage_test.go` 同一套纪律）。
//
// 背景：`cli_rest_coverage_test.go` 盯的是「CLI 命令有没有 REST 端点」；而**界面用不用这些端点**
// 是另一层——两者混为一谈会得出「REST 覆盖高就万事大吉」的错误结论。本文件把界面这一层也机器化：
//
//   - **已接**：由前端源码**机器提取**（`api('/x')` 与 `fetch(API + '/x')` 的字面量路径，
//     以 `/` 结尾的按前缀匹配），不是人工维护的表——"界面真的调了它"由源码保证；
//   - **未接**：契约里存在、界面**有意**不接的路径，逐条写理由（`uiNotWired`）；
//   - 断言：① 前端调的路径必须在契约里（映射过期即红）；② 契约里每条路径要么已接、
//     要么在 `uiNotWired` 里写明理由——**新增端点忘记归类即红**，界面缺口数因此始终可见。

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// uiNotWired 契约有、界面**有意**不接的路径 → 理由（新增端点须在此归类或接入界面）。
var uiNotWired = map[string]string{
	// —— 已登记：后续增量的界面工作（**这一段就是界面缺口清单**）——
	"/interfaces/{name}/dpdk":            "DPDK 绑定/解绑涉及管理口红线，界面暂不提供（CLI 有守卫）",
	"/interfaces/{name}/sriov":           "SR-IOV 无硬件环境验证，界面暂不提供",
	"/virtual-switches/{name}/mac-table": "MAC 表数据量大需分页，后续增量",
	"/virtual-switches/{name}/ports":     "成员端口全量替换属配置编辑，走「配置」卡",
	"/system/hardware":                   "硬件健康明细——后续增量（阈值告警已在告警卡体现）",
	"/system/health/thresholds":          "健康阈值设置——后续增量",
	"/configuration/rollback/{n}":        "回滚到历史快照——需先看差异再确认，后续增量",
	"/protocols/lldp":                    "LLDP 开关状态——邻居表已接；开关属配置编辑（走「配置」卡）",

	// —— 高风险 / 需要文件选择：界面有意不提供（CLI 有二次确认与守卫）——
	"/system/restore":                            "恢复配置属高风险，界面暂不提供",
	"/system:zeroize":                            "恢复出厂（破坏性极强），界面暂不提供",
	"/system/software":                           "软件升级涉及重启与回退，界面暂不提供",
	"/system/software:rollback":                  "同上",
	"/system/tls":                                "证书上传需文件选择与 PEM 校验（重签已提供）",
	"/system/login-users":                        "用户管理涉及口令策略，界面暂不提供",
	"/system/login-users/{name}":                 "同上",
	"/system/login-users/{name}:change-password": "同上",
	"/system/kernel:apply":                       "内核基线应用需重启生效，界面暂不提供",
	"/system/kernel:rollback":                    "同上",
	"/system/ntp:sync":                           "NTP 立即同步——界面暂无入口",

	// —— 配置类：走「配置」卡的候选 → 提交流程（不单列界面入口）——
	"/system":     "系统配置段——走「配置」卡（candidate → 提交）",
	"/vpp/config": "VPP 配置段——同上",

	// —— by design：非界面读物 ——
	"/metrics":                       "Prometheus 文本格式，界面改读 /system/status 的同源字段",
	"/openapi.json":                  "契约自查用，界面不消费",
	"/ui":                            "302 到 /ui/，由浏览器自行跟随",
	"/ui/":                           "页面本体",
	"/cli/execute":                   "x-internal：CLI 执行通道，界面只走类型化端点",
	"/cli/candidates":                "x-internal：补全候选，界面用表单替代",
	"/system/configuration/sessions": "持锁会话查询——排障用，界面暂无入口",
}

// uiUsedPaths 从**前端源码**提取路径字面量：api('/x')、fetch(API + '/x')。
// 以 `/` 结尾的字面量（如 `/interfaces/` + name）按前缀匹配使用。
func uiUsedPaths(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile("ui/app.js")
	if err != nil {
		t.Fatalf("读取 ui/app.js: %v", err)
	}
	src := string(data)
	seen := map[string]bool{}
	// 并入**路由表**（ui/routes.json）的 endpoints：控制台改成"资源域一级 + 对象详情二级"后，
	// 页面取数由路由表声明（如 /system/status 只在总览页取），字面量只剩动作类端点。
	// 真 JSON 解析（不用正则）——与 ui_routes_test.go 同一份真源。
	rb, err := os.ReadFile("ui/routes.json")
	if err != nil {
		t.Fatalf("读取 ui/routes.json: %v", err)
	}
	var doc struct {
		Routes []struct {
			Endpoints []string `json:"endpoints"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(rb, &doc); err != nil {
		t.Fatalf("ui/routes.json 不是合法 JSON: %v", err)
	}
	for _, r := range doc.Routes {
		for _, p := range r.Endpoints {
			if i := strings.IndexByte(p, '?'); i >= 0 {
				p = p[:i]
			}
			seen[p] = true
		}
	}
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`api\('(/[^']*)'`),
		regexp.MustCompile(`fetch\(API \+ '(/[^']*)'`),
	} {
		for _, m := range re.FindAllStringSubmatch(src, -1) {
			p := m[1]
			if i := strings.IndexByte(p, '?'); i >= 0 {
				p = p[:i] // 去掉查询串（/audit-logs?limit=50 → /audit-logs）
			}
			seen[p] = true
		}
	}
	if len(seen) < 15 {
		t.Fatalf("从 ui/app.js 只提取到 %d 条路径，提取规则可能失效（前端调用形式变了？）", len(seen))
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// uiCovers 判断某契约路径是否被界面使用。
//
// 规则：① 字面量完全相等；② 以 `/` 结尾的字面量（动态拼接，如 `api('/interfaces/' + name)`）
// **只覆盖"恰好多一段"的路径**（`/interfaces/{name}`）——不覆盖更深的子路径
// （`/interfaces/{name}/dpdk`、`/virtual-machine-functions/{name}/console`）。
// 更深的动态路径必须在 uiDynamicWired 里显式登记（见下）——否则"前缀把一切都算已接"会**虚报覆盖**。
func uiCovers(lits []string, path string) bool {
	for _, l := range lits {
		if l == path {
			return true
		}
	}
	return false
}

// uiDynamicWired 由前端**动态拼接**构造、字面量提取不到、但确实调用了的路径 → 出处说明。
// 收紧 uiCovers 之后，这类路径必须显式登记，否则会被误判成"未接"。
var uiDynamicWired = map[string]string{
	"/acls/{name}":                                                    "网络对象表的详情按钮：objDetail('/acls/' + name)",
	"/bonds/{name}":                                                   "同上（bond 详情）",
	"/qos/policies/{name}":                                            "同上（QoS 详情）",
	"/port-mirroring/{name}":                                          "同上（SPAN 详情）",
	"/container-functions/{name}":                                     "容器行的详情按钮：objDetail('/container-functions/' + name)",
	"/images/{name}":                                                  "镜像行的详情按钮：objDetail('/images/' + name)",
	"/system/kernel":                                                  "系统卡的内核基线小节：api('/system/kernel')",
	"/vrfs/{name}/routes":                                             "bigRoutes()：api('/vrfs/' + name + '/routes')",
	"/vrfs/{name}":                                                    "bigRoutes() 的路由表覆盖了排障所需（VRF 详情暂无独立入口）",
	"/nat/sessions":                                                   "bigNat()：api('/nat/sessions')",
	"/interfaces/{name}":                                              "loadInterfaceStats()：api('/interfaces/' + name)（逐口取计数）",
	"/virtual-machine-functions/{name}":                               "loadVMStats()：api('/virtual-machine-functions/' + name)（取 vhost-user 计数）",
	"/virtual-switches/{name}":                                        "loadVSwitchStats()：api('/virtual-switches/' + name)（取成员口计数）",
	"/virtual-machine-functions/{name}:start":                         "vmAction()：POST …/{name}:start",
	"/virtual-machine-functions/{name}:stop":                          "vmAction()：POST …/{name}:stop",
	"/virtual-machine-functions/{name}:restart":                       "vmAction()：POST …/{name}:restart",
	"/container-functions/{name}:start":                               "ctAction()：POST …/{name}:start",
	"/container-functions/{name}:stop":                                "ctAction()：POST …/{name}:stop",
	"/container-functions/{name}:restart":                             "ctAction()：POST …/{name}:restart",
	"/container-functions/{name}/logs":                                "ctLogsLoad()：fetch('/container-functions/' + name + '/logs?tail=200')",
	"/virtual-machine-functions/{name}/console":                       "vmConsoleOpen()：api('/virtual-machine-functions/' + name + '/console')",
	"/virtual-machine-functions/{name}/snapshots":                     "vmSnapLoad()/vmSnapCreate()：api(… + '/snapshots')",
	"/virtual-machine-functions/{name}/snapshots/{snapshot}":          "vmSnapAct()：DELETE …/snapshots/{snapshot}",
	"/virtual-machine-functions/{name}/snapshots/{snapshot}:rollback": "vmSnapAct()：POST …:rollback",
	"/virtual-machine-functions/{name}/console/ws":                    "同上的 WebSocket：new WebSocket(… + res.ws_url)（ws_url 由该端点返回）",
	"/vpp/capture/{file}":                                             "renderCapture()：downloadFile('/vpp/capture/' + name, …)",
	"/system/backup/{file}":                                           "renderArchives()：downloadFile('/system/backup/' + name, …)",
	"/system/tech-support/{file}":                                     "renderArchives()：downloadFile('/system/tech-support/' + name, …)",
}

// contractPathSet 契约里的全部路径（方法无关）。
func contractPathSet(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for r := range contractRoutes(t) {
		out[strings.TrimSpace(r[strings.IndexByte(r, ' ')+1:])] = true
	}
	return out
}

// TestUIWiredPathsExistInContract 前端调的路径必须在契约里（端点改名/写错即红）。
func TestUIWiredPathsExistInContract(t *testing.T) {
	contractPaths := contractPathSet(t)
	for _, p := range uiUsedPaths(t) {
		if strings.HasSuffix(p, "/") { // 前缀字面量：只要有任一路径以它为前缀即可
			found := false
			for cp := range contractPaths {
				if strings.HasPrefix(cp, p) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("ui/app.js 用了前缀 %q，但契约里没有任何路径以它开头", p)
			}
			continue
		}
		if !contractPaths[p] {
			t.Errorf("ui/app.js 调用了 %q，但契约里没有该路径（端点改名或写错了？）", p)
		}
	}
}

// TestUICoverageClassified 契约里每条路径要么被界面使用、要么在 uiNotWired 里写明理由
// ——**新增端点忘记归类即红**，界面缺口数因此始终可见（与 CLI⇄REST 同一纪律）。
func TestUICoverageClassified(t *testing.T) {
	contractPaths := contractPathSet(t)
	lits := uiUsedPaths(t)

	unclassified := []string{}
	wired, unwired := 0, 0
	for p := range contractPaths {
		switch {
		case uiCovers(lits, p) || uiDynamicWired[p] != "":
			wired++
		case uiNotWired[p] != "":
			unwired++
		default:
			unclassified = append(unclassified, p)
		}
	}
	if len(unclassified) > 0 {
		sort.Strings(unclassified)
		t.Errorf("以下契约路径既没被界面使用、也没在 uiNotWired 里归类（新增端点请归类或接入界面）：\n  %s",
			strings.Join(unclassified, "\n  "))
	}

	// uiNotWired 里不许有"幽灵条目"（写错路径名会让断言变成空转）
	ghosts := []string{}
	for p := range uiNotWired {
		if !contractPaths[p] {
			ghosts = append(ghosts, p)
		}
	}
	for p := range uiDynamicWired {
		if !contractPaths[p] {
			ghosts = append(ghosts, p)
		}
	}
	if len(ghosts) > 0 {
		sort.Strings(ghosts)
		t.Errorf("uiNotWired 里的以下路径不是契约里的真实路径（拼错或已删？）：\n  %s", strings.Join(ghosts, "\n  "))
	}

	// 已接的路径不许再出现在 uiNotWired 里（否则缺口数虚高）
	stale := []string{}
	for p := range uiNotWired {
		if uiCovers(lits, p) || uiDynamicWired[p] != "" {
			stale = append(stale, p)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("以下路径界面已接，却仍列在 uiNotWired 里（缺口数会虚高）：\n  %s", strings.Join(stale, "\n  "))
	}

	t.Logf("界面覆盖：已接 %d 条路径 / 未接 %d 条（均已写明理由）/ 契约共 %d 条",
		wired, unwired, len(contractPaths))
}

// TestUIRoutesEndpointsExistInContract 路由表里声明的端点必须都在契约里——
// 防止"路由表写出幽灵端点"（界面取一个服务端没有的路径，页面只会静默空掉）。
func TestUIRoutesEndpointsExistInContract(t *testing.T) {
	contractPaths := contractPathSet(t)
	rb, err := os.ReadFile("ui/routes.json")
	if err != nil {
		t.Fatalf("读取 ui/routes.json: %v", err)
	}
	var doc struct {
		Routes []struct {
			Path      string   `json:"path"`
			Endpoints []string `json:"endpoints"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(rb, &doc); err != nil {
		t.Fatalf("ui/routes.json 不是合法 JSON: %v", err)
	}
	for _, r := range doc.Routes {
		for _, p := range r.Endpoints {
			q := p
			if i := strings.IndexByte(q, '?'); i >= 0 {
				q = q[:i]
			}
			if !contractPaths[q] {
				t.Errorf("路由 %s 声明的端点 %s 不在契约里", r.Path, q)
			}
		}
	}
}
