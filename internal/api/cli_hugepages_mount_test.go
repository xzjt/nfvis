package api

import (
	"strings"
	"testing"
)

// 决策 #459（R13-E）：内核在 /proc/mounts 里把 hugetlbfs 的 pagesize= **规范化成「最大整数单位」
// 形式**（1G ⇒ `1024M`、2M ⇒ `2M`），故按解析出的 kB 值匹配，不按字面串——此前 1G 只匹配
// `1G`/`1048576kB`，与实况 `1024M` 不符而恒漏报。
func TestParsePagesizeKB(t *testing.T) {
	cases := map[string]int{
		"1024M":     1048576, // 内核实况：1G 挂载
		"2M":        2048,    // 内核实况：2M 挂载
		"2048kB":    2048,    // 兼容 kB 形态
		"1G":        1048576,
		"1048576kB": 1048576,
		"1048576":   1048576,
		" 1024M ":   1048576,
		"":          0,
		"abc":       0,
		"0M":        0,
	}
	for in, want := range cases {
		if got := parsePagesizeKB(in); got != want {
			t.Errorf("parsePagesizeKB(%q) = %d, want %d", in, got, want)
		}
	}
}

// 决策 #459：已声明但未挂载的页尺寸要如实进「需重启」差异（否则报「一致（无需重启）」而 2M 的 VM 起不来）。
func TestHugepageMountMissing(t *testing.T) {
	cases := []struct {
		name          string
		d1G, d2M      bool
		mounted       map[string]bool
		wantContain   []string
		wantEmptySize bool
	}{
		{"1G 已声明且已挂载", true, false, map[string]bool{"1G": true}, nil, true},
		{"1G 已声明未挂载", true, false, map[string]bool{}, []string{"大页 1G"}, false},
		{"双池仅 2M 未挂载", true, true, map[string]bool{"1G": true}, []string{"大页 2M"}, false},
		{"双池都挂载", true, true, map[string]bool{"1G": true, "2M": true}, nil, true},
		{"未声明任何池", false, false, map[string]bool{}, nil, true},
		{"仅 2M 声明未挂载", false, true, map[string]bool{"1G": true}, []string{"大页 2M"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hugepageMountMissing(tc.d1G, tc.d2M, tc.mounted)
			if tc.wantEmptySize {
				if len(got) != 0 {
					t.Fatalf("不应有差异，实际 %v", got)
				}
				return
			}
			for _, w := range tc.wantContain {
				found := false
				for _, g := range got {
					if strings.Contains(g, w) {
						found = true
					}
				}
				if !found {
					t.Fatalf("差异应含 %q，实际 %v", w, got)
				}
			}
		})
	}
}
