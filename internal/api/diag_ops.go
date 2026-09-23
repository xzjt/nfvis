package api

// M5-4：诊断归档与 core dump 管理（FR-OPS-040/041）。
//
// GET    /system/tech-support          归档列表
// POST   /system/tech-support          生成诊断归档（202）
// GET    /system/tech-support/{file}   下载归档
// GET    /system/core-dumps            转储清单
// DELETE /system/core-dumps?file=      删除转储（缺省全部，204）
// POST   /system/core-dumps:export     导出转储清单到 URL（宿主操作）

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/xzjt/nfvis/internal/system"
)

// DiagOpsRuntime 诊断能力（*system.TechSupport + *system.CoreDumps 包装；nil = 503）。
type DiagOpsRuntime interface {
	GenerateTechSupport() (system.File, error)
	ListTechSupport() []system.File
	TechSupportPath(name string) (string, error)
	ListCoreDumps() []system.CoreDump
	DeleteCoreDumps(file string) (int, error)
	// ExportCoreDumps 把转储清单 POST 到目标 URL（条数、目标端 HTTP 状态、错误）。
	ExportCoreDumps(ctx context.Context, url string) (int, int, error)
}

func (s *Server) requireDiagOps(w http.ResponseWriter) bool {
	if s.diagOps == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "诊断模块未接入", nil)
		return false
	}
	return true
}

func (s *Server) handleListTechSupport(w http.ResponseWriter, r *http.Request) {
	if !s.requireDiagOps(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.diagOps.ListTechSupport())
}

func (s *Server) handleCreateTechSupport(w http.ResponseWriter, r *http.Request) {
	if !s.requireDiagOps(w) {
		return
	}
	f, err := s.diagOps.GenerateTechSupport()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	user := "api"
	if info, ok := Identity(r); ok {
		user = info.User
	}
	s.engine.Audit(user, "system.tech-support", "生成诊断归档 "+f.File, "success")
	writeJSON(w, http.StatusAccepted, f)
}

func (s *Server) handleDownloadTechSupport(w http.ResponseWriter, r *http.Request) {
	if !s.requireDiagOps(w) {
		return
	}
	name := r.PathValue("file")
	path, err := s.diagOps.TechSupportPath(name)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error(), nil)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeFile(w, r, path)
}

func (s *Server) handleListCoreDumps(w http.ResponseWriter, r *http.Request) {
	if !s.requireDiagOps(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.diagOps.ListCoreDumps())
}

// handleDeleteCoreDumps DELETE /api/v1/system/core-dumps?file=<name>（缺省全部，204）。
func (s *Server) handleDeleteCoreDumps(w http.ResponseWriter, r *http.Request) {
	if !s.requireDiagOps(w) {
		return
	}
	file := r.URL.Query().Get("file")
	n, err := s.diagOps.DeleteCoreDumps(file)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error(), nil)
		return
	}
	user := "api"
	if info, ok := Identity(r); ok {
		user = info.User
	}
	s.engine.Audit(user, "system.core-dumps.delete", "删除 core dump "+file, "success")
	w.WriteHeader(http.StatusNoContent)
	_ = n
}

// handleExportCoreDumps POST /api/v1/system/core-dumps:export（FR-OPS-041）
//
// 与 CLI `request system core-dumps export <url>` 同一实现：把转储清单 POST 到目标 URL。
// 目标不合法 → 400；目标不可达/非 2xx → **502**（上游失败，不谎报成功）。
func (s *Server) handleExportCoreDumps(w http.ResponseWriter, r *http.Request) {
	if !s.requireDiagOps(w) {
		return
	}
	var in struct {
		URL string `json:"url"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if strings.TrimSpace(in.URL) == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "url 必填", nil)
		return
	}
	user := "api"
	if info, ok := Identity(r); ok {
		user = info.User
	}
	n, status, err := s.diagOps.ExportCoreDumps(r.Context(), in.URL)
	if err != nil {
		s.engine.Audit(user, "system.core-dumps.export", "导出转储清单至 "+in.URL, err.Error())
		// 目标不合法（http/https 之外）算入参问题，其余算上游失败
		code, msg := http.StatusBadGateway, "EXPORT_FAILED"
		if strings.Contains(err.Error(), "导出地址") {
			code, msg = http.StatusBadRequest, "VALIDATION_FAILED"
		}
		writeError(w, code, msg, err.Error(), nil)
		return
	}
	s.engine.Audit(user, "system.core-dumps.export",
		fmt.Sprintf("导出 %d 个转储清单至 %s（HTTP %d）", n, in.URL, status), "success")
	msg := fmt.Sprintf("已导出 %d 个转储清单至 %s（HTTP %d）", n, in.URL, status)
	if n == 0 {
		msg = "（无 core dump 可导出）目标已收到空清单"
	}
	writeJSON(w, http.StatusOK, map[string]any{"exported": n, "url": in.URL, "status": status, "message": msg})
}
