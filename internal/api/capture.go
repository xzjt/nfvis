package api

// M5-3：数据面抓包 API（FR-OPS-042）。
//
// GET    /vpp/capture          抓包会话状态与已导出 pcap 清单
// POST   /vpp/capture          开始抓包（202；已有会话 409）
// DELETE /vpp/capture          停止抓包（?export=true 时同时导出 pcap，200 + 文件行）
// GET    /vpp/capture/{file}   下载 pcap（octet-stream）

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// CaptureSessionRow 活动抓包会话（契约 CaptureStatus.active）。
type CaptureSessionRow struct {
	Interface string    `json:"interface"`
	Captured  int       `json:"captured"`
	StartedAt time.Time `json:"started_at"`
	MaxDepth  int       `json:"max_depth,omitempty"`
}

// CaptureFileRow 已导出 pcap（契约 CaptureStatus.files）。
type CaptureFileRow struct {
	Name      string    `json:"name"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

// CaptureRuntime 抓包能力（*network.CaptureProvider 经适配注入；nil = 503）。
type CaptureRuntime interface {
	Status() (*CaptureSessionRow, []CaptureFileRow)
	Start(ctx context.Context, ifname string, count int, filterACL string) error
	Stop(ctx context.Context, export bool) (CaptureFileRow, error)
	Path(name string) (string, error)
}

func (s *Server) requireCapture(w http.ResponseWriter) bool {
	if s.capture == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "抓包模块未接入", nil)
		return false
	}
	return true
}

// handleGetCapture GET /api/v1/vpp/capture
func (s *Server) handleGetCapture(w http.ResponseWriter, r *http.Request) {
	if !s.requireCapture(w) {
		return
	}
	active, files := s.capture.Status()
	writeJSON(w, http.StatusOK, map[string]any{"active": active, "files": files})
}

// handlePostCapture POST /api/v1/vpp/capture
func (s *Server) handlePostCapture(w http.ResponseWriter, r *http.Request) {
	if !s.requireCapture(w) {
		return
	}
	var in struct {
		Interface string `json:"interface"`
		Count     int    `json:"count"`
		FilterACL string `json:"filter_acl"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if in.Interface == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "interface 必填", nil)
		return
	}
	if err := s.capture.Start(r.Context(), in.Interface, in.Count, in.FilterACL); err != nil {
		switch {
		case errors.Is(err, network.ErrCaptureActive):
			writeError(w, http.StatusConflict, "CONFLICT", err.Error(), nil)
		case errors.Is(err, network.ErrCaptureFilterUnsupported):
			writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		case errors.Is(err, network.ErrIfaceUnavailable):
			writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		default:
			writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		}
		return
	}
	user := "api"
	if info, ok := Identity(r); ok {
		user = info.User
	}
	s.engine.Audit(user, "vpp.capture.start", "开始抓包 "+in.Interface, "success")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "capturing", "interface": in.Interface})
}

// handleDeleteCapture DELETE /api/v1/vpp/capture
//
// 缺省停止并丢弃缓冲（204）；`?export=true` 表示**停止并导出**（200 + 导出的文件行），
// 与 CLI `request vpp trace export` 同义。
func (s *Server) handleDeleteCapture(w http.ResponseWriter, r *http.Request) {
	if !s.requireCapture(w) {
		return
	}
	export := r.URL.Query().Get("export") == "true"
	f, err := s.capture.Stop(r.Context(), export)
	if err != nil {
		if errors.Is(err, network.ErrNoCapture) {
			writeError(w, http.StatusConflict, "CONFLICT", err.Error(), nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	user := "api"
	if info, ok := Identity(r); ok {
		user = info.User
	}
	if export {
		note := "停止抓包并导出 " + f.Name
		if f.Name == "" {
			note = "停止抓包（未捕获到报文，无文件导出）"
		}
		s.engine.Audit(user, "vpp.capture.export", note, "success")
		if f.Name == "" {
			// 未捕获到报文：如实说明，不回一个不存在的文件
			writeJSON(w, http.StatusOK, map[string]any{"exported": false, "message": "未捕获到报文，无文件导出"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"exported": true, "name": f.Name, "size_bytes": f.SizeBytes, "created_at": f.CreatedAt,
		})
		return
	}
	s.engine.Audit(user, "vpp.capture.stop", "停止抓包（不导出）", "success")
	w.WriteHeader(http.StatusNoContent)
}

// handleDownloadCapture GET /api/v1/vpp/capture/{file}
func (s *Server) handleDownloadCapture(w http.ResponseWriter, r *http.Request) {
	if !s.requireCapture(w) {
		return
	}
	name := r.PathValue("file")
	path, err := s.capture.Path(name)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error(), nil)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeFile(w, r, path)
}
