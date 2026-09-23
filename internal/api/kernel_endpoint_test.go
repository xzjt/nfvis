package api

// R51-2 收口（决策 #137）：`GET /system/kernel` —— 契约早已声明但服务端从未注册的幽灵路径。
// 实现后要求：形状与契约一致（KernelBaseline 的字段真的发得出来），且与 CLI `show system kernel` 同源。

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestKernelEndpointShape(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/kernel", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /system/kernel: %d %s", status, data)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("解析: %v (%s)", err, data)
	}
	// 契约 KernelBaseline 声明的字段必须真的发得出来（R37-1 的口径）
	for _, k := range []string{"cmdline", "isolated_cores", "hugepages_1g", "hugepages_1g_free",
		"hugepages_2m", "hugepages_2m_free", "thp", "desired", "diffs"} {
		if _, ok := got[k]; !ok {
			t.Errorf("响应缺字段 %s（契约已声明）: %s", k, data)
		}
	}
	if _, ok := got["cmdline"].([]any); !ok {
		t.Errorf("cmdline 应为数组（契约 array of string），得到 %T", got["cmdline"])
	}
	if _, ok := got["diffs"].([]any); !ok {
		t.Errorf("diffs 应为数组，得到 %T", got["diffs"])
	}
	// desired 子树必须按**契约的 snake_case** 出键（R37-1 类：曾按 Go 字段名出键）
	des, _ := got["desired"].(map[string]any)
	for _, k := range []string{"hugepages_1g", "hugepages_2m", "isolated_cores", "nmi_watchdog", "transparent_hugepages"} {
		if _, ok := des[k]; !ok {
			t.Errorf("desired 缺契约字段 %s（实际键：%v）", k, des)
		}
	}
}
