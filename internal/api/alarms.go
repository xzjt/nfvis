package api

// M3-8：告警列表端点（FR-OPS-010）。恢复收敛无法补齐的项记录于此，
// 由编排器注入的 AlarmRuntime 提供；本层不 import orchestrator。

import (
	"net/http"
	"time"
)

// AlarmRow /alarms 一行（契约 components/schemas/Alarm）。
type AlarmRow struct {
	ID         string     `json:"id"`
	Severity   string     `json:"severity"`
	Code       string     `json:"code"`
	Message    string     `json:"message"`
	Source     string     `json:"source,omitempty"`
	RaisedAt   time.Time  `json:"raised_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	State      string     `json:"state"`
}

// AlarmRuntime 告警表读取能力（编排器装配注入；nil = 503）。
type AlarmRuntime interface {
	List(state string) []AlarmRow
}

// handleGetAlarms GET /api/v1/alarms：告警列表（state=active|resolved|all，缺省 active）。
func (s *Server) handleGetAlarms(w http.ResponseWriter, r *http.Request) {
	if s.alarms == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "告警表未接入（编排器未装配）", nil)
		return
	}
	state := r.URL.Query().Get("state")
	rows := s.alarms.List(state)
	if rows == nil {
		rows = []AlarmRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}
