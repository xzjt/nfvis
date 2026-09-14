package api

// M4-12：CLI 计算/容器/镜像命令接入单测。
//
// 覆盖三层：
//  1. show 族：commit 配置后 `show virtual-machine-functions|container-functions|images|resource-pools`
//     必须渲染实体值（无 %% 占位），注入运行态接口后合并 state/快照/引用计数；
//  2. request 族：start/stop/restart/console/snapshot/log/upload 落到注入的运行态接口，
//     并断言审计落库（FR-OPS-031）；delete 需交互确认（未确认只问不做，--yes 才执行并提交）。
//  3. 动态候选：DynImages 读镜像仓库运行态（附录 A #51）。

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/images"
)

// ---------- 运行态 fake ----------

// fakeCLIContainer 容器运行态（ContainerRuntime）。
type fakeCLIContainer struct {
	states  map[string]string
	actions []string
	logs    string
	logTail int
	err     error
}

func newFakeCLIContainer() *fakeCLIContainer {
	return &fakeCLIContainer{states: map[string]string{}}
}

func (f *fakeCLIContainer) StartContainer(_ context.Context, name string) error {
	f.actions = append(f.actions, "start:"+name)
	f.states[name] = "running"
	return f.err
}
func (f *fakeCLIContainer) StopContainer(_ context.Context, name string) error {
	f.actions = append(f.actions, "stop:"+name)
	f.states[name] = "exited"
	return f.err
}
func (f *fakeCLIContainer) RestartContainer(_ context.Context, name string) error {
	f.actions = append(f.actions, "restart:"+name)
	f.states[name] = "running"
	return f.err
}
func (f *fakeCLIContainer) ContainerState(_ context.Context, name string) (string, error) {
	if s, ok := f.states[name]; ok {
		return s, nil
	}
	return "absent", nil
}
func (f *fakeCLIContainer) ContainerLogs(_ context.Context, name string, tail int) (string, error) {
	f.logTail = tail
	return f.logs, f.err
}

// ---------- show 族 ----------

func TestCLIShowVMsListAndDetail(t *testing.T) {
	x, _ := newCLIKit(t)
	seedVMConfig(t, x)
	vmRT := newFakeVM()
	vmRT.states["fw-vm"] = "running"
	x.setComputeRuntime(vmRT, nil, nil, nil, nil)

	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-machine-functions")
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("列表不应报错: %s", res.Output)
	}
	if !strings.Contains(res.Output, "fw-vm") || !strings.Contains(res.Output, "running") {
		t.Fatalf("列表应含 VM 与运行态 state:\n%s", res.Output)
	}

	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-machine-functions fw-vm detail")
	if strings.Contains(res.Output, "%%") || !strings.Contains(res.Output, "fw-vm") {
		t.Fatalf("detail 应渲染 VM 配置:\n%s", res.Output)
	}
}

func TestCLIShowVMInterfacesAndSnapshots(t *testing.T) {
	x, _ := newCLIKit(t)
	seedVMConfig(t, x)
	snaps := &fakeSnapshots{rows: []SnapshotRow{{Name: "snap1", Description: "d"}}}
	x.setComputeRuntime(newFakeVM(), nil, snaps, nil, nil)

	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-machine-functions fw-vm interfaces")
	if strings.Contains(res.Output, "%%") || !strings.Contains(res.Output, "eth0") {
		t.Fatalf("interfaces 应含 vNIC:\n%s", res.Output)
	}
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-machine-functions fw-vm snapshots")
	if strings.Contains(res.Output, "%%") || !strings.Contains(res.Output, "snap1") {
		t.Fatalf("snapshots 应含快照:\n%s", res.Output)
	}
}

func TestCLIShowVMStatisticsDegradesExplicitly(t *testing.T) {
	x, _ := newCLIKit(t)
	seedVMConfig(t, x)
	x.setComputeRuntime(newFakeVM(), nil, nil, nil, nil)
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-machine-functions fw-vm statistics")
	if !strings.Contains(res.Output, "统计不可用") {
		t.Fatalf("未接入 state 应明确报不可用: %s", res.Output)
	}
}

func TestCLIShowContainersImagesPools(t *testing.T) {
	x, _ := newCLIKit(t)
	seedContainerConfig(t, x)

	ctRT := newFakeCLIContainer()
	ctRT.states["sbc-ct1"] = "running"
	store := newCLIImagesStore(t)
	writeImageIndex(t, store, "base.qcow2")

	x.setComputeRuntime(nil, nil, nil, ctRT, store)

	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show container-functions")
	if strings.Contains(res.Output, "%%") || !strings.Contains(res.Output, "sbc-ct1") ||
		!strings.Contains(res.Output, "running") {
		t.Fatalf("容器列表应含容器与 state:\n%s", res.Output)
	}
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show images")
	if strings.Contains(res.Output, "%%") || !strings.Contains(res.Output, "base.qcow2") {
		t.Fatalf("镜像列表应含镜像:\n%s", res.Output)
	}
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show resource-pools")
	if strings.Contains(res.Output, "%%") || !strings.Contains(res.Output, "Hugepages") {
		t.Fatalf("resource-pools 应渲染池视图:\n%s", res.Output)
	}
}

// ---------- request 族 ----------

func TestCLIRequestVMLifecycleAudits(t *testing.T) {
	x, engine := newCLIKit(t)
	seedVMConfig(t, x)
	vmRT := newFakeVM()
	x.setComputeRuntime(vmRT, nil, nil, nil, nil)

	for _, act := range []string{"start", "stop", "restart"} {
		res := x.Execute("admin", aaa.ClassSuperUser, "ssh",
			"request virtual-machine-functions fw-vm "+act)
		if strings.Contains(res.Output, "%%") {
			t.Fatalf("%s 应成功: %s", act, res.Output)
		}
	}
	if len(vmRT.actions) != 3 {
		t.Fatalf("应调用 3 次生命周期动作: %v", vmRT.actions)
	}
	for _, act := range []string{"vm.start", "vm.stop", "vm.restart"} {
		if !auditHas(t, engine, act) {
			t.Fatalf("动作 %s 应入审计（FR-OPS-031）", act)
		}
	}
}

func TestCLIRequestVMSnapshotActions(t *testing.T) {
	x, engine := newCLIKit(t)
	seedVMConfig(t, x)
	snaps := &fakeSnapshots{}
	x.setComputeRuntime(newFakeVM(), nil, snaps, nil, nil)

	res := x.Execute("admin", aaa.ClassSuperUser, "ssh",
		"request virtual-machine-functions fw-vm snapshot create name snap1")
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("快照创建应成功: %s", res.Output)
	}
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh",
		"request virtual-machine-functions fw-vm snapshot rollback name snap1")
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("快照回滚应成功: %s", res.Output)
	}
	if !auditHas(t, engine, "vm.snapshot.create") || !auditHas(t, engine, "vm.snapshot.rollback") {
		t.Fatal("快照动作应入审计")
	}
}

func TestCLIRequestVMDeleteRequiresConfirm(t *testing.T) {
	x, engine := newCLIKit(t)
	seedVMConfig(t, x)
	x.setComputeRuntime(newFakeVM(), nil, nil, nil, nil)

	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request virtual-machine-functions fw-vm delete")
	if !strings.Contains(res.Output, "[yes,no]") {
		t.Fatalf("删除应要求交互确认: %s", res.Output)
	}
	cfg, err := engine.Committed()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findVM(cfg, "fw-vm"); !ok {
		t.Fatal("未确认时不应删除")
	}

	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "request virtual-machine-functions fw-vm delete --yes")
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("确认后删除应成功: %s", res.Output)
	}
	cfg, err = engine.Committed()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findVM(cfg, "fw-vm"); ok {
		t.Fatal("确认后应删除 VM")
	}
	if !auditHas(t, engine, "vm.delete") {
		t.Fatal("删除应入审计")
	}
}

func TestCLIRequestVMDeleteRemovesSwitchPortRefs(t *testing.T) {
	x, engine := newCLIKit(t)
	// 建交换机端口引用该 VM 的 vNIC：删除 VM 时必须一并摘除（否则 FR-CFG-002 校验拒绝提交）
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set resource-pools hugepages page-size 1G count 8",
		"set resource-pools cpu isolated-cores 4-7",
		"set virtual-switches vs-a type l2",
		"set virtual-machine-functions fw-vm image base.qcow2",
		"set virtual-machine-functions fw-vm vcpu count 1",
		"set virtual-machine-functions fw-vm memory size-mb 512",
		"set virtual-machine-functions fw-vm interfaces eth0 type vhost-user",
		"set virtual-machine-functions fw-vm interfaces eth0 virtual-switch vs-a",
		"set virtual-switches vs-a ports 1 vnf fw-vm interface eth0",
		"commit",
		"exit",
	)
	x.setComputeRuntime(newFakeVM(), nil, nil, nil, nil)
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request virtual-machine-functions fw-vm delete --yes")
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("删除应成功（端口引用已同步摘除）: %s", res.Output)
	}
	cfg, err := engine.Committed()
	if err != nil {
		t.Fatal(err)
	}
	for _, vs := range cfg.VirtualSwitches {
		for _, p := range vs.Ports {
			if p.Vnf == "fw-vm" {
				t.Fatalf("交换机端口仍引用已删 VM: %+v", p)
			}
		}
	}
}

func TestCLIRequestContainerLifecycleAndLog(t *testing.T) {
	x, engine := newCLIKit(t)
	seedContainerConfig(t, x)
	ctRT := newFakeCLIContainer()
	ctRT.logs = "hello from container"
	x.setComputeRuntime(nil, nil, nil, ctRT, nil)

	res := x.Execute("admin", aaa.ClassSuperUser, "ssh",
		"request container-functions sbc-ct1 log last 50")
	if strings.Contains(res.Output, "%%") || !strings.Contains(res.Output, "hello from container") {
		t.Fatalf("容器日志应返回内容: %s", res.Output)
	}
	if ctRT.logTail != 50 {
		t.Fatalf("last 应传 50，实际 %d", ctRT.logTail)
	}
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "request container-functions sbc-ct1 restart")
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("容器重启应成功: %s", res.Output)
	}
	if !auditHas(t, engine, "container.restart") {
		t.Fatal("容器动作应入审计")
	}
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "request container-functions sbc-ct1 delete")
	if !strings.Contains(res.Output, "[yes,no]") {
		t.Fatalf("容器删除应要求确认: %s", res.Output)
	}
}

func TestCLIRequestImagesUploadAndDelete(t *testing.T) {
	x, engine := newCLIKit(t)
	store := newCLIImagesStore(t)
	x.setComputeRuntime(nil, nil, nil, nil, store)

	src := filepath.Join(store.Config().IncomingDir, "base.qcow2")
	if err := os.WriteFile(src, []byte("qcow2data"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh",
		"request images upload name base.qcow2 type vm-image file "+src)
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("upload 应成功: %s", res.Output)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("导入后 incoming 源文件应清理")
	}
	if !auditHas(t, engine, "images.upload") {
		t.Fatal("upload 应入审计")
	}

	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "request images delete name base.qcow2")
	if !strings.Contains(res.Output, "[yes,no]") {
		t.Fatalf("镜像删除应要求确认: %s", res.Output)
	}
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "request images delete name base.qcow2 --yes")
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("确认后删除应成功: %s", res.Output)
	}
	if _, ok := store.Get("base.qcow2"); ok {
		t.Fatal("确认后镜像应删除")
	}
}

func TestCLIRequestVMConsoleIssuesTicket(t *testing.T) {
	x, _ := newCLIKit(t)
	seedVMConfig(t, x)
	x.setComputeRuntime(newFakeVM(), &fakeConsoleRuntime{stream: newFakeConsoleStream("")}, nil, nil, nil)
	x.issueConsole = func(vm, _ string) (string, int, error) {
		return APIPrefix + "/virtual-machine-functions/" + vm + "/console/ws?ticket=tok123", 60, nil
	}
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request virtual-machine-functions fw-vm console")
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("console 应签发凭证: %s", res.Output)
	}
	if res.Console == nil || res.Console.VM != "fw-vm" || !strings.Contains(res.Console.WSURL, "ticket=tok123") {
		t.Fatalf("应回传 console 接管请求: %+v", res.Console)
	}
}

// ---------- 运行态缺失时的降级 ----------

func TestCLIComputeUnavailableDegrades(t *testing.T) {
	x, _ := newCLIKit(t)
	seedVMConfig(t, x)
	// 未注入任何运行态：show 仍可用（state 显示 "-"），request 动作明确报不可用
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-machine-functions")
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("未装配运行态时列表仍应可用: %s", res.Output)
	}
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "request virtual-machine-functions fw-vm start")
	if !strings.Contains(res.Output, "未接入") {
		t.Fatalf("未装配时应明确报不可用: %s", res.Output)
	}
}

// ---------- 动态候选 ----------

func TestCLIDynImagesCandidates(t *testing.T) {
	store := newCLIImagesStore(t)
	writeImageIndex(t, store, "img-a.qcow2")
	writeImageIndex(t, store, "img-b.qcow2")

	// 服务端候选来源为镜像仓库运行态（附录 A #51）；经既有候选端点验证。
	ts := newTestServerOpts(t, Options{Images: store})
	token := loginAdmin(t, ts)
	_, data := getWithToken(t, ts.URL+APIPrefix+"/cli/candidates?kind=images", token)
	if !strings.Contains(string(data), "img-a.qcow2") || !strings.Contains(string(data), "img-b.qcow2") {
		t.Fatalf("DynImages 候选应含镜像仓库清单: %s", data)
	}
}

// ---------- 测试辅助 ----------

// TestCLIExecuteHTTPCarriesConsole 经 HTTP /cli/execute 时 console 接管请求必须回传到响应
// （此前 cliExecuteResponse 漏了该字段，真机冒烟才发现——回归守护）。
func TestCLIExecuteHTTPCarriesConsole(t *testing.T) {
	ts := newTestServerOpts(t, Options{
		VMConsole: &fakeConsoleRuntime{stream: newFakeConsoleStream("")},
	})
	token := loginAdmin(t, ts)
	seedVMPool(t, ts, token)
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		vmBody("fw-vm"), map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("建 VM: %d %s", status, data)
	}
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/cli/execute", token,
		map[string]any{"line": "request virtual-machine-functions fw-vm console", "source": "ssh"}, nil)
	if status != http.StatusOK {
		t.Fatalf("cli/execute: %d %s", status, data)
	}
	var resp struct {
		Console *ConsoleRequest `json:"console"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if resp.Console == nil || resp.Console.VM != "fw-vm" || !strings.Contains(resp.Console.WSURL, "/console/ws") {
		t.Fatalf("HTTP 响应应携带 console 接管请求: %s", data)
	}
}

// seedVMConfig 经 CLI 语句建好资源池 + 1 个 VM 并 commit。
func seedVMConfig(t *testing.T, x *cliExecutor) {
	t.Helper()
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set resource-pools hugepages page-size 1G count 8",
		"set resource-pools cpu isolated-cores 4-7",
		"set virtual-machine-functions fw-vm image base.qcow2",
		"set virtual-machine-functions fw-vm vcpu count 1",
		"set virtual-machine-functions fw-vm memory size-mb 512",
		"set virtual-machine-functions fw-vm serial console enable",
		"set virtual-machine-functions fw-vm interfaces eth0 type vhost-user",
		"commit",
		"exit",
	)
}

// seedContainerConfig 建好 1 个容器并 commit。
func seedContainerConfig(t *testing.T, x *cliExecutor) {
	t.Helper()
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set container-functions sbc-ct1 image alpine:3.20",
		"set container-functions sbc-ct1 vcpu count 2",
		"set container-functions sbc-ct1 memory size-mb 256",
		"commit",
		"exit",
	)
}

// auditHas 断言审计表中存在指定动作（FR-OPS-031）。
func auditHas(t *testing.T, engine *config.Engine, action string) bool {
	t.Helper()
	entries, err := engine.AuditTrail(200, 0)
	if err != nil {
		t.Fatalf("读取审计: %v", err)
	}
	for _, e := range entries {
		if e.Action == action {
			return true
		}
	}
	return false
}

// newCLIImagesStore 建临时镜像仓库（incoming 目录在仓库目录下）。
func newCLIImagesStore(t *testing.T) *images.Store {
	t.Helper()
	dir := t.TempDir()
	inc := filepath.Join(dir, "incoming")
	if err := os.MkdirAll(inc, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := images.Open(images.Config{Dir: filepath.Join(dir, "images"), IncomingDir: inc})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// writeImageIndex 直接向仓库索引登记一个 vm-image（供 show/candidates 断言）。
func writeImageIndex(t *testing.T, s *images.Store, name string) {
	t.Helper()
	inc := filepath.Join(s.Config().IncomingDir, name)
	if err := os.WriteFile(inc, []byte("img"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportIncoming(name, images.TypeVM, inc, ""); err != nil {
		t.Fatal(err)
	}
}

// TestCopyFileExportsSecretsAs0600 覆盖决策 #77（安全）：
// copyFile 当前唯一调用方是 `request system configuration backup to <path>` 的导出，
// 而备份归档内含 password_hash（决策 #70 已认定口令哈希不得外泄）。
// 原先用 os.Create 落 0644，使导出件**比自动命名的归档（0600）更宽松** →
// 本地任意用户可读到口令哈希。本测试锁定 0600。
func TestCopyFileExportsSecretsAs0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows 不实现 POSIX 权限位（Go 一律报 0666），本断言只在类 Unix 上有意义。
		t.Skip("跳过：Windows 无 POSIX 权限位")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.json")
	dst := filepath.Join(dir, "exported.json")
	body := []byte(`{"config":{"login":{"users":[{"password_hash":"pbkdf2$sha256$..."}]}}}`)
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("备份导出件权限应为 0600，实际 %04o（归档含口令哈希，不得对他人可读）", perm)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("内容应完整复制:\n got=%q\nwant=%q", got, body)
	}
}
