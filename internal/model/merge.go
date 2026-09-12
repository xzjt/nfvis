package model

import (
	"encoding/json"
	"fmt"
)

// Merge 将 patch 合并进 dst 并返回新配置（load merge 语义，FR-CFG-008）：
//   - 对象：递归合并；
//   - 具名对象数组：按身份字段（name/interface/prefix/seq/node/server/page_size）
//     匹配的元素递归合并，未匹配的追加；
//   - 标量数组与标量：整体替换。
//
// patch 中的零值（空串/0/false/null）视为「未设置」被剪枝——merge 不能用于清零，
// 清零走 delete 语义；这使部分填充的 Config 结构可作为增量补丁使用。
func Merge(dst, patch Config) (Config, error) {
	var out Config
	dj, err := json.Marshal(&dst)
	if err != nil {
		return out, fmt.Errorf("序列化 dst: %w", err)
	}
	pj, err := json.Marshal(&patch)
	if err != nil {
		return out, fmt.Errorf("序列化 patch: %w", err)
	}
	var dm, pm map[string]any
	if err := json.Unmarshal(dj, &dm); err != nil {
		return out, fmt.Errorf("反序列化 dst: %w", err)
	}
	if err := json.Unmarshal(pj, &pm); err != nil {
		return out, fmt.Errorf("反序列化 patch: %w", err)
	}
	pruned, keep := pruneZero(pm)
	if !keep {
		pruned = map[string]any{}
	}
	merged := mergeMaps(dm, pruned.(map[string]any))
	mj, err := json.Marshal(merged)
	if err != nil {
		return out, fmt.Errorf("序列化合并结果: %w", err)
	}
	if err := json.Unmarshal(mj, &out); err != nil {
		return out, fmt.Errorf("反序列化合并结果: %w", err)
	}
	return out, nil
}

// pruneZero 递归剪除零值叶子。空对象/空数组保留（空数组表示整体清空该标量列表）。
func pruneZero(v any) (any, bool) {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			if pe, keep := pruneZero(e); keep {
				out[k] = pe
			}
		}
		return out, len(out) > 0 || len(x) == 0
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			if pe, keep := pruneZero(e); keep {
				out = append(out, pe)
			}
		}
		return out, len(out) > 0 || len(x) == 0
	case string:
		return x, x != ""
	case float64:
		return x, x != 0
	case bool:
		return x, x
	default:
		return x, x != nil
	}
}

func mergeMaps(dst, patch map[string]any) map[string]any {
	out := make(map[string]any, len(dst)+len(patch))
	for k, v := range dst {
		out[k] = v
	}
	for k, pv := range patch {
		dv, exists := out[k]
		if !exists {
			out[k] = pv
			continue
		}
		dm, dok := dv.(map[string]any)
		pm, pok := pv.(map[string]any)
		switch {
		case dok && pok:
			out[k] = mergeMaps(dm, pm)
		case isArray(dv) && isArray(pv):
			out[k] = mergeArrays(dv.([]any), pv.([]any))
		default:
			out[k] = pv
		}
	}
	return out
}

func isArray(v any) bool {
	_, ok := v.([]any)
	return ok
}

func mergeArrays(dst, patch []any) []any {
	out := append([]any{}, dst...)
	for _, pe := range patch {
		pm, ok := pe.(map[string]any)
		if !ok {
			// 标量数组：整体替换
			return append([]any{}, patch...)
		}
		_, pid := identityOf(pm)
		if pid == "" {
			out = append(out, pe)
			continue
		}
		matched := false
		for i, de := range out {
			dm, ok := de.(map[string]any)
			if !ok {
				continue
			}
			if _, did := identityOf(dm); did == pid {
				out[i] = mergeMaps(dm, pm)
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, pe)
		}
	}
	return out
}
