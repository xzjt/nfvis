package api

// M5-6：配置备份/恢复/恢复出厂（FR-OPS-004~007）。
//
// GET    /system/backup            归档列表
// POST   /system/backup            生成归档（202）
// GET    /system/backup/{file}     下载归档（octet-stream，路径限定在备份目录内）
// POST   /system/restore           从归档恢复（multipart file → candidate 提交）
// POST   /system:zeroize           恢复出厂（JSON confirm=true，双重确认）

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/images"
	"github.com/xzjt/nfvis/internal/system"
)

// SystemOpsRuntime 备份/恢复/恢复出厂能力（*system.Manager 注入；nil = 503）。
type SystemOpsRuntime interface {
	Backup() (system.File, error)
	List() []system.File
	Path(name string) (string, error)
	Restore(ctx context.Context, data []byte, user string) (config.CommitResult, []images.Meta, error)
	Zeroize(ctx context.Context, user string) (system.ZeroizeResult, error)
}

func (s *Server) requireSystemOps(w http.ResponseWriter) bool {
	if s.sysOps == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "系统运维模块未接入", nil)
		return false
	}
	return true
}

func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	if !s.requireSystemOps(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.sysOps.List())
}

func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	if !s.requireSystemOps(w) {
		return
	}
	f, err := s.sysOps.Backup()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	user := "api"
	if info, ok := Identity(r); ok {
		user = info.User
	}
	s.engine.Audit(user, "system.backup", "生成配置备份 "+f.File, "success")
	writeJSON(w, http.StatusAccepted, f)
}

func (s *Server) handleDownloadBackup(w http.ResponseWriter, r *http.Request) {
	if !s.requireSystemOps(w) {
		return
	}
	name := r.PathValue("file")
	path, err := s.sysOps.Path(name)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error(), nil)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeFile(w, r, path)
}

// handleRestore POST /api/v1/system/restore（multipart 上传归档 → candidate 提交）。
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	if !s.requireSystemOps(w) {
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "解析 multipart 失败: "+err.Error(), nil)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "缺少 file 字段（multipart/form-data）", nil)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 64<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "读取归档失败: "+err.Error(), nil)
		return
	}
	user := "api"
	if info, ok := Identity(r); ok {
		user = info.User
	}
	res, manifest, err := s.sysOps.Restore(r.Context(), data, user)
	if err != nil {
		switch {
		case errors.Is(err, system.ErrBadFormat), errors.Is(err, system.ErrUnsupported):
			writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		case errors.Is(err, config.ErrLocked), errors.Is(err, config.ErrNotEditing):
			writeError(w, http.StatusConflict, "CONFLICT", err.Error(), nil)
		default:
			var ve *config.ValidationError
			if errors.As(err, &ve) {
				mapEngineError(w, err) // 逐条 detail 由统一映射输出
				return
			}
			writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		}
		return
	}
	s.engine.Audit(user, "system.restore", "从备份归档恢复配置", "success")
	writeJSON(w, http.StatusOK, map[string]any{
		"revision": res.Revision, "warnings": res.Warnings,
		"images_in_archive": len(manifest),
		"note":              "镜像文件本体不在归档内，如被配置引用需另行导入",
	})
}

// handleZeroize POST /api/v1/system:zeroize（FR-OPS-007，需 confirm=true）。
func (s *Server) handleZeroize(w http.ResponseWriter, r *http.Request) {
	if !s.requireSystemOps(w) {
		return
	}
	var in struct {
		Confirm bool `json:"confirm"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if !in.Confirm {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "恢复出厂需 confirm=true（双重确认）", nil)
		return
	}
	user := "api"
	if info, ok := Identity(r); ok {
		user = info.User
	}
	res, err := s.sysOps.Zeroize(r.Context(), user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	s.engine.Audit(user, "system.zeroize", "恢复出厂（清空配置/镜像/VNF，重置账号）", "success")
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status": "zeroized", "revision": res.Revision, "removed_images": res.RemovedImages,
		"note": strings.TrimSpace("配置/镜像/VNF 已清空，账号复位；重启后进入初始化状态"),
	})
}
