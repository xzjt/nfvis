package api

// 决策 #116：契约声明的**响应形状**必须真的在响应里。
//
// 由来（R37-1）：`routes_contract_test.go` 只守护「路由 ⊆ 契约」（**路径**），于是
// `/system/status` 少回 `cpu`/`memory`/`hugepages`/`storage`、`/interfaces` 少回运行态字段，
// 长期没人发现——照契约（FR-API-002 明说 Web 控制面据此开发）写出来的客户端一律取空。
// 更隐蔽的一层：契约里 `VppStatus` 被**重复定义**，`safe_load` 静默取最后一个，
// 发布的规范与实现不符（已由 `check_openapi_no_duplicate_keys.sh` 守护）。
//
// 本用例按契约声明的 schema **逐字段**核对实际响应：字段必须出现（字符串还必须非空），
// 条件出现或暂未实现的字段进下面的显式白名单并**写明理由**（新增条目必须写理由）。

import (
	"encoding/json"
	"net/http"

	"github.com/xzjt/nfvis/internal/metrics"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
	ksys "github.com/xzjt/nfvis/internal/system"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// shapeConditional 「字段会合法缺席（或为空串）」的白名单：`METHOD /path` → 字段 → 理由。
var shapeConditional = map[string]map[string]string{
	"GET /system/status": {
		"hostname": "配置声明优先；未声明时回退主机实际主机名（os.Hostname）；两者都取不到才为空串（不编造）",
		"cpu":      "依赖宿主指标：Linux 采集（读 /proc），非 Linux 上是空实现",
		"memory":   "同上",
		"storage":  "同上",
	},
	"GET /interfaces": {
		"enabled":        "运行态字段：VppStateRuntime 未接入时不返回（不编造）",
		"link":           "同上",
		"driver":         "同上",
		"speed_mbps":     "同上；且 DPDK 口速率可能为 0（取不到就不给）",
		"mac":            "仅内核未接管口有 sysfs 来源（决策 #302）；VPP 口暂无 MAC 来源，测试环境无 /sys 故验不到",
		"numa_node":      "模型未采集（无数据源）",
		"mtu":            "有效 MTU：配置未显式给且运行态未上报时省略（不编造 0/默认值）",
		"description":    "未配置时省略；内核未接管口无配置描述",
		"sriov":          "未配置 SR-IOV 时省略",
		"statistics":     "仅在详情端点 /interfaces/{name} 附带",
		"taken_over":     "决策 #302：VPP 运行态不可判定（未接入/查询失败）时省略，不编造",
		"ingress_policy": "决策 #331：未绑定入向限速策略时省略（配置对象 omitempty；不编造空串）",
		"egress_policy":  "决策 #331：未绑定出向限速策略时省略（配置对象 omitempty；不编造空串）",
		"storm_control":  "决策 #385：未配置入向风暴抑制时省略（配置对象 omitempty；不编造空对象）",
		"port_security":  "决策 #389：未配置端口安全白名单时省略（数组 omitempty；不编造空数组）",
	},
	"GET /system/version": {
		// R37-2 已收口（决策 #118）：ubuntu/libvirt/qemu/docker 经 VersionProbe 探测、
		// vpp 与 /vpp/status 同源，本用例经注入的假源核到这些键。唯独 dpdk 没有可靠
		// 版本来源（随 VPP 静态链接进 dpdk_plugin.so、show_version 不含、运行进程也
		// 没有独立 DPDK 库可读），有意不汇报——这是唯一"合法缺席"的键。
		"dpdk": "无可靠版本来源（DPDK 静态链接进 VPP 插件、binary API 不含），有意不汇报",
	},
	"GET /vpp/status": {
		"last_error":          "无错误时为空串",
		"buffers_source":      "无 buffer 统计时省略",
		"buffers_unavailable": "有数据时省略（决策 #68）",
		"threads":             "未连接数据面时省略",
		"buffers":             "未连接或统计不可用时省略（改由 buffers_unavailable 说明）",
		"memory":              "stats segment 不可用时省略",
		// 注意：`config_revision` **不在白名单里**——它已实现（决策 #116），必须真的发得出来。
		// 白名单只该收"会合法缺席"的字段；把已实现字段列进来会让对应断言变成空转。
	},
	"GET /login-banner": {
		// 决策 #303：未设置横幅时 banner 字段省略——不回空串、不编造，客户端（Web 登录页
		// 与 CLI 登录前）据此不渲染横幅块；端点也只回这一个字段。
		"banner": "未设置横幅时省略该字段（不编造空串）；设置了就必须原样发出",
	},
	"GET /metrics/history": {
		// 决策 #356：历史时序读视图。不可用时仍 200，以 available=false + reason 如实说明；
		// 可用时省略 reason（不编造），点集被裁剪时才置 truncated。
		"reason":    "仅当 available=false 时出现（如实说明不可用原因；可用时省略，不编造）",
		"truncated": "仅当点集被 step/limit 裁剪时出现（未裁剪时省略）",
	},
	"GET /vxlan-tunnels": {
		// 决策 #383：运行态不可用（未注入/数据面未连接）时给出原因；可用时省略（不编造）。
		"runtime_reason": "仅当 runtime_available=false 时出现（如实说明不可用原因；可用时省略，不编造）",
	},
	"GET /system/firewall": {
		// 决策 #388：未声明管理口时省略；applied=false 时给原因（未接入运行态/未配置残留表/
		// 未收敛等），一致时省略（不编造）。
		"mgmt_interface": "未声明管理口时省略（防火墙的前置是管理口已声明；测试服务基线未声明）",
		"error":          "仅当 applied=false 时出现（如实说明原因；一致时省略，不编造）",
	},
}

// TestResponseShapeMatchesContract 契约声明的响应字段必须出现在实际响应里。
func TestResponseShapeMatchesContract(t *testing.T) {
	// /system/version 的组件版本经注入的假源供给（R37-2 收口，决策 #118）：
	// 真机可用性由 internal/system 的单测与真机复验覆盖，这里只核"handler 把声明字段发出去"。
	ts := newTestServerOpts(t, Options{
		Versions: fakeVersions{},
		VPP:      &fakeVppController{status: VppStatus{Version: "26.06-release", Connected: true}},
	})
	token := loginAdmin(t, ts)

	// 种一台接口：否则 /interfaces 是空数组，检查会**落空**（空数组什么都验不到）。
	status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/interfaces/ens2f0", token,
		map[string]any{"name": "ens2f0", "description": "to-TOR", "mtu": 9000},
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("写入接口配置: %d %s", status, body)
	}

	spec := loadEmbeddedSpec(t)
	// 宿主指标在非 Linux 上不产出：此时 /system/status 的 cpu/memory/storage 必然缺席，
	// 而它们已在白名单里，无需额外分支——只是本机验不到，如实记一行。
	if !hasHostMetrics() {
		t.Log("提示：本机非 Linux，宿主指标不产出，/system/status 的 cpu/memory/storage 只会走白名单")
	}

	for _, ep := range []struct{ method, path string }{
		{"GET", "/system/status"},
		{"GET", "/system/version"},
		{"GET", "/interfaces"},
		{"GET", "/resource-pools"},
		{"GET", "/system/hugepages"},          // 决策 #329：大页池三方数字（恒有 pools/reclaimable）
		{"GET", "/configuration"},             // 决策 #119：整配置出口（committed）
		{"GET", "/system/api-tokens"},         // 决策 #301：活动会话清单（登录后恒有≥1 条，自己的会话）
		{"GET", "/login-banner"},              // 决策 #303：登录横幅（未设置时走白名单省略）
		{"GET", "/configuration/permissions"}, // 决策 #304：生效权限视图（调用者自己 class）
		{"GET", "/metrics/history"},           // 决策 #356：历史时序读视图（不可用时仍 200 + available=false + reason）
		{"GET", "/vxlan-tunnels"},             // 决策 #383：VXLAN 隧道读视图（运行态不可用时仍 200 + runtime_available=false + reason）
		{"GET", "/system/firewall"},           // 决策 #388：主机防火墙读视图（未接入/未配置恒 200 + applied=false + error）
	} {
		props := declaredProps(t, spec, ep.path, ep.method)
		if len(props) == 0 {
			t.Fatalf("%s %s：契约里取不到响应字段（schema 缺 properties？）", ep.method, ep.path)
		}
		status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+ep.path, token, nil, nil)
		if status != http.StatusOK {
			t.Fatalf("%s %s：状态 %d", ep.method, ep.path, status)
		}
		got := responseObject(t, body)
		key := ep.method + " " + ep.path
		allow := shapeConditional[key]
		checked, allowed := 0, 0
		for _, f := range props {
			v, ok := got[f]
			if !ok {
				if r, a := allow[f]; a {
					allowed++
					t.Logf("  跳过 %s.%s（%s）", key, f, r)
					continue
				}
				t.Errorf("%s：契约声明了 %s，响应里没有（照契约开发的客户端会取空）", key, f)
				continue
			}
			// 字符串字段还必须非空：声明了却恒回空串，与"没实现"是一回事（R37-2 就是这么发现的）。
			if sv, isStr := v.(string); isStr && sv == "" {
				if r, a := allow[f]; a {
					allowed++
					t.Logf("  跳过 %s.%s（%s）", key, f, r)
					continue
				}
				t.Errorf("%s：契约声明了字符串字段 %s，但响应里是空串（形同未实现）", key, f)
				continue
			}
			checked++
		}
		t.Logf("%s：核对 %d 个字段，白名单跳过 %d 个", key, checked, allowed)
	}
}

// TestConfigurationHistoryShapeMatchesContract GET /configuration/history 的响应形状
// （决策 #142：契约声明的是**数组**，元素五个字段必须真的发得出来）。
//
// 与上面几个端点不同，这里不能用 responseObject（顶层是数组），故单独核：
// ① 响应是数组；② 元素的每个契约字段都在；③ 布尔字段必须是布尔；
// ④ `user`/`comment` 允许为空串——**理由**：迁移前写入的历史快照没有提交者
// （存储 schema v3 才加该列，老记录保持 NULL，**不谎称已知**），未填 commit 说明
// 时 comment 也是空串。这两个字段的空值是有意义的事实，不是"没实现"。
func TestConfigurationHistoryShapeMatchesContract(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	spec := loadEmbeddedSpec(t)
	props := declaredProps(t, spec, "/configuration/history", "GET")
	if len(props) == 0 {
		t.Fatal("契约里取不到 /configuration/history 的响应字段（schema 缺 properties？）")
	}

	// 先提交两次：一条历史至少要有内容才验得到东西（空数组什么都验不到）。
	for _, host := range []string{"hist-node-1", "hist-node-2"} {
		status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
			map[string]any{"system": map[string]any{"hostname": host,
				"login": map[string]any{"users": []map[string]any{superUserDoc()}}}},
			map[string]string{"X-NFVIS-Auto-Commit": "true"})
		if status != http.StatusOK {
			t.Fatalf("提交 %s: %d %s", host, status, body)
		}
	}

	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/history", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /configuration/history: %d %s", status, body)
	}
	var items []map[string]any
	if err := json.Unmarshal(body, &items); err != nil {
		t.Fatalf("响应不是数组（契约声明为数组）: %v %s", err, body)
	}
	if len(items) == 0 {
		t.Fatal("提交后仍无历史条目（列表取自配置库，应有内容）")
	}
	allowedEmpty := map[string]string{
		"user":    "迁移前写入的历史快照未记录提交者（存储 v3 才加该列），空串是如实结果",
		"comment": "commit 未填说明时为空串",
	}
	currents := 0
	for i, it := range items {
		for _, f := range props {
			v, ok := it[f]
			if !ok {
				t.Errorf("第 %d 条历史缺少契约字段 %s（照契约开发的客户端会取空）", i, f)
				continue
			}
			if sv, isStr := v.(string); isStr && sv == "" {
				if r, a := allowedEmpty[f]; a {
					t.Logf("  第 %d 条 %s 为空串（%s）", i, f, r)
					continue
				}
				t.Errorf("第 %d 条历史的字符串字段 %s 是空串（形同未实现）", i, f)
			}
		}
		if cur, ok := it["current"].(bool); !ok {
			t.Errorf("第 %d 条历史的 current 应为布尔（契约声明 boolean），得到 %T", i, it["current"])
		} else if cur {
			currents++
		}
	}
	if currents != 1 {
		t.Errorf("current=true 的条目应恰有 1 条（当前 committed），实得 %d", currents)
	}
	t.Logf("/configuration/history：核对 %d 条历史 × %d 个字段", len(items), len(props))
}

// TestLoginResponseShapeMatchesContract POST /login 的响应形状（round39 可视验收补的覆盖）。
// 契约 LoginResponse.user 是**对象** LoginUser（name/class），而实现曾回扁平字符串 + 顶层
// class——照契约（FR-API-002：Web 控制面据此开发）写的前端把 body.user 当对象用，顶栏于是
// 显示 "undefined（undefined）"。此前守护只核四个 GET 端点，这类漂移覆盖不到。
func TestLoginResponseShapeMatchesContract(t *testing.T) {
	ts := newTestServer(t)
	spec := loadEmbeddedSpec(t)
	props := declaredProps(t, spec, "/login", "POST")
	if len(props) == 0 {
		t.Fatal("契约里取不到 /login 的响应字段")
	}
	status, body := postJSON(t, ts.URL+APIPrefix+"/login",
		loginRequest{Username: "admin", Password: "s3cret-Passw0rd!"}, nil)
	if status != http.StatusOK {
		t.Fatalf("登录: %d %s", status, body)
	}
	got := responseObject(t, body)
	for _, f := range props {
		v, ok := got[f]
		if !ok {
			t.Errorf("POST /login：契约声明了 %s，响应里没有（照契约开发的客户端会取空）", f)
			continue
		}
		if sv, isStr := v.(string); isStr && sv == "" {
			t.Errorf("POST /login：契约声明了字符串字段 %s，但响应里是空串（形同未实现）", f)
		}
	}
	// user 必须是对象且 name/class 非空（LoginUser）；退回扁平字符串正是本轮抓到的缺陷形态。
	user, ok := got["user"].(map[string]any)
	if !ok {
		t.Fatalf("POST /login：user 应为对象（契约 LoginUser），得到 %T", got["user"])
	}
	for _, f := range []string{"name", "class"} {
		if sv, _ := user[f].(string); sv == "" {
			t.Errorf("POST /login：user.%s 为空（契约 LoginUser）", f)
		}
	}
}

// TestVppStatusStructCoversContract /vpp/status 需要数据面 Provider（测试进程里起不了），
// 改用**静态核对**：契约声明的每个字段，响应结构体都得能发出来（json tag 覆盖）。
// 这正是 VppStatus 漂移的形态——契约写的字段结构体里根本没有。
func TestVppStatusStructCoversContract(t *testing.T) {
	spec := loadEmbeddedSpec(t)
	props := declaredProps(t, spec, "/vpp/status", "GET")
	if len(props) == 0 {
		t.Fatal("契约里取不到 /vpp/status 的响应字段")
	}
	tags := map[string]bool{}
	rt := reflect.TypeOf(VppStatus{})
	for i := 0; i < rt.NumField(); i++ {
		name := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			tags[name] = true
		}
	}
	allow := shapeConditional["GET /vpp/status"]
	for _, f := range props {
		if tags[f] {
			continue
		}
		if r, ok := allow[f]; ok {
			t.Logf("  跳过 %s（%s）", f, r)
			continue
		}
		t.Errorf("契约声明了 /vpp/status.%s，但 VppStatus 结构体发不出该字段（缺 json tag）", f)
	}
}

// ---------- 辅助 ----------

// loadEmbeddedSpec 解析随二进制内嵌的规范（客户端看到的正是它）。
func loadEmbeddedSpec(t *testing.T) map[string]any {
	t.Helper()
	var spec map[string]any
	if err := json.Unmarshal(openAPISpec, &spec); err != nil {
		t.Fatalf("解析内嵌规范: %v", err)
	}
	return spec
}

// declaredProps 取某端点 200 响应的**字段名**；数组响应取 items 的字段。
func declaredProps(t *testing.T, spec map[string]any, path, method string) []string {
	t.Helper()
	return declaredPropsStatus(t, spec, path, method, "200")
}

// declaredPropsStatus 同上，可取任意状态码（如 202）。
func declaredPropsStatus(t *testing.T, spec map[string]any, path, method, status string) []string {
	t.Helper()
	paths, _ := spec["paths"].(map[string]any)
	node, _ := paths[path].(map[string]any)
	op, _ := node[strings.ToLower(method)].(map[string]any)
	resp, _ := op["responses"].(map[string]any)
	ok, _ := resp[status].(map[string]any)
	schema := schemaOf(t, spec, ok)
	if items, ok := schema["items"].(map[string]any); ok {
		schema = schemaOf(t, spec, items)
	}
	props, _ := schema["properties"].(map[string]any)
	out := make([]string, 0, len(props))
	for k := range props {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestFormatDataResponseShapeMatchesContract 决策 #305：`POST /system:format-data`（202）的响应形状。
//
// 契约声明的统计字段必须真的发得出来。只有两个字段是**条件出现**：
//   - `already_factory`：已是出厂态才出现（幂等第二次执行的如实结论）；非出厂态缺席；
//   - `residuals`：无残留时缺席；**有残留时端点返回 500**（走错误分支），202 里因此不见。
//
// 两者都是"有意义的事实"而非"没实现"——与 zeroize 的响应口径一致（zeroize 不设响应 schema，
// 本端点按用户要求把统计写进契约，故这里单列一条守护）。
func TestFormatDataResponseShapeMatchesContract(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	spec := loadEmbeddedSpec(t)
	props := declaredPropsStatus(t, spec, "/system:format-data", "POST", "202")
	if len(props) == 0 {
		t.Fatal("契约里取不到 POST /system:format-data 的 202 响应字段（schema 缺 properties？）")
	}
	status, _, body := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system:format-data", token,
		map[string]any{"confirm": true}, map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusAccepted {
		t.Fatalf("POST /system:format-data：状态 %d %s", status, body)
	}
	got := responseObject(t, body)
	// 条件出现字段的白名单（理由见函数注释）。
	optional := map[string]string{
		"already_factory": "已是出厂态才出现（幂等第二次执行的如实结论）",
		"residuals":       "无残留时缺席；有残留时端点返回 500（错误分支），202 里因此不见",
	}
	checked, allowed := 0, 0
	for _, f := range props {
		if _, ok := got[f]; ok {
			checked++
			continue
		}
		if reason, a := optional[f]; a {
			allowed++
			t.Logf("  跳过 %s（%s）", f, reason)
			continue
		}
		t.Errorf("POST /system:format-data：契约声明了 %s，响应里没有（照契约开发的客户端会取空）", f)
	}
	// 统计的必发字段逐个点名（防"整体缺席也算过"）。
	for _, f := range []string{"status", "revision", "kept_sections", "removed_objects",
		"removed_images", "purged_files", "freed_bytes"} {
		if _, ok := got[f]; !ok {
			t.Errorf("统计字段 %s 必须发出（契约声明且非条件字段）", f)
		}
	}
	t.Logf("POST /system:format-data：核对 %d 个字段，白名单跳过 %d 个", checked, allowed)
}

// TestHugepageReclaimResponseShapeMatchesContract 决策 #329：`POST /system/hugepages:reclaim`
// 的响应形状（契约声明的字段必须真的发得出来）。
//
// 只有三个字段是**条件出现**：
//   - `blockers`：无法回收时给出「谁在占用」的证据；能回收/无需回收时缺席；
//   - `error`：写入失败/回读不一致时的原因；正常收敛时缺席；
//   - `note` 属读视图（GET）的条件字段，不在本端点。
//
// 其余字段（page_size/declared/actual_before/actual_after/in_use/free/reclaimed/action/reasons）
// 一律必发——「字段没发出来」与「没有这一项」分不清正是本守护要防的漂移。
func TestHugepageReclaimResponseShapeMatchesContract(t *testing.T) {
	root := t.TempDir()
	writeHugepageFixture(t, root, "1G", 4, 2) // 声明 2 时造出 2 页可回收的空闲多余页
	ts := newTestServerOpts(t, Options{
		Hugepages:    ksys.SysfsHugepageSetter{Root: root},
		HugepageRoot: root,
	})
	token := loginAdmin(t, ts)
	if status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/resource-pools", token,
		map[string]any{"hugepages": []map[string]any{{"page_size": "1G", "count": 2}}},
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("声明大页池: %d %s", status, body)
	}
	spec := loadEmbeddedSpec(t)
	props := declaredPropsStatus(t, spec, "/system/hugepages:reclaim", "POST", "200")
	if len(props) == 0 {
		t.Fatal("契约里取不到 POST /system/hugepages:reclaim 的 200 响应字段")
	}
	status, _, body := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/hugepages:reclaim", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("POST reclaim: %d %s", status, body)
	}
	got := responseObject(t, body)
	for _, f := range props {
		if _, ok := got[f]; !ok {
			t.Errorf("契约声明了 %s，响应里没有（照契约开发的客户端会取空）", f)
		}
	}
	// 逐池字段（items.properties）也要真的发得出来。
	itemProps := declaredItemProps(t, spec, "/system/hugepages:reclaim", "POST", "200")
	if len(itemProps) == 0 {
		t.Fatal("契约里取不到回收结果 items 的字段")
	}
	pools, _ := got["pools"].([]any)
	if len(pools) == 0 {
		t.Fatal("pools 为空——测试应先造出无主占用（否则什么都验不到）")
	}
	first, _ := pools[0].(map[string]any)
	optional := map[string]string{
		"blockers": "无法回收时才出现（谁在占用的证据）",
		"error":    "写入失败/回读不一致时才出现",
	}
	for _, f := range itemProps {
		if _, ok := first[f]; ok {
			continue
		}
		if r, a := optional[f]; a {
			t.Logf("  跳过 pools[0].%s（%s）", f, r)
			continue
		}
		t.Errorf("契约声明了 pools[].%s，该池响应里没有", f)
	}
	// reasons 是声明为 array 的字段，必须是数组而非 null。
	if _, ok := first["reasons"].([]any); !ok {
		t.Errorf("pools[].reasons 应为数组，得到 %T（发 null 会让「没有依据」与「没实现」分不清）", first["reasons"])
	}
}

// TestHugepagePoolsReadShapeMatchesContract 决策 #329/#346/#353：`GET /system/hugepages` 的
// 读视图形状（契约声明字段必须真的发得出来，含 items 级字段）。
//
// 重点在**决策 #353 新增的 `held_by_dataplane` 是恒发字段**：取不到时也要发 -1（不编造、
// 也不省略）——「字段缺席」与「取不到」分不清正是本守护要防的漂移。本 fixture 的临时根
// 没有 /proc，恰好覆盖「取不到 → -1 仍发出」这一路；有 /proc 的正路（comm=vpp 归属）
// 由 internal/system 与本包 CLI/REST 单测覆盖。
func TestHugepagePoolsReadShapeMatchesContract(t *testing.T) {
	root := t.TempDir()
	writeHugepageFixture(t, root, "1G", 2, 0)
	ts := newTestServerOpts(t, Options{HugepageRoot: root})
	token := loginAdmin(t, ts)
	if status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/resource-pools", token,
		map[string]any{"hugepages": []map[string]any{{"page_size": "1G", "count": 2}}},
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("声明大页池: %d %s", status, body)
	}
	spec := loadEmbeddedSpec(t)
	props := declaredProps(t, spec, "/system/hugepages", "GET")
	itemProps := declaredItemProps(t, spec, "/system/hugepages", "GET", "200")
	if len(props) == 0 || len(itemProps) == 0 {
		t.Fatal("契约里取不到 GET /system/hugepages 的响应字段（schema 缺 properties？）")
	}
	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/hugepages", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /system/hugepages: %d %s", status, body)
	}
	got := responseObject(t, body)
	for _, f := range props {
		if _, ok := got[f]; !ok {
			t.Errorf("契约声明了 %s，响应里没有（照契约开发的客户端会取空）", f)
		}
	}
	pools, _ := got["pools"].([]any)
	if len(pools) != 2 {
		t.Fatalf("pools 应恒为两个页尺寸，实得 %d：%s", len(pools), body)
	}
	optional := map[string]string{"note": "判定依据 / 取不到的原因只在有话说时出现（条件出现）"}
	for i, raw := range pools {
		p, _ := raw.(map[string]any)
		for _, f := range itemProps {
			if _, ok := p[f]; ok {
				continue
			}
			if r, a := optional[f]; a {
				t.Logf("  跳过 pools[%d].%s（%s）", i, f, r)
				continue
			}
			t.Errorf("契约声明了 pools[].%s，响应里没有", f)
		}
		// held_by_dataplane（决策 #353）恒发：本 fixture 无 /proc → -1，且必须是数字而非 null/省略。
		if v, ok := p["held_by_dataplane"].(float64); !ok || v != -1 {
			t.Errorf("pools[%d].held_by_dataplane = %v（%T），无 /proc 时也应恒发 -1 而不是省略",
				i, p["held_by_dataplane"], p["held_by_dataplane"])
		}
	}
}

// declaredItemProps 取某端点响应 schema 的 items.properties 字段名（数组元素字段）。
func declaredItemProps(t *testing.T, spec map[string]any, path, method, status string) []string {
	t.Helper()
	paths, _ := spec["paths"].(map[string]any)
	node, _ := paths[path].(map[string]any)
	op, _ := node[strings.ToLower(method)].(map[string]any)
	resp, _ := op["responses"].(map[string]any)
	ok, _ := resp[status].(map[string]any)
	schema := schemaOf(t, spec, ok)
	// schema.properties.pools.items
	props, _ := schema["properties"].(map[string]any)
	pools, _ := props["pools"].(map[string]any)
	items, _ := pools["items"].(map[string]any)
	itemProps, _ := items["properties"].(map[string]any)
	out := make([]string, 0, len(itemProps))
	for k := range itemProps {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// schemaOf 取 content.application/json.schema（或 items），必要时解一层 $ref。
func schemaOf(t *testing.T, spec map[string]any, node map[string]any) map[string]any {
	t.Helper()
	if node == nil {
		return nil
	}
	if c, ok := node["content"].(map[string]any); ok {
		if aj, ok := c["application/json"].(map[string]any); ok {
			node, _ = aj["schema"].(map[string]any)
		}
	}
	if ref, ok := node["$ref"].(string); ok {
		parts := strings.Split(strings.TrimPrefix(ref, "#/"), "/")
		cur := spec
		for _, p := range parts {
			next, ok := cur[p].(map[string]any)
			if !ok {
				t.Fatalf("解 $ref %s 失败于 %s", ref, p)
			}
			cur = next
		}
		return cur
	}
	return node
}

// responseObject 响应体 → 字段表；数组响应取**第一个元素**。
func responseObject(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err == nil && obj != nil {
		return obj
	}
	var arr []map[string]any
	if err := json.Unmarshal(body, &arr); err != nil {
		t.Fatalf("响应既不是对象也不是对象数组: %s", body)
	}
	if len(arr) == 0 {
		t.Fatalf("响应是空数组——什么都验不到（测试应先种一个对象）：%s", body)
	}
	return arr[0]
}

// hasHostMetrics 宿主指标是否产出（Linux 才有）。
func hasHostMetrics() bool {
	return len(metrics.HostMetrics()) > 0
}

// fakeDHCPShapeRuntime DHCP 服务器运行态假实现（形状守护用；生产注入 *network.L2Network）。
type fakeDHCPShapeRuntime struct {
	leases []network.DHCPLease
}

func (f *fakeDHCPShapeRuntime) DHCPServerLeases(string) ([]network.DHCPLease, bool) {
	return f.leases, true
}
func (f *fakeDHCPShapeRuntime) DHCPServerActiveLeases(string) (int, bool) {
	return len(f.leases), true
}
func (f *fakeDHCPShapeRuntime) DHCPTapIndexes() map[uint32]bool { return nil }

// componentProps 从内嵌契约取某 component schema 的属性名（解析结果为空即失败——
// 排版/结构变了守护必须跟着改，而不是静默空转）。
func componentProps(t *testing.T, spec map[string]any, name string) map[string]bool {
	t.Helper()
	components, _ := spec["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	props, _ := schemas[name].(map[string]any)["properties"].(map[string]any)
	if len(props) == 0 {
		t.Fatalf("契约 components.schemas.%s 取不到 properties（解析规则失效或契约改动）", name)
	}
	out := make(map[string]bool, len(props))
	for k := range props {
		out[k] = true
	}
	return out
}

// schemaPropsAt 取某 schema 下对象型属性**自身**的 properties 字段名（嵌套对象用，
// 如 VirtualSwitch.dhcp_server）。
func schemaPropsAt(t *testing.T, spec map[string]any, schema, field string) map[string]bool {
	t.Helper()
	components, _ := spec["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	node, _ := schemas[schema].(map[string]any)["properties"].(map[string]any)[field].(map[string]any)
	props, _ := node["properties"].(map[string]any)
	if len(props) == 0 {
		t.Fatalf("契约 %s.%s 取不到 properties（解析规则失效或契约改动）", schema, field)
	}
	out := make(map[string]bool, len(props))
	for k := range props {
		out[k] = true
	}
	return out
}

// assertPropsExact 响应字段与契约声明的集合**双向相等**：契约声明了而响应发不出（客户端
// 照契约取空）、或响应多出契约没有的字段（实现与契约漂移）都算失败。
func assertPropsExact(t *testing.T, key string, got map[string]any, want map[string]bool) {
	t.Helper()
	for f := range want {
		if _, ok := got[f]; !ok {
			t.Errorf("%s：契约声明了 %s，响应里没有（照契约开发的客户端会取空）", key, f)
		}
	}
	for f := range got {
		if !want[f] {
			t.Errorf("%s：响应里的 %s 不在契约中（实现与契约漂移，或漏改 openapi）", key, f)
		}
	}
}

// TestDHCPServerShapeMatchesContract DHCP 服务器**字段级**形状守护（决策 #359 测试清单；
// 同 #332 的教训——路径级守护挡不住「端点对、字段错」）：① 详情响应的 dhcp_server 对象与
// 契约 VirtualSwitch.dhcp_server 逐字段一致；② 租约端点元素与契约 DhcpLease 逐字段一致
// ——REST 直接序列化 network.DHCPLease，漏 json tag 会按 Go 字段名输出（IP/MAC/…），
// Web 与其它照契约写的客户端全部取空，本守护钉死该回归。
func TestDHCPServerShapeMatchesContract(t *testing.T) {
	ts := newTestServerOpts(t, Options{
		DHCPServer: &fakeDHCPShapeRuntime{leases: []network.DHCPLease{{
			IP: "192.168.100.10", MAC: "aa:bb:cc:dd:ee:01", State: "active", ExpiresInSeconds: 3600,
		}}},
	})
	token := loginAdmin(t, ts)

	// 种一台启用 dhcp-server 的 L2 交换机（校验前置：L2 + IPv4 BVI 网关；pool 两键同时给）。
	status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		map[string]any{
			"system": map[string]any{
				"hostname": "shape-dhcp",
				"login":    map[string]any{"users": []map[string]any{superUserDoc()}},
			},
			"virtual_switches": []map[string]any{{
				"name": "vs-dhcp", "type": "l2",
				"gateway":                        map[string]any{"addresses": []string{"192.168.100.1/24"}},
				"dhcp_server_pool_start":         "192.168.100.10",
				"dhcp_server_pool_end":           "192.168.100.20",
				"dhcp_server_lease_time_seconds": 3600,
				"dhcp_server_dns":                "192.168.100.1",
				"dhcp_server_domain_name":        "lab.local",
			}},
		},
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("种启用 dhcp-server 的交换机: %d %s", status, body)
	}

	spec := loadEmbeddedSpec(t)

	// ① GET /virtual-switches/{name}：dhcp_server 对象字段与契约 VirtualSwitch.dhcp_server
	//   双向一致（六字段全配置 + 假运行态在场 ⇒ 都应发得出来）。
	status, _, body = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-switches/vs-dhcp", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /virtual-switches/vs-dhcp: %d %s", status, body)
	}
	resp := responseObject(t, body)
	ds, ok := resp["dhcp_server"].(map[string]any)
	if !ok {
		t.Fatalf("详情响应缺 dhcp_server 对象（配置了 pool 就必须出现）: %s", body)
	}
	assertPropsExact(t, "GET /virtual-switches/{name}.dhcp_server", ds,
		schemaPropsAt(t, spec, "VirtualSwitch", "dhcp_server"))

	// ② GET /virtual-switches/{name}/dhcp-leases：元素字段与契约 DhcpLease 双向一致，
	//   且值逐字对（防 json tag 错位映射——字段名对了值挂了同样取空/串号）。
	status, _, body = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-switches/vs-dhcp/dhcp-leases", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /virtual-switches/vs-dhcp/dhcp-leases: %d %s", status, body)
	}
	lease := responseObject(t, body) // 数组响应取第一个元素
	assertPropsExact(t, "GET /virtual-switches/{name}/dhcp-leases[]", lease,
		componentProps(t, spec, "DhcpLease"))
	if lease["ip"] != "192.168.100.10" || lease["state"] != "active" || lease["mac"] != "aa:bb:cc:dd:ee:01" {
		t.Errorf("租约字段的值与注入的运行态不符（json tag 错位？）: %v", lease)
	}
	if v, ok := lease["expires_in_seconds"].(float64); !ok || v != 3600 {
		t.Errorf("expires_in_seconds 应为数值 3600: %v", lease["expires_in_seconds"])
	}
}
