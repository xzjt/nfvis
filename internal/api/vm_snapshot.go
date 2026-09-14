package api

// M4-6：VM 快照（FR-CMP-015/018）。qcow2 内部快照，含全部磁盘；操作入审计。

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// SnapshotRow 快照元数据（契约 Snapshot）。
type SnapshotRow struct {
	Name        string     `json:"name"`
	CreatedAt   *time.Time `json:"created_at,omitempty"`
	SizeBytes   int64      `json:"size_bytes,omitempty"`
	Description string     `json:"description,omitempty"`
}

// VMSnapshotRuntime 快照能力（libvirt 编排器注入；nil = 503）。
type VMSnapshotRuntime interface {
	SnapshotCreate(ctx context.Context, domain, name, description string) error
	Snapshots(ctx context.Context, domain string) ([]SnapshotRow, error)
	SnapshotRevert(ctx context.Context, domain, name string) error
	SnapshotDelete(ctx context.Context, domain, name string) error
}

func (s *Server) snapshotTarget(w http.ResponseWriter, r *http.Request) (name string, ok bool) {
	name = r.PathValue("name")
	if s.vmSnapshots == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "计算编排未接入（libvirt 未装配）", nil)
		return "", false
	}
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return "", false
	}
	if _, found := findVM(cfg, name); !found {
		writeError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("VM %s 不存在", name), nil)
		return "", false
	}
	return name, true
}

// handleListSnapshots GET /virtual-machine-functions/{name}/snapshots
func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	name, ok := s.snapshotTarget(w, r)
	if !ok {
		return
	}
	rows, err := s.vmSnapshots.Snapshots(r.Context(), name)
	if err != nil {
		s.snapshotError(w, err)
		return
	}
	if rows == nil {
		rows = []SnapshotRow{}
	}
	writeJSON(w, http.StatusOK, paginate(r, rows))
}

// handleCreateSnapshot POST /virtual-machine-functions/{name}/snapshots
func (s *Server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	name, ok := s.snapshotTarget(w, r)
	if !ok {
		return
	}
	var in SnapshotRow
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "快照 name 必填", nil)
		return
	}
	if msg, bad := snapshotNameInvalid(in.Name); bad {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", msg, nil)
		return
	}
	if err := s.vmSnapshots.SnapshotCreate(r.Context(), name, in.Name, in.Description); err != nil {
		s.auditSnapshot(r, "create", name, in.Name, err)
		s.snapshotError(w, err)
		return
	}
	s.auditSnapshot(r, "create", name, in.Name, nil)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "created", "name": in.Name})
}

// dispatchSnapshotPost 分发 `.../snapshots/{snapshot}:rollback`（冒号后缀需手工分发）。
func (s *Server) dispatchSnapshotPost(w http.ResponseWriter, r *http.Request) {
	name, ok := s.snapshotTarget(w, r)
	if !ok {
		return
	}
	tail := r.PathValue("tail")
	snap, action, cut := strings.Cut(tail, ":")
	if !cut || snap == "" || action != "rollback" {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "未知的快照动作路径: "+tail, nil)
		return
	}
	if err := s.vmSnapshots.SnapshotRevert(r.Context(), name, snap); err != nil {
		s.auditSnapshot(r, "rollback", name, snap, err)
		s.snapshotError(w, err)
		return
	}
	s.auditSnapshot(r, "rollback", name, snap, nil)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "reverted", "name": snap})
}

// handleDeleteSnapshot DELETE /virtual-machine-functions/{name}/snapshots/{snapshot}
func (s *Server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	name, ok := s.snapshotTarget(w, r)
	if !ok {
		return
	}
	snap := r.PathValue("snapshot")
	if err := s.vmSnapshots.SnapshotDelete(r.Context(), name, snap); err != nil {
		s.auditSnapshot(r, "delete", name, snap, err)
		s.snapshotError(w, err)
		return
	}
	s.auditSnapshot(r, "delete", name, snap, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "name": snap})
}

func (s *Server) auditSnapshot(r *http.Request, action, vm, snap string, err error) {
	user := "api"
	if info, ok := Identity(r); ok {
		user = info.User
	}
	result := "success"
	detail := fmt.Sprintf("%s snapshot %s/%s", action, vm, snap)
	if err != nil {
		result, detail = "failure", fmt.Sprintf("%s snapshot %s/%s: %v", action, vm, snap, err)
	}
	s.engine.Audit(user, "vm.snapshot."+action, detail, result)
}

func (s *Server) snapshotError(w http.ResponseWriter, err error) {
	writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
}

// snapshotNameInvalid 校验快照名（与 VM/资源名同规则：字母数字开头，允许 -_.）。
func snapshotNameInvalid(name string) (string, bool) {
	if len(name) > 64 {
		return "快照名长度需 ≤64", true
	}
	for i, c := range name {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.'
		if !ok || (i == 0 && !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9')) {
			return fmt.Sprintf("快照名 %q 不合法（字母数字-_.，≤64）", name), true
		}
	}
	return "", false
}
