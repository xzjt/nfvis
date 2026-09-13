package api

// M5-2：Prometheus 指标端点（FR-SYS-005）。GET /metrics，无鉴权（契约 security: []），
// 文本格式 version=0.0.4（手写渲染，不引入外部客户端库）。
//
// 指标来源：主机（/proc、statfs，见 internal/metrics）、VPP 运行态（state.State）、
// 配置计数（事务引擎 committed）、VNF 状态（VMRuntime/ContainerRuntime）、
// 活动告警计数（AlarmRuntime）。

import (
	"context"
	"net/http"
	"time"

	"github.com/xzjt/nfvis/internal/metrics"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	var samples []metrics.Sample
	samples = append(samples, metrics.HostMetrics()...)
	samples = append(samples, s.vppMetrics(r.Context())...)
	samples = append(samples, s.configMetrics()...)
	samples = append(samples, s.vnfMetrics(r.Context())...)
	samples = append(samples, s.alarmMetrics()...)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(metrics.Render(samples)))
}

// vppMetrics VPP 数据面指标（未接入监控来源时省略）。
func (s *Server) vppMetrics(ctx context.Context) []metrics.Sample {
	if s.state == nil {
		return nil
	}
	var out []metrics.Sample
	if threads := s.state.Threads(ctx); len(threads) > 0 {
		out = append(out, metrics.Sample{Name: "nfvis_vpp_threads", Help: "VPP 线程数（main + workers）", Type: "gauge", Value: float64(len(threads))})
	}
	if mem, ok := s.state.Memory(ctx); ok {
		out = append(out,
			metrics.Sample{Name: "nfvis_vpp_memory_total_bytes", Help: "VPP 主堆总量", Type: "gauge", Value: float64(mem.Total)},
			metrics.Sample{Name: "nfvis_vpp_memory_used_bytes", Help: "VPP 主堆已用", Type: "gauge", Value: float64(mem.Used)},
			metrics.Sample{Name: "nfvis_vpp_memory_free_bytes", Help: "VPP 主堆空闲", Type: "gauge", Value: float64(mem.Free)},
		)
	}
	if bufs, ok := s.state.Buffers(ctx); ok {
		for _, p := range bufs.Pools {
			out = append(out, metrics.Sample{Name: "nfvis_vpp_buffer_pool_used", Help: "VPP buffer 池已用数量", Type: "gauge",
				Labels: map[string]string{"pool": p.Name}, Value: p.Used})
		}
	}
	// 接口计数：按配置接口逐口取（契约 Interface.statistics 同源）
	if s.engine != nil {
		if cfg, err := s.engine.Committed(); err == nil {
			for _, iface := range cfg.Interfaces {
				c, ok := s.state.InterfaceCounters(ctx, iface.Name)
				if !ok {
					continue
				}
				l := map[string]string{"interface": iface.Name}
				out = append(out,
					metrics.Sample{Name: "nfvis_vpp_interface_rx_packets", Help: "接口收包数", Type: "counter", Labels: l, Value: float64(c.RxPackets)},
					metrics.Sample{Name: "nfvis_vpp_interface_tx_packets", Help: "接口发包数", Type: "counter", Labels: l, Value: float64(c.TxPackets)},
					metrics.Sample{Name: "nfvis_vpp_interface_rx_bytes", Help: "接口收字节数", Type: "counter", Labels: l, Value: float64(c.RxBytes)},
					metrics.Sample{Name: "nfvis_vpp_interface_tx_bytes", Help: "接口发字节数", Type: "counter", Labels: l, Value: float64(c.TxBytes)},
					metrics.Sample{Name: "nfvis_vpp_interface_rx_errors", Help: "接口收包错误", Type: "counter", Labels: l, Value: float64(c.RxErrors)},
					metrics.Sample{Name: "nfvis_vpp_interface_tx_errors", Help: "接口发包错误", Type: "counter", Labels: l, Value: float64(c.TxErrors)},
					metrics.Sample{Name: "nfvis_vpp_interface_rx_drops", Help: "接口收包丢弃", Type: "counter", Labels: l, Value: float64(c.RxDrops)},
					metrics.Sample{Name: "nfvis_vpp_interface_tx_drops", Help: "接口发包丢弃", Type: "counter", Labels: l, Value: float64(c.TxDrops)},
				)
			}
		}
	}
	return out
}

// configMetrics 配置对象计数（BD 数 = 虚拟交换机数，FR-SYS-005）。
func (s *Server) configMetrics() []metrics.Sample {
	if s.engine == nil {
		return nil
	}
	cfg, err := s.engine.Committed()
	if err != nil {
		return nil
	}
	count := func(name, help string, v int) metrics.Sample {
		return metrics.Sample{Name: name, Help: help, Type: "gauge", Value: float64(v)}
	}
	return []metrics.Sample{
		count("nfvis_config_interfaces", "配置的物理接口数", len(cfg.Interfaces)),
		count("nfvis_config_virtual_switches", "虚拟交换机数（bridge domain）", len(cfg.VirtualSwitches)),
		count("nfvis_config_vrfs", "VRF 数", len(cfg.Vrfs)),
		count("nfvis_config_virtual_machine_functions", "配置的 VM VNF 数", len(cfg.VirtualMachineFunctions)),
		count("nfvis_config_container_functions", "配置的容器 VNF 数", len(cfg.ContainerFunctions)),
	}
}

// vnfMetrics VNF 运行态与 vCPU 分配。
func (s *Server) vnfMetrics(ctx context.Context) []metrics.Sample {
	if s.engine == nil {
		return nil
	}
	cfg, err := s.engine.Committed()
	if err != nil {
		return nil
	}
	var vmsRunning, ctsRunning, vcpuVM, vcpuCT float64
	if s.vm != nil {
		for _, vm := range cfg.VirtualMachineFunctions {
			vcpuVM += float64(vm.VCPU.Count)
			cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			st, err := s.vm.VMState(cctx, vm.Name)
			cancel()
			if err == nil && st == orchestrator.VMStateRunning {
				vmsRunning++
			}
		}
	}
	if s.containers != nil {
		for _, ct := range cfg.ContainerFunctions {
			vcpuCT += float64(ct.VCPU)
			cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			st, err := s.containers.ContainerState(cctx, ct.Name)
			cancel()
			if err == nil && st == orchestrator.CTStateRunning {
				ctsRunning++
			}
		}
	}
	out := []metrics.Sample{
		{Name: "nfvis_vnf_running", Help: "运行中的 VNF 数", Type: "gauge",
			Labels: map[string]string{"kind": "vm"}, Value: vmsRunning},
		{Name: "nfvis_vnf_running", Help: "运行中的 VNF 数", Type: "gauge",
			Labels: map[string]string{"kind": "container"}, Value: ctsRunning},
		{Name: "nfvis_vnf_vcpu_allocated", Help: "已分配的 vCPU 数（按配置）", Type: "gauge",
			Labels: map[string]string{"kind": "vm"}, Value: vcpuVM},
		{Name: "nfvis_vnf_vcpu_allocated", Help: "已分配的 vCPU 数（按配置）", Type: "gauge",
			Labels: map[string]string{"kind": "container"}, Value: vcpuCT},
	}
	return out
}

// alarmMetrics 活动告警计数（按严重级别）。
func (s *Server) alarmMetrics() []metrics.Sample {
	if s.alarms == nil {
		return nil
	}
	rows := s.alarms.List("active")
	count := map[string]float64{}
	for _, a := range rows {
		count[a.Severity]++
	}
	names := []string{"info", "warning", "error", "critical"}
	out := make([]metrics.Sample, 0, len(names))
	for _, sev := range names {
		out = append(out, metrics.Sample{Name: "nfvis_alarms_active", Help: "活动告警数（按严重级别）", Type: "gauge",
			Labels: map[string]string{"severity": sev}, Value: count[sev]})
	}
	return out
}
