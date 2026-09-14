package api

// FR-API-007：列表端点分页（limit/offset）。
//
// 此前只有 `/audit-logs` 支持 limit（且 offset 被静默忽略，已在决策 #70④ 修复）；
// 其余列表端点无分页参数。规格要求「分页（limit/offset）用于列表端点」。

import (
	"net/http"
	"strconv"
)

// paginate 对列表结果应用 limit/offset。
//
// 语义（保持向后兼容）：
//   - `limit` 缺省 / <=0 → **不截断**（既有调用方行为不变；客户端显式传 limit 才分页）；
//   - `offset` 缺省 0；负数按 0；超出长度返回**空列表**（非 null）；
//   - `limit` 超过剩余长度时返回剩余部分。
func paginate[T any](r *http.Request, items []T) []T {
	if r == nil {
		return items
	}
	q := r.URL.Query()
	offset, err := strconv.Atoi(q.Get("offset"))
	if err != nil || offset < 0 {
		offset = 0
	}
	if offset > 0 {
		if offset >= len(items) {
			return items[:0] // 空列表而非 null
		}
		items = items[offset:]
	}
	limit, err := strconv.Atoi(q.Get("limit"))
	if err == nil && limit > 0 && limit < len(items) {
		items = items[:limit]
	}
	return items
}
