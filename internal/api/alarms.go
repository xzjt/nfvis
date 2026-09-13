package api

// M3-8：告警列表端点（FR-OPS-010）。恢复收敛无法补齐的项记录于此，
// 由编排器注入的 AlarmRuntime 提供；本层不 import orchestrator。

import (
	"fmt"
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
	// Clear 删除已 resolved 告警（M5-9，FR-OPS-022）；all=true 清全部。
	Clear(id string, all bool) int
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

// handleClearAlarms POST /api/v1/alarms:clear（清除已 resolved 告警，204）。
func (s *Server) handleClearAlarms(w http.ResponseWriter, r *http.Request) {
	if s.alarms == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "告警表未接入", nil)
		return
	}
	var in struct {
		ID  string `json:"id"`
		All bool   `json:"all"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if in.ID == "" && !in.All {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "需指定 id 或 all=true", nil)
		return
	}
	n := s.alarms.Clear(in.ID, in.All)
	user := "api"
	if info, ok := Identity(r); ok {
		user = info.User
	}
	s.engine.Audit(user, "alarms.clear", fmt.Sprintf("清除 %d 条已 resolved 告警（id=%q all=%v）", n, in.ID, in.All), "success")
	w.WriteHeader(http.StatusNoContent)
}
