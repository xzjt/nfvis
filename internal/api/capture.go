package api

// 数据面抓包 API（FR-OPS-042）。
//
// 抓包是**数据面无关**的能力（VPP 走 pcap trace、内核走 tcpdump），故端点用数据面中立的
// 新名 `/capture`；旧名 `/vpp/capture` 保留为**兼容别名**（既有脚本/集成不破），
// 两组路径由**同一批 handler** 承载（语义、状态码、导出目录与文件名口径逐字相同）：
//
// GET    /capture          抓包会话状态与已导出 pcap 清单（别名 /vpp/capture）
// POST   /capture          开始抓包（202；已有会话 409）
// DELETE /capture          停止抓包（?export=true 时同时导出 pcap，200 + 文件行）
// GET    /capture/{file}   下载 pcap（octet-stream）
//
// 契约与手册以新名为准（CLI 侧同理：`show capture` / `request capture …`，
// 旧写法 `show vpp capture` / `request vpp trace …` 仍可用）。

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

// CaptureRuntime 抓包能力（*network.CaptureProvider 或内核侧的 *netkernel.Capture 经适配注入；
// nil = 503）。
type CaptureRuntime interface {
	Status() (*CaptureSessionRow, []CaptureFileRow)
	Start(ctx context.Context, ifname string, count int, filterACL string) error
	Stop(ctx context.Context, export bool) (CaptureFileRow, error)
	Path(name string) (string, error)
}

// requireCapture 抓包能力是否已接入。
//
// 两种数据面**都有**实现（VPP pcap trace / 内核 tcpdump），故 nil 只剩一种成因：
// 装配缺口（编排器未装配抓包 Provider）——如实回 503，不让操作者去猜数据面。
func (s *Server) requireCapture(w http.ResponseWriter) bool {
	if s.capture == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "抓包模块未接入", nil)
		return false
	}
	return true
}

// handleGetCapture GET /api/v1/capture（别名 /api/v1/vpp/capture）
func (s *Server) handleGetCapture(w http.ResponseWriter, r *http.Request) {
	if !s.requireCapture(w) {
		return
	}
	active, files := s.capture.Status()
	writeJSON(w, http.StatusOK, map[string]any{"active": active, "files": files})
}

// handlePostCapture POST /api/v1/capture（别名 /api/v1/vpp/capture）
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
	s.engine.Audit(user, "capture.start", "开始抓包 "+in.Interface, "success")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "capturing", "interface": in.Interface})
}

// handleDeleteCapture DELETE /api/v1/capture（别名 /api/v1/vpp/capture）
//
// 缺省停止并丢弃缓冲（204）；`?export=true` 表示**停止并导出**（200 + 导出的文件行），
// 与 CLI `request capture export` 同义。
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
		s.engine.Audit(user, "capture.export", note, "success")
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
	s.engine.Audit(user, "capture.stop", "停止抓包（不导出）", "success")
	w.WriteHeader(http.StatusNoContent)
}

// handleDownloadCapture GET /api/v1/capture/{file}（别名 /api/v1/vpp/capture/{file}）
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
