package api

// M5-1：事件流（SSE）。GET /events 推送 alarm-raised/resolved、vnf-state-changed、
// image-import-progress、config-committed（契约 Event，FR-API-006 / FR-OPS-020~022）。
//
// 传输语义：订阅进程内总线（internal/events），逐条写 `id/event/data` 帧并按帧 Flush；
// 支持 `Last-Event-ID` 断线补发；20s 心跳注释帧防中间设备断连；客户端断开经
// r.Context() 感知后取消订阅（通道关闭）。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/xzjt/nfvis/internal/events"
)

// publishVNFState 发布 VNF/容器状态变化事件（nil 总线时静默）。
func (s *Server) publishVNFState(kind, name, state string) {
	if s.events == nil {
		return
	}
	s.events.Publish(events.TypeVNFStateChanged, map[string]any{
		"resource": kind, "name": name, "state": state,
	})
}

// handleEvents GET /api/v1/events（text/event-stream）
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if s.events == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "事件总线未接入", nil)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "响应不支持流式输出", nil)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	// 断线补发：Last-Event-ID 之后的历史（客户端可据此不丢事件）。
	if last, err := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64); err == nil {
		for _, ev := range s.events.Since(last) {
			if err := writeSSE(w, ev); err != nil {
				return
			}
		}
		fl.Flush()
	}

	ch, cancel := s.events.Subscribe()
	defer cancel()
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if err := writeSSE(w, ev); err != nil {
				return
			}
			fl.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, ev events.Event) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, data)
	return err
}
