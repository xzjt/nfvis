package api

// M5-4：诊断归档与 core dump 管理（FR-OPS-040/041）。
//
// GET    /system/tech-support          归档列表
// POST   /system/tech-support          生成诊断归档（202）
// GET    /system/tech-support/{file}   下载归档
// GET    /system/core-dumps            转储清单
// DELETE /system/core-dumps?file=      删除转储（缺省全部，204）

import (
	"net/http"

	"github.com/xzjt/nfvis/internal/system"
)

// DiagOpsRuntime 诊断能力（*system.TechSupport + *system.CoreDumps 包装；nil = 503）。
type DiagOpsRuntime interface {
	GenerateTechSupport() (system.File, error)
	ListTechSupport() []system.File
	TechSupportPath(name string) (string, error)
	ListCoreDumps() []system.CoreDump
	DeleteCoreDumps(file string) (int, error)
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
