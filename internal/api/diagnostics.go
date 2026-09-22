package api

// 诊断视图的 REST 出口（决策 #123）：round42 覆盖核查把「日志 / ping / traceroute /
// 清零统计」列为缺口 #2~#4/#7——它们的**能力在服务端都已存在**（CLI 走同一批运行时），
// 缺的只是对外端点。本文件把既有 `DiagRuntime` 与日志来源（`Options.LogSource`）接出来，
// 判定口径与 CLI **完全一致**（尤其是 ping 的「未通即失败」，附录 A #89/#93）。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// handleSystemLogs GET /system/logs：服务端日志尾部（与 CLI `show log system` 同源）。
func (s *Server) handleSystemLogs(w http.ResponseWriter, r *http.Request) {
	if s.logs == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "日志来源未接入", nil)
		return
	}
	data, err := s.logs()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "读取日志失败: "+err.Error(), nil)
		return
	}
	text := string(data)
	if last := r.URL.Query().Get("last"); last != "" {
		n, err := strconv.Atoi(last)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "last 须为正整数", nil)
			return
		}
		lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
		if len(lines) > n {
			lines = lines[len(lines)-n:]
		}
		text = strings.Join(lines, "\n") + "\n"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(text))
}

type pingRequest struct {
	Host   string `json:"host"`
	Source string `json:"source"`
	VRF    string `json:"vrf"`
	Count  int    `json:"count"`
}

type tracerouteRequest struct {
	Host string `json:"host"`
	VRF  string `json:"vrf"`
}

// handlePing POST /diagnostics/ping：经 VPP 数据面做连通性测试（与 CLI `ping` 同源）。
// 未通即 502（0 发包 / 0 应答都算失败）——判定口径与 CLI 一致，不让"没发出去"看起来像通了。
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	if s.diag == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "诊断模块未接入", nil)
		return
	}
	var req pingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Host) == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "需要 host", nil)
		return
	}
	out, err := s.diag.Ping(r.Context(), req.Host, req.Source, req.VRF, req.Count)
	if err != nil {
		// 未通/不可用：把原始回显一并给出（客户端要能照着排查）
		writeError(w, http.StatusBadGateway, "PING_FAILED", err.Error(), detailOf(out))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"output": out})
}

// handleTraceroute POST /diagnostics/traceroute：路径跟踪（与 CLI `traceroute` 同源）。
func (s *Server) handleTraceroute(w http.ResponseWriter, r *http.Request) {
	if s.diag == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "诊断模块未接入", nil)
		return
	}
	var req tracerouteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Host) == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "需要 host", nil)
		return
	}
	out, err := s.diag.Traceroute(r.Context(), req.Host, req.VRF)
	if err != nil {
		writeError(w, http.StatusBadGateway, "TRACEROUTE_FAILED", err.Error(), detailOf(out))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"output": out})
}

// handleClearInterfaceStats POST /interfaces:clear-statistics：清零接口统计计数
// （与 CLI `clear interfaces statistics [<ifname>]` 同源；缺省为全部）。
func (s *Server) handleClearInterfaceStats(w http.ResponseWriter, r *http.Request) {
	if s.diag == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "诊断模块未接入", nil)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "请求体 JSON 不合法: "+err.Error(), nil)
			return
		}
	}
	if err := s.diag.ClearInterfaceStats(r.Context(), req.Name); err != nil {
		mapDiagError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// detailOf 把原始回显切成逐行 detail（客户端展示与排查都要用）。
func detailOf(out string) []ErrorDetail {
	var detail []ErrorDetail
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		detail = append(detail, ErrorDetail{Path: "output", Message: line})
	}
	if len(detail) == 0 {
		return nil
	}
	return detail
}

// mapDiagError 诊断类错误 → 响应（不可用 503、其余 502：诊断命令执行失败）。
func mapDiagError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		writeError(w, http.StatusBadGateway, "DIAG_TIMEOUT", err.Error(), nil)
		return
	}
	writeError(w, http.StatusBadGateway, "DIAG_FAILED", err.Error(), nil)
}
