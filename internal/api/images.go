package api

// M4-8：镜像仓库 API（FR-CMP-030~033）。
//
// POST /images 二选一：JSON {url,sha256?} 异步拉取；JSON {incoming_file} 从 /data/incoming 导入。
// 删除前校验引用（VNF/容器），被引用返回 409（FR-CMP-033）。

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/xzjt/nfvis/internal/images"
)

// ImagesRuntime 镜像仓库能力（*images.Store 注入；nil = 503）。
type ImagesRuntime interface {
	List() []images.Meta
	Get(name string) (images.Meta, bool)
	Download(ctx context.Context, opts images.DownloadOptions) (images.Meta, error)
	ImportIncoming(name, typ, incomingFile, description string) (images.Meta, error)
	Delete(name string, refCount int) error
}

// imageResponse 镜像详情（元数据 + 引用计数，契约 Image）。
type imageResponse struct {
	images.Meta
	RefCount int `json:"ref_count"`
}

func (s *Server) requireImages(w http.ResponseWriter) bool {
	if s.images == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "镜像仓库未接入", nil)
		return false
	}
	return true
}

// handleListImages GET /api/v1/images
func (s *Server) handleListImages(w http.ResponseWriter, r *http.Request) {
	if !s.requireImages(w) {
		return
	}
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	metas := s.images.List()
	out := make([]imageResponse, 0, len(metas))
	for _, m := range metas {
		out = append(out, imageResponse{Meta: m, RefCount: images.RefCount(cfg, m.Name)})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetImage GET /api/v1/images/{name}
func (s *Server) handleGetImage(w http.ResponseWriter, r *http.Request) {
	if !s.requireImages(w) {
		return
	}
	name := r.PathValue("name")
	m, ok := s.images.Get(name)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("镜像 %s 不存在", name), nil)
		return
	}
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, imageResponse{Meta: m, RefCount: images.RefCount(cfg, name)})
}

// handlePostImage POST /api/v1/images（URL 拉取 或 incoming 导入）
func (s *Server) handlePostImage(w http.ResponseWriter, r *http.Request) {
	if !s.requireImages(w) {
		return
	}
	var in struct {
		Name         string `json:"name"`
		Type         string `json:"type"`
		URL          string `json:"url"`
		SHA256       string `json:"sha256"`
		Description  string `json:"description"`
		IncomingFile string `json:"incoming_file"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if in.Name == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "name 必填", nil)
		return
	}
	if (in.URL == "") == (in.IncomingFile == "") {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "url 与 incoming_file 必须且只能指定其一", nil)
		return
	}
	if in.IncomingFile != "" {
		m, err := s.images.ImportIncoming(in.Name, in.Type, in.IncomingFile, in.Description)
		if err != nil {
			writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
			return
		}
		writeJSON(w, http.StatusCreated, m)
		return
	}
	// URL 拉取为异步（进度经 GET /images/{name} 的 import_state 观察；事件流随 M5）。
	// 但参数校验必须**同步**先做：否则缺 sha256 会被当成"受理成功"再静默转 failed
	// （FR-SEC-004 默认强制校验，决策 #71⑤）。
	dlOpts := images.DownloadOptions{Name: in.Name, Type: in.Type, URL: in.URL, SHA256: in.SHA256, Description: in.Description}
	if err := images.ValidateDownloadOptions(dlOpts); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	go func(opts images.DownloadOptions) {
		_, _ = s.images.Download(context.Background(), opts)
	}(dlOpts)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "downloading", "name": in.Name})
}

// handleDeleteImage DELETE /api/v1/images/{name}（被引用 409，FR-CMP-033）
func (s *Server) handleDeleteImage(w http.ResponseWriter, r *http.Request) {
	if !s.requireImages(w) {
		return
	}
	name := r.PathValue("name")
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	if err := s.images.Delete(name, images.RefCount(cfg, name)); err != nil {
		switch {
		case errors.Is(err, images.ErrReferenced):
			writeError(w, http.StatusConflict, "CONFLICT", err.Error(), nil)
		case errors.Is(err, images.ErrNotFound):
			writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error(), nil)
		default:
			writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "name": name})
}
