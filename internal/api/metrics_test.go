package api

// 决策 #355（收口 v2 待做 三.7）：/metrics 运行态可用性如实 + 逐对象序列。
//
// 覆盖：可用性三态（provider 未接入 / 查询出错 / 正常）、逐对象 up 的 0/1 与
// 「不可用不发出」、聚合计数回归（不可用时行为不变——查询失败仍不计入 running，
// 但 availability=0 使外部可判定计数不可信）、渲染格式（HELP/TYPE + 带标签行）。

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/metrics"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// metricsFixtureConfig 可提交的最小配置：2 台 VM（vcpu 2/4）+ 1 个容器（vcpu 3）。
func metricsFixtureConfig() model.Config {
	return model.Config{
		ResourcePools: &model.ResourcePool{
			Hugepages: []model.HPool{{PageSize: "1G", Count: 8}},
			CPU:       &model.CPUSetup{IsolatedCores: []int{4, 5, 6, 7, 8, 9, 10, 11, 12, 13}},
		},
		VirtualMachineFunctions: []model.VMFunction{
			{Name: "vma", Image: "base.qcow2", VCPU: model.VMCpu{Count: 2}, Memory: model.VMMemory{SizeMB: 512}},
			{Name: "vmb", Image: "base.qcow2", VCPU: model.VMCpu{Count: 4}, Memory: model.VMMemory{SizeMB: 512}},
		},
		ContainerFunctions: []model.ContainerFunction{
			{Name: "cta", Image: "alpine:3.20", VCPU: 3, MemoryMB: 256},
		},
	}
}

// newMetricsServer 建真实引擎并提交 cfg，装配运行态替身（nil = 编排未接入）。
func newMetricsServer(t *testing.T, cfg model.Config, vm VMRuntime, ct ContainerRuntime) *Server {
	t.Helper()
	store, err := config.OpenStore(filepath.Join(t.TempDir(), "nfvis.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	engine, err := config.NewEngine(store, orchestrator.NewNoopApplier(), config.Options{})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(engine.Close)
	sess := config.Session{User: "system", Source: "console"}
	if err := engine.Edit(sess); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if err := engine.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	// 夹具 fixture 不含账号：豁免「至少保留一个 super-user」兜底（与指标无关）。
	if _, err := engine.Commit(context.Background(), sess, config.CommitOpts{AllowNoSuperUser: true}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	_ = engine.Release(sess)
	return &Server{engine: engine, vm: vm, containers: ct}
}

// errVMStatus 在 fakeVM 基础上给指定 VM 注入查询错误。
type errVMStatus struct {
	fakeVM
	errs map[string]error
}

func (f *errVMStatus) VMState(ctx context.Context, name string) (string, error) {
	if err := f.errs[name]; err != nil {
		return "", err
	}
	return f.fakeVM.VMState(ctx, name)
}

// errCTStatus 在 fakeCLIContainer 基础上给指定容器注入查询错误。
type errCTStatus struct {
	fakeCLIContainer
	errs map[string]error
}

func (f *errCTStatus) ContainerState(ctx context.Context, name string) (string, error) {
	if err := f.errs[name]; err != nil {
		return "", err
	}
	return f.fakeCLIContainer.ContainerState(ctx, name)
}

// metricSample 按 name+labels 精确查样本（ok=false 表示未发出）。
func metricSample(samples []metrics.Sample, name string, labels map[string]string) (metrics.Sample, bool) {
	for _, s := range samples {
		if s.Name != name || len(s.Labels) != len(labels) {
			continue
		}
		match := true
		for k, v := range labels {
			if s.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			return s, true
		}
	}
	return metrics.Sample{}, false
}

// assertMetric 断言样本存在且取值等于 want。
func assertMetric(t *testing.T, samples []metrics.Sample, name string, labels map[string]string, want float64) {
	t.Helper()
	s, ok := metricSample(samples, name, labels)
	if !ok {
		t.Fatalf("缺少样本 %s%v", name, labels)
	}
	if s.Value != want {
		t.Fatalf("%s%v = %v，期望 %v", name, labels, s.Value, want)
	}
}

func metricsVM() *errVMStatus {
	f := &errVMStatus{fakeVM: *newFakeVM()}
	f.states["vma"] = orchestrator.VMStateRunning
	f.states["vmb"] = orchestrator.VMStateShutoff
	return f
}

func metricsCT() *errCTStatus {
	f := &errCTStatus{fakeCLIContainer: *newFakeCLIContainer()}
	f.states["cta"] = "running"
	return f
}

// 可用性三态：正常 ⇒ 1；provider 未接入 ⇒ 0；任一查询出错 ⇒ 0；无声明对象也算 1。
func TestVNFMetricsRuntimeAvailable(t *testing.T) {
	// 正常（全部查询成功，含「对象不存在」的正常返回）。
	s := newMetricsServer(t, metricsFixtureConfig(), metricsVM(), metricsCT())
	samples := s.vnfMetrics(context.Background())
	assertMetric(t, samples, "nfvis_vnf_runtime_available", map[string]string{"kind": "vm"}, 1)
	assertMetric(t, samples, "nfvis_vnf_runtime_available", map[string]string{"kind": "container"}, 1)

	// provider 未接入：两类各自独立置 0。
	s = newMetricsServer(t, metricsFixtureConfig(), nil, nil)
	samples = s.vnfMetrics(context.Background())
	assertMetric(t, samples, "nfvis_vnf_runtime_available", map[string]string{"kind": "vm"}, 0)
	assertMetric(t, samples, "nfvis_vnf_runtime_available", map[string]string{"kind": "container"}, 0)

	// 查询出错（任一对象）⇒ 0。
	vmErr := metricsVM()
	vmErr.errs = map[string]error{"vmb": errors.New("libvirt 连接失败")}
	ctErr := metricsCT()
	ctErr.errs = map[string]error{"cta": errors.New("docker 不可用")}
	s = newMetricsServer(t, metricsFixtureConfig(), vmErr, ctErr)
	samples = s.vnfMetrics(context.Background())
	assertMetric(t, samples, "nfvis_vnf_runtime_available", map[string]string{"kind": "vm"}, 0)
	assertMetric(t, samples, "nfvis_vnf_runtime_available", map[string]string{"kind": "container"}, 0)

	// 无声明对象：可查询即可用 ⇒ 1（两类）。
	s = newMetricsServer(t, model.Config{}, metricsVM(), metricsCT())
	samples = s.vnfMetrics(context.Background())
	assertMetric(t, samples, "nfvis_vnf_runtime_available", map[string]string{"kind": "vm"}, 1)
	assertMetric(t, samples, "nfvis_vnf_runtime_available", map[string]string{"kind": "container"}, 1)
}

// 逐对象 up：运行中 ⇒ 1、已知非运行 ⇒ 0、查询出错/未接入 ⇒ 不发该对象。
func TestVNFMetricsPerObjectUp(t *testing.T) {
	s := newMetricsServer(t, metricsFixtureConfig(), metricsVM(), metricsCT())
	samples := s.vnfMetrics(context.Background())
	assertMetric(t, samples, "nfvis_vnf_up", map[string]string{"kind": "vm", "name": "vma"}, 1)
	assertMetric(t, samples, "nfvis_vnf_up", map[string]string{"kind": "vm", "name": "vmb"}, 0)
	assertMetric(t, samples, "nfvis_vnf_up", map[string]string{"kind": "container", "name": "cta"}, 1)

	// 容器「已知非运行」（absent，查询成功）⇒ 0。
	s = newMetricsServer(t, metricsFixtureConfig(), metricsVM(), newFakeCLIContainer())
	samples = s.vnfMetrics(context.Background())
	assertMetric(t, samples, "nfvis_vnf_up", map[string]string{"kind": "container", "name": "cta"}, 0)

	// 查询出错：该对象不发（宁缺不谎报 0），其余对象照发、可用性置 0。
	vmErr := metricsVM()
	vmErr.errs = map[string]error{"vmb": errors.New("查询超时")}
	ctErr := metricsCT()
	ctErr.errs = map[string]error{"cta": errors.New("docker 查询失败")}
	s = newMetricsServer(t, metricsFixtureConfig(), vmErr, ctErr)
	samples = s.vnfMetrics(context.Background())
	if _, ok := metricSample(samples, "nfvis_vnf_up", map[string]string{"kind": "vm", "name": "vmb"}); ok {
		t.Fatalf("查询出错的 VM 不应发出逐对象序列")
	}
	if _, ok := metricSample(samples, "nfvis_vnf_up", map[string]string{"kind": "container", "name": "cta"}); ok {
		t.Fatalf("查询出错的容器不应发出逐对象序列")
	}
	assertMetric(t, samples, "nfvis_vnf_up", map[string]string{"kind": "vm", "name": "vma"}, 1)
	assertMetric(t, samples, "nfvis_vnf_runtime_available", map[string]string{"kind": "vm"}, 0)
	assertMetric(t, samples, "nfvis_vnf_runtime_available", map[string]string{"kind": "container"}, 0)

	// provider 未接入：逐对象序列一条也不发。
	s = newMetricsServer(t, metricsFixtureConfig(), nil, nil)
	samples = s.vnfMetrics(context.Background())
	for _, sample := range samples {
		if sample.Name == "nfvis_vnf_up" {
			t.Fatalf("provider 未接入不应发出逐对象序列: %+v", sample)
		}
	}
}

// 聚合回归：正常时计数不变；不可用时既有行为不变（失败不计入 running），由 availability=0 说明。
func TestVNFMetricsAggregateRegression(t *testing.T) {
	vmRT := metricsVM()
	vmRT.states["vmb"] = orchestrator.VMStateRunning
	s := newMetricsServer(t, metricsFixtureConfig(), vmRT, metricsCT())
	samples := s.vnfMetrics(context.Background())
	assertMetric(t, samples, "nfvis_vnf_running", map[string]string{"kind": "vm"}, 2)
	assertMetric(t, samples, "nfvis_vnf_running", map[string]string{"kind": "container"}, 1)
	assertMetric(t, samples, "nfvis_vnf_vcpu_allocated", map[string]string{"kind": "vm"}, 6)
	assertMetric(t, samples, "nfvis_vnf_vcpu_allocated", map[string]string{"kind": "container"}, 3)

	// 查询失败：失败对象不计入 running、vCPU 仍按配置计入（既有行为），可用性如实为 0。
	vmErr := metricsVM()
	vmErr.errs = map[string]error{"vmb": errors.New("查询失败")}
	s = newMetricsServer(t, metricsFixtureConfig(), vmErr, metricsCT())
	samples = s.vnfMetrics(context.Background())
	assertMetric(t, samples, "nfvis_vnf_runtime_available", map[string]string{"kind": "vm"}, 0)
	assertMetric(t, samples, "nfvis_vnf_running", map[string]string{"kind": "vm"}, 1)
	assertMetric(t, samples, "nfvis_vnf_vcpu_allocated", map[string]string{"kind": "vm"}, 6)

	// 未接入：聚合仍发出且计 0（既有行为），availability=0 使「0 不可信」可判定。
	s = newMetricsServer(t, metricsFixtureConfig(), nil, nil)
	samples = s.vnfMetrics(context.Background())
	assertMetric(t, samples, "nfvis_vnf_runtime_available", map[string]string{"kind": "vm"}, 0)
	assertMetric(t, samples, "nfvis_vnf_running", map[string]string{"kind": "vm"}, 0)
	assertMetric(t, samples, "nfvis_vnf_vcpu_allocated", map[string]string{"kind": "vm"}, 0)
}

// 渲染层面：两条新序列按 Prometheus 文本格式出现（HELP/TYPE + 带标签行，同名只一次）。
func TestVNFMetricsRenderNewSeries(t *testing.T) {
	s := newMetricsServer(t, metricsFixtureConfig(), metricsVM(), newFakeCLIContainer())
	out := metrics.Render(s.vnfMetrics(context.Background()))
	for _, want := range []string{
		"# HELP nfvis_vnf_runtime_available ",
		"# TYPE nfvis_vnf_runtime_available gauge",
		`nfvis_vnf_runtime_available{kind="vm"} 1`,
		`nfvis_vnf_runtime_available{kind="container"} 1`,
		"# HELP nfvis_vnf_up ",
		"# TYPE nfvis_vnf_up gauge",
		`nfvis_vnf_up{kind="vm",name="vma"} 1`,
		`nfvis_vnf_up{kind="vm",name="vmb"} 0`,
		`nfvis_vnf_up{kind="container",name="cta"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("渲染缺少 %q:\n%s", want, out)
		}
	}
	for _, name := range []string{"nfvis_vnf_runtime_available", "nfvis_vnf_up"} {
		if n := strings.Count(out, "# TYPE "+name+" gauge"); n != 1 {
			t.Fatalf("TYPE %s 应只出现一次，实际 %d:\n%s", name, n, out)
		}
	}
}
