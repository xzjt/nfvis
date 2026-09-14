package metrics

import (
	"strings"
	"testing"
)

func TestRenderPrometheusFormat(t *testing.T) {
	out := Render([]Sample{
		{Name: "nfvis_vnf_running", Help: "运行中的 VNF 数", Type: "gauge", Labels: map[string]string{"kind": "vm"}, Value: 2},
		{Name: "nfvis_vnf_running", Help: "运行中的 VNF 数", Type: "gauge", Labels: map[string]string{"kind": "container"}, Value: 0},
		{Name: "nfvis_vpp_interface_rx_packets", Help: "接口收包数", Type: "counter", Labels: map[string]string{"interface": "ens192"}, Value: 12345},
		{Name: "nfvis_system_cpu_utilization_ratio", Type: "gauge", Value: 0.125},
	})
	if n := strings.Count(out, "# HELP nfvis_vnf_running"); n != 1 {
		t.Fatalf("HELP 应只出现一次，实际 %d\n%s", n, out)
	}
	if n := strings.Count(out, "# TYPE nfvis_vnf_running gauge"); n != 1 {
		t.Fatalf("TYPE 应只出现一次，实际 %d\n%s", n, out)
	}
	want := []string{
		`nfvis_vnf_running{kind="container"} 0`,
		`nfvis_vnf_running{kind="vm"} 2`,
		`nfvis_vpp_interface_rx_packets{interface="ens192"} 12345`,
		`nfvis_system_cpu_utilization_ratio 0.125`,
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Fatalf("缺少样本 %q\n%s", w, out)
		}
	}
}

// 标签值内的双引号按 Prometheus 规则转义。
func TestRenderEscapesLabels(t *testing.T) {
	quote := string(rune(34)) // 双引号
	slash := string(rune(92)) // 反斜杠
	val := "a" + slash + slash + "b" + quote + "c"
	out := Render([]Sample{{Name: "nfvis_x", Labels: map[string]string{"note": val}, Value: 1}})
	want := "nfvis_x{note=" + quote + "a" + slash + slash + slash + slash + "b" + slash + quote + "c" + quote + "} 1"
	if !strings.Contains(out, want) {
		t.Fatalf("标签转义不符，期望含 %s，实际:\n%s", want, out)
	}
}

func TestFormatValueInteger(t *testing.T) {
	out := Render([]Sample{{Name: "nfvis_y", Value: 42}})
	if !strings.Contains(out, "nfvis_y 42\n") {
		t.Fatalf("整数应无小数点:\n%s", out)
	}
}
