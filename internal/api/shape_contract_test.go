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
	"reflect"
	"sort"
	"strings"
	"testing"
)

// shapeConditional 「字段会合法缺席（或为空串）」的白名单：`METHOD /path` → 字段 → 理由。
var shapeConditional = map[string]map[string]string{
	"GET /system/status": {
		"hostname": "取自 committed 的 system.hostname：未配置时为空串（真机上通常已配置；有意不退回内核 hostname）",
		"cpu":      "依赖宿主指标：Linux 采集（读 /proc），非 Linux 上是空实现",
		"memory":   "同上",
		"storage":  "同上",
	},
	"GET /interfaces": {
		"enabled":     "运行态字段：VppStateRuntime 未接入时不返回（不编造）",
		"link":        "同上",
		"driver":      "同上",
		"speed_mbps":  "同上；且 DPDK 口速率可能为 0（取不到就不给）",
		"mac":         "模型未采集（无数据源）",
		"numa_node":   "模型未采集（无数据源）",
		"mtu":         "未配置时省略",
		"description": "未配置时省略",
		"sriov":       "未配置 SR-IOV 时省略",
		"statistics":  "仅在详情端点 /interfaces/{name} 附带",
	},
	"GET /system/version": {
		// 桩：7 个键都在、只有 nfvis 有值。补齐要跨 libvirt/docker/qemu 三个 provider
		// 与宿主事实探测，属独立一批（决策 #116 登记的 R37-2），故此处显式标注而不是假装通过。
		"ubuntu":  "R37-2：组件版本探测未实现（当前是桩，返回空串）",
		"vpp":     "R37-2：同上",
		"dpdk":    "R37-2：同上",
		"libvirt": "R37-2：同上",
		"qemu":    "R37-2：同上",
		"docker":  "R37-2：同上",
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
}

// TestResponseShapeMatchesContract 契约声明的响应字段必须出现在实际响应里。
func TestResponseShapeMatchesContract(t *testing.T) {
	ts := newTestServer(t)
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
	paths, _ := spec["paths"].(map[string]any)
	node, _ := paths[path].(map[string]any)
	op, _ := node[strings.ToLower(method)].(map[string]any)
	resp, _ := op["responses"].(map[string]any)
	ok200, _ := resp["200"].(map[string]any)
	schema := schemaOf(t, spec, ok200)
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
