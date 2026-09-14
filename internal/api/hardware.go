package api

// M5-5：硬件健康与阈值（FR-SYS-012）。
//
// GET /system/hardware            硬件健康（BMC/温度/风扇/电源/SMART，越限状态标注）
// GET /system/health/thresholds   告警阈值（committed 配置）
// PUT /system/health/thresholds   设置告警阈值（candidate + 提交）

import (
	"context"
	"net/http"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/system"
)

// HardwareRuntime 硬件健康采集（*system.HardwareProvider 经适配注入；nil = 503）。
type HardwareRuntime interface {
	Collect(ctx context.Context) system.HardwareHealth
	Evaluate(hh *system.HardwareHealth, cpuTemp, diskTemp, diskUsed int) []string
}

func (s *Server) requireHardware(w http.ResponseWriter) bool {
	if s.hardware == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "硬件健康采集未接入", nil)
		return false
	}
	return true
}

// committedThresholds 取 committed 阈值（无配置时返回零值 = 不设阈值）。
func (s *Server) committedThresholds() model.HealthThresholds {
	if cfg, err := s.engine.Committed(); err == nil && cfg.System != nil && cfg.System.Health != nil {
		return *cfg.System.Health
	}
	return model.HealthThresholds{}
}

// handleGetHardware GET /api/v1/system/hardware
func (s *Server) handleGetHardware(w http.ResponseWriter, r *http.Request) {
	if !s.requireHardware(w) {
		return
	}
	hh := s.hardware.Collect(r.Context())
	th := s.committedThresholds()
	violations := s.hardware.Evaluate(&hh, th.CPUTempCelsius, th.DiskTempCelsius, th.DiskUsedPercent)
	writeJSON(w, http.StatusOK, map[string]any{
		"bmc_present": hh.BMCPresent, "sensors": hh.Sensors, "disks": hh.Disks,
		"root_used_percent": hh.RootUsedPercent, "thresholds": th, "violations": violations,
	})
}

// handleGetHealthThresholds GET /api/v1/system/health/thresholds
func (s *Server) handleGetHealthThresholds(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.committedThresholds())
}

// handlePutHealthThresholds PUT /api/v1/system/health/thresholds
func (s *Server) handlePutHealthThresholds(w http.ResponseWriter, r *http.Request) {
	var in model.HealthThresholds
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	s.mutateCandidate(w, r, http.StatusOK, func(cfg *model.Config) error {
		if cfg.System == nil {
			cfg.System = &model.SystemConfig{}
		}
		h := in
		cfg.System.Health = &h
		return nil
	})
}
