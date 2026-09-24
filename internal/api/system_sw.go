package api

// M5-7：软件升级/回退、电源操作与 NTP 同步（FR-OPS-001~003）。
//
// POST /system/software             安装 deb（本地路径或 URL，202）
// POST /system/software:rollback    回退上一版本（202）
// POST /system:reboot               重启整机（202）
// POST /system:shutdown             关机（202）
// POST /system/ntp:sync             立即同步时间（200）

import (
	"context"
	"fmt"
	"net/http"

	"github.com/xzjt/nfvis/internal/system"
)

// SoftwareRuntime 软件/电源能力（*system.SoftwareManager 经适配注入；nil = 503）。
type SoftwareRuntime interface {
	Add(ctx context.Context, pkg, sha256 string) (system.SoftwareResult, error)
	Rollback(ctx context.Context) (system.SoftwareResult, error)
	Reboot(ctx context.Context) error
	Shutdown(ctx context.Context) error
	NTPSync(ctx context.Context, servers []string) (string, error)
}

func (s *Server) requireSoftware(w http.ResponseWriter) bool {
	if s.software == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "软件升级模块未接入", nil)
		return false
	}
	return true
}

// handlePostSoftware POST /api/v1/system/software
func (s *Server) handlePostSoftware(w http.ResponseWriter, r *http.Request) {
	if !s.requireSoftware(w) {
		return
	}
	var in struct {
		Package string `json:"package"`
		SHA256  string `json:"sha256"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if in.Package == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "package 必填（本地路径或 https URL）", nil)
		return
	}
	user := "api"
	if info, ok := Identity(r); ok {
		user = info.User
	}
	// 决策 #150：软件升级是高危档动作——执行前写意图、执行后写结果（成功/失败都写）。
	// CLI `request system software add …` 走同一助手（同一 action 与意图文案）。
	var res system.SoftwareResult
	err := runHighRisk(s.engine, user, highRiskSoftwareAdd(in.Package, in.SHA256), func() (string, error) {
		var rerr error
		res, rerr = s.software.Add(r.Context(), in.Package, in.SHA256)
		if rerr != nil {
			return "", rerr
		}
		return fmt.Sprintf("升级完成：%s → %s（包 %s）", res.Previous, res.Version, res.Package), nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

// handlePostSoftwareRollback POST /api/v1/system/software:rollback
func (s *Server) handlePostSoftwareRollback(w http.ResponseWriter, r *http.Request) {
	if !s.requireSoftware(w) {
		return
	}
	user := "api"
	if info, ok := Identity(r); ok {
		user = info.User
	}
	// 决策 #150：软件回退同升级（两条记录；CLI `request system software rollback` 走同一助手）。
	var res system.SoftwareResult
	err := runHighRisk(s.engine, user, highRiskSoftwareRollback(), func() (string, error) {
		var rerr error
		res, rerr = s.software.Rollback(r.Context())
		if rerr != nil {
			return "", rerr
		}
		return fmt.Sprintf("回退完成：%s → %s", res.Previous, res.Version), nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

// handleSystemPower POST /api/v1/system:reboot|shutdown（FR-OPS-003）。
func (s *Server) handleSystemPower(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.requireSoftware(w) {
			return
		}
		user := "api"
		if info, ok := Identity(r); ok {
			user = info.User
		}
		var err error
		switch action {
		case "reboot":
			err = s.software.Reboot(r.Context())
		case "shutdown":
			err = s.software.Shutdown(r.Context())
		}
		if err != nil {
			s.engine.Audit(user, "system."+action, action+" 失败: "+err.Error(), "failure")
			writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
			return
		}
		s.engine.Audit(user, "system."+action, "执行 "+action, "success")
		writeJSON(w, http.StatusAccepted, map[string]string{"status": action + " issued"})
	}
}

// handlePostNTPSync POST /api/v1/system/ntp:sync
func (s *Server) handlePostNTPSync(w http.ResponseWriter, r *http.Request) {
	if !s.requireSoftware(w) {
		return
	}
	// 取 committed 的 ntp server 列表（未配置则用系统默认源）
	var servers []string
	if cfg, err := s.engine.Committed(); err == nil && cfg.System != nil {
		for _, n := range cfg.System.Ntp {
			if n.Server != "" {
				servers = append(servers, n.Server)
			}
		}
	}
	out, err := s.software.NTPSync(r.Context(), servers)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "synced", "detail": out})
}
