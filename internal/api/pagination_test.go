package api

// FR-API-007（决策 #74）：列表端点分页。
// 此前仅 /audit-logs 支持 limit（offset 还被静默忽略，决策 #70④ 已修），其余列表端点无分页。

import (
	"encoding/json"
	"net/http"
	"testing"
)

// 纯函数语义：缺省不截断、offset 越界返回空列表（非 null）、limit 超长取剩余。
func TestPaginateSemantics(t *testing.T) {
	items := []int{1, 2, 3, 4, 5}
	cases := []struct {
		q    string
		want []int
	}{
		{"", []int{1, 2, 3, 4, 5}},
		{"?limit=2", []int{1, 2}},
		{"?offset=2", []int{3, 4, 5}},
		{"?limit=2&offset=2", []int{3, 4}},
		{"?limit=0", []int{1, 2, 3, 4, 5}},   // limit<=0 = 不截断（向后兼容）
		{"?limit=-1", []int{1, 2, 3, 4, 5}},  // 负数同 0
		{"?offset=-3", []int{1, 2, 3, 4, 5}}, // 负 offset 按 0
		{"?offset=5", []int{}},               // 越界 → 空列表
		{"?offset=99", []int{}},              // 远越界 → 空列表
		{"?limit=99", []int{1, 2, 3, 4, 5}},  // 超长 → 全部
		{"?limit=2&offset=99", []int{}},      // 越界优先
	}
	for _, c := range cases {
		r, _ := http.NewRequest(http.MethodGet, "/x"+c.q, nil)
		got := paginate(r, items)
		if len(got) != len(c.want) {
			t.Fatalf("%q → %v，期望 %v", c.q, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%q → %v，期望 %v", c.q, got, c.want)
			}
		}
	}
	// 越界必须返回空列表而非 nil（JSON 序列化为 [] 而非 null）
	r, _ := http.NewRequest(http.MethodGet, "/x?offset=99", nil)
	if paginate(r, items) == nil {
		t.Fatal("越界应返回空列表而非 nil")
	}
}

// HTTP 层：真实列表端点（/images）分页生效且不影响既有调用。
func TestListEndpointPagination(t *testing.T) {
	store := newImagesStore(t)
	writeImageIndex(t, store, "a.qcow2")
	writeImageIndex(t, store, "b.qcow2")
	writeImageIndex(t, store, "c.qcow2")
	ts := newTestServerOpts(t, Options{Images: store})
	token := loginAdmin(t, ts)

	get := func(q string) []map[string]any {
		t.Helper()
		status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/images"+q, token, nil, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /images%s: %d %s", q, status, data)
		}
		var rows []map[string]any
		if err := json.Unmarshal(data, &rows); err != nil {
			t.Fatalf("解析: %v", err)
		}
		return rows
	}

	if got := get(""); len(got) != 3 {
		t.Fatalf("缺省应返回全部: %d", len(got))
	}
	if got := get("?limit=2"); len(got) != 2 {
		t.Fatalf("limit=2 应返回 2 条: %d", len(got))
	}
	page0, page1 := get("?limit=2&offset=0"), get("?limit=2&offset=2")
	if len(page0) != 2 || len(page1) != 1 {
		t.Fatalf("分页大小错误: %d %d", len(page0), len(page1))
	}
	if page0[0]["name"] == page1[0]["name"] {
		t.Fatalf("offset 未生效（两页出现同一条）: %v", page0[0])
	}
	if got := get("?offset=99"); len(got) != 0 {
		t.Fatalf("越界应返回空列表: %v", got)
	}
}
