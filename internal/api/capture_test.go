package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/orchestrator/network"
	"github.com/xzjt/nfvis/internal/system"
)

type fakeCapture struct {
	active   *CaptureSessionRow
	files    []CaptureFileRow
	startErr error
	last     string
	stopped  bool
	exported bool
	// pathFn 覆盖 Path 解析（缺省只认 .pcap 后缀、返回不存在的 /tmp 路径）。
	pathFn func(name string) (string, error)
}

func (f *fakeCapture) Status() (*CaptureSessionRow, []CaptureFileRow) { return f.active, f.files }
func (f *fakeCapture) Start(_ context.Context, ifname string, count int, acl string) error {
	if f.startErr != nil {
		return f.startErr
	}
	f.last = ifname
	f.active = &CaptureSessionRow{Interface: ifname, StartedAt: time.Now().UTC(), MaxDepth: count}
	return nil
}
func (f *fakeCapture) Stop(_ context.Context, export bool) (CaptureFileRow, error) {
	if f.active == nil {
		return CaptureFileRow{}, network.ErrNoCapture
	}
	f.active = nil
	f.stopped = true
	f.exported = export
	if !export {
		return CaptureFileRow{}, nil
	}
	row := CaptureFileRow{Name: "nfvis-cap-ens2f0-20260914T000000Z.pcap", SizeBytes: 128, CreatedAt: time.Now().UTC()}
	f.files = append(f.files, row)
	return row, nil
}
func (f *fakeCapture) Path(name string) (string, error) {
	if f.pathFn != nil {
		return f.pathFn(name)
	}
	if !strings.HasSuffix(name, ".pcap") {
		return "", network.ErrCaptureNotFound
	}
	return "/tmp/" + name, nil
}

// M5-3：/vpp/capture 端点（开始/状态/停止 409/下载）。
func TestCaptureEndpoints(t *testing.T) {
	fc := &fakeCapture{}
	ts := newTestServerOpts(t, Options{Capture: fc})
	token := loginAdmin(t, ts)

	// 开始
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/vpp/capture", token,
		map[string]any{"interface": "ens2f0", "count": 100},
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusAccepted {
		t.Fatalf("开始抓包: %d %s", status, data)
	}
	// 状态
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/vpp/capture", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), `"interface":"ens2f0"`) {
		t.Fatalf("抓包状态: %d %s", status, data)
	}
	// 会话进行中 → 409
	fc.startErr = network.ErrCaptureActive
	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/vpp/capture", token,
		map[string]any{"interface": "ens2f0"}, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusConflict {
		t.Fatalf("活动会话应 409，实际 %d", status)
	}
	fc.startErr = nil
	// ACL 过滤不支持 → 400
	fc.startErr = network.ErrCaptureFilterUnsupported
	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/vpp/capture", token,
		map[string]any{"interface": "ens2f0", "filter_acl": "a1"}, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusBadRequest {
		t.Fatalf("filter_acl 应 400，实际 %d", status)
	}
	fc.startErr = nil

	// 停止（不导出）→ 204
	if status, _, _ := cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/vpp/capture", token, nil,
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusNoContent {
		t.Fatalf("停止应 204，实际 %d", status)
	}
	// 无会话再停 → 409
	if status, _, _ := cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/vpp/capture", token, nil,
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusConflict {
		t.Fatalf("无会话停止应 409，实际 %d", status)
	}
}

// M5-3：CLI `request vpp trace start|stop|export` 与 `show vpp capture`。
func TestCLIVppTrace(t *testing.T) {
	fc := &fakeCapture{}
	x, _ := newCLIKit(t)
	x.setCapture(fc)

	out := run(t, x, "admin", "super-user", "ssh", "request vpp trace start interface ens2f0 count 100")
	if !strings.Contains(out, "已开始抓包") || fc.last != "ens2f0" {
		t.Fatalf("start: %s", out)
	}
	out = run(t, x, "admin", "super-user", "ssh", "show vpp capture")
	if !strings.Contains(out, "capturing: interface ens2f0") {
		t.Fatalf("show vpp capture: %s", out)
	}
	out = run(t, x, "admin", "super-user", "ssh", "request vpp trace export")
	if !strings.Contains(out, "已导出 pcap") || !fc.exported {
		t.Fatalf("export: %s", out)
	}
	out = run(t, x, "admin", "super-user", "ssh", "show vpp capture")
	if !strings.Contains(out, "nfvis-cap-") {
		t.Fatalf("show vpp capture 应列出文件: %s", out)
	}
	// 停止（不导出）路径
	run(t, x, "admin", "super-user", "ssh", "request vpp trace start interface ens2f0")
	out = run(t, x, "admin", "super-user", "ssh", "request vpp trace stop")
	if !strings.Contains(out, "已停止抓包") || fc.exported {
		t.Fatalf("stop: %s", out)
	}
	// 参数错误
	if got := x.Execute("admin", "super-user", "ssh", "request vpp trace start"); !strings.Contains(got.Output, "语法") {
		t.Fatalf("缺参数应报语法: %s", got.Output)
	}
}

// ---------- M5-7：软件升级/电源/NTP ----------

type fakeSoftware struct {
	added    string
	sha      string
	rolled   bool
	rebooted bool
	shut     bool
	ntp      bool
	err      error
}

func (f *fakeSoftware) Add(_ context.Context, pkg, sha string) (system.SoftwareResult, error) {
	if f.err != nil {
		return system.SoftwareResult{}, f.err
	}
	f.added, f.sha = pkg, sha
	return system.SoftwareResult{Action: "add", Version: "1.0.1", Previous: "1.0.0", Package: "nfvis_1.0.1_amd64.deb"}, nil
}
func (f *fakeSoftware) Rollback(_ context.Context) (system.SoftwareResult, error) {
	f.rolled = true
	return system.SoftwareResult{Action: "rollback", Version: "1.0.0", Previous: "1.0.1", Package: "nfvis_1.0.0_amd64.deb"}, nil
}
func (f *fakeSoftware) Reboot(_ context.Context) error   { f.rebooted = true; return nil }
func (f *fakeSoftware) Shutdown(_ context.Context) error { f.shut = true; return nil }
func (f *fakeSoftware) NTPSync(_ context.Context, _ []string) (string, error) {
	f.ntp = true
	return "chronyc makestep: 200 OK", nil
}

// 端点：POST /system/software、:rollback、:reboot、:shutdown、/system/ntp:sync。
func TestSoftwareEndpoints(t *testing.T) {
	fs := &fakeSoftware{}
	ts := newTestServerOpts(t, Options{Software: fs})
	token := loginAdmin(t, ts)

	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/software", token,
		map[string]any{"package": "/tmp/nfvis_1.0.1_amd64.deb", "sha256": "abc"},
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusAccepted {
		t.Fatalf("software add: %d %s", status, data)
	}
	if fs.added != "/tmp/nfvis_1.0.1_amd64.deb" || fs.sha != "abc" {
		t.Fatalf("Add 参数: %+v", fs)
	}
	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/software:rollback", token, nil,
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusAccepted || !fs.rolled {
		t.Fatalf("rollback 失败: %d", status)
	}
	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system:reboot", token, nil,
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusAccepted || !fs.rebooted {
		t.Fatalf("reboot 失败: %d", status)
	}
	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system:shutdown", token, nil,
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusAccepted || !fs.shut {
		t.Fatalf("shutdown 失败: %d", status)
	}
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/ntp:sync", token, nil,
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK || !fs.ntp ||
		!strings.Contains(string(data), "chronyc") {
		t.Fatalf("ntp sync 失败: %d %s", status, data)
	}
}

// CLI：request system software add|rollback（确认）、reboot|shutdown（确认）、ntp sync。
func TestCLISoftwarePowerNTP(t *testing.T) {
	fs := &fakeSoftware{}
	x, _ := newCLIKit(t)
	x.setSoftware(fs)

	// software add 需确认
	r := x.Execute("admin", "super-user", "ssh", "request system software add /tmp/nfvis_1.0.1_amd64.deb")
	if !strings.HasSuffix(strings.TrimSpace(r.Output), "[yes,no]") {
		t.Fatalf("software add 应先问询: %q", r.Output)
	}
	out := run(t, x, "admin", "super-user", "ssh", "request system software add /tmp/nfvis_1.0.1_amd64.deb --yes")
	if !strings.Contains(out, "升级完成") || fs.added == "" {
		t.Fatalf("software add: %s", out)
	}
	out = run(t, x, "admin", "super-user", "ssh", "request system software rollback --yes")
	if !strings.Contains(out, "回退完成") || !fs.rolled {
		t.Fatalf("rollback: %s", out)
	}
	// reboot 需确认
	r = x.Execute("admin", "super-user", "ssh", "request system reboot")
	if !strings.HasSuffix(strings.TrimSpace(r.Output), "[yes,no]") {
		t.Fatalf("reboot 应先问询: %q", r.Output)
	}
	out = run(t, x, "admin", "super-user", "ssh", "request system reboot --yes")
	if !strings.Contains(out, "已下发重启指令") || !fs.rebooted {
		t.Fatalf("reboot: %s", out)
	}
	out = run(t, x, "admin", "super-user", "ssh", "request system ntp sync")
	if !strings.Contains(out, "NTP 同步已触发") || !fs.ntp {
		t.Fatalf("ntp sync: %s", out)
	}
}

// ---------- 抓包：数据面中立命名 + 兼容别名（新名/旧名同一实现） ----------

// CLI：新名 `show capture` / `request capture start|stop|export` 与旧名
// `show vpp capture` / `request vpp trace …` 是**同一实现**——输出逐字相同（别名不破）。
func TestCLICaptureNamesAndAliases(t *testing.T) {
	fc := &fakeCapture{}
	x, _ := newCLIKit(t)
	x.setCapture(fc)

	// 旧名 start，新名看状态：两队名字指的是同一次会话
	out := run(t, x, "admin", "super-user", "ssh", "request vpp trace start interface ens2f0 count 100")
	if !strings.Contains(out, "已开始抓包") || fc.last != "ens2f0" {
		t.Fatalf("旧名 start: %s", out)
	}
	oldShow := run(t, x, "admin", "super-user", "ssh", "show vpp capture")
	newShow := run(t, x, "admin", "super-user", "ssh", "show capture")
	if !strings.Contains(newShow, "capturing: interface ens2f0") || newShow != oldShow {
		t.Fatalf("show capture 应与 show vpp capture 逐字一致：\n新名 %s\n旧名 %s", newShow, oldShow)
	}
	// 新名 export（旧名 stop 等价路径）
	out = run(t, x, "admin", "super-user", "ssh", "request capture export")
	if !strings.Contains(out, "已导出 pcap") || !fc.exported {
		t.Fatalf("新名 export: %s", out)
	}
	if !strings.Contains(out, "GET /capture/") || strings.Contains(out, "/vpp/capture/") {
		t.Fatalf("导出提示应指向中立端点 /capture：%s", out)
	}
	// 新名 stop
	run(t, x, "admin", "super-user", "ssh", "request capture start interface ens2f0")
	out = run(t, x, "admin", "super-user", "ssh", "request capture stop")
	if !strings.Contains(out, "已停止抓包") || fc.exported {
		t.Fatalf("新名 stop: %s", out)
	}
	// 参数错误与未知参数两支都要在（两族名字同一解析）
	if got := x.Execute("admin", "super-user", "ssh", "request capture start"); !strings.Contains(got.Output, "语法") {
		t.Fatalf("新名缺参数应报语法: %s", got.Output)
	}
	if got := x.Execute("admin", "super-user", "ssh", "request capture start interface ens2f0 nope x"); !strings.Contains(got.Output, "未知参数") {
		t.Fatalf("新名未知参数应报错: %s", got.Output)
	}
	if got := x.Execute("admin", "super-user", "ssh", "request vpp trace start interface ens2f0 nope x"); !strings.Contains(got.Output, "未知参数") {
		t.Fatalf("旧名未知参数应报错: %s", got.Output)
	}
}

// REST：新名 `/capture` 与兼容别名 `/vpp/capture` 走同一 handler（状态/开始/停止/下载四态逐字相同）。
func TestCaptureEndpointsBothPaths(t *testing.T) {
	fc := &fakeCapture{}
	ts := newTestServerOpts(t, Options{Capture: fc})
	token := loginAdmin(t, ts)

	paths := []string{"/capture", "/vpp/capture"}
	for _, p := range paths {
		if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+p, token,
			map[string]any{"interface": "ens2f0", "count": 100},
			map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusAccepted {
			t.Fatalf("POST %s 开始抓包: %d %s", p, status, data)
		}
	}

	// 状态：两条路径读到同一次会话
	bodies := map[string]string{}
	for _, p := range paths {
		status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+p, token, nil, nil)
		if status != http.StatusOK || !strings.Contains(string(data), `"interface":"ens2f0"`) {
			t.Fatalf("GET %s 抓包状态: %d %s", p, status, data)
		}
		bodies[p] = string(data)
	}
	if bodies["/capture"] != bodies["/vpp/capture"] {
		t.Fatalf("两条路径的状态响应应逐字相同：\n/capture %s\n/vpp/capture %s", bodies["/capture"], bodies["/vpp/capture"])
	}

	// 冲突与校验错误在两族路径上同码
	fc.startErr = network.ErrCaptureActive
	for _, p := range paths {
		if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+p, token,
			map[string]any{"interface": "ens2f0"}, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusConflict {
			t.Fatalf("POST %s 活动会话应 409，实际 %d", p, status)
		}
	}
	fc.startErr = network.ErrCaptureFilterUnsupported
	for _, p := range paths {
		if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+p, token,
			map[string]any{"interface": "ens2f0", "filter_acl": "a1"}, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusBadRequest {
			t.Fatalf("POST %s filter_acl 应 400，实际 %d", p, status)
		}
	}
	fc.startErr = nil

	// 停止并导出：两条路径都能导出（各自先起一次会话），且下载路径两条都通
	for _, p := range paths {
		if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+p, token,
			map[string]any{"interface": "ens2f0", "count": 10},
			map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusAccepted {
			t.Fatalf("POST %s 起会话: %d %s", p, status, data)
		}
		status, _, data := cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+p+"?export=true", token, nil,
			map[string]string{"X-NFVIS-Auto-Commit": "true"})
		if status != http.StatusOK || !strings.Contains(string(data), `"exported":true`) {
			t.Fatalf("DELETE %s?export=true: %d %s", p, status, data)
		}
	}
	// 无会话再停 → 409（两条路径同码）
	for _, p := range paths {
		if status, _, _ := cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+p, token, nil,
			map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusConflict {
			t.Fatalf("DELETE %s 无会话应 409，实际 %d", p, status)
		}
	}
	// 下载：两条路径都到同一个 handler——渲染真实导出件（临时目录里造一个 .pcap）
	pcapPath := filepath.Join(t.TempDir(), "x.pcap")
	if err := os.WriteFile(pcapPath, []byte("pcap-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	fc.pathFn = func(name string) (string, error) {
		if name != "x.pcap" {
			return "", network.ErrCaptureNotFound
		}
		return pcapPath, nil
	}
	for _, p := range []string{"/capture/x.pcap", "/vpp/capture/x.pcap"} {
		status, headers, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+p, token, nil, nil)
		if status != http.StatusOK || string(data) != "pcap-bytes" {
			t.Fatalf("GET %s 应下载导出件：%d %s", p, status, data)
		}
		if ct := headers.Get("Content-Type"); !strings.Contains(ct, "octet-stream") {
			t.Fatalf("GET %s Content-Type 应为 octet-stream：%s", p, ct)
		}
	}
	for _, p := range []string{"/capture/x.txt", "/vpp/capture/x.txt"} {
		if status, _, _ := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+p, token, nil, nil); status != http.StatusNotFound {
			t.Fatalf("GET %s 非导出件应 404，实际 %d", p, status)
		}
	}
}

// 抓包接口候选：只列**当前数据面可抓的设备**（新名命令的候选来源 dataplane-ifnames），
// 不是「VPP ∪ 内核」的并集——否则会把管理口列进候选、敲下去才被拒。
func TestCaptureInterfaceCandidates(t *testing.T) {
	ts := newTestServerOpts(t, Options{Ports: fakePorts{
		vpp:    []string{"ens192", "vs-l2"},
		kernel: []string{"ens160", "ens192"}, // ens160 是管理口（不在数据面）
	}})
	token := loginAdmin(t, ts)

	toks := cliCandidateTokens(t, ts, token, "request,capture,start,interface")
	if !hasTok(toks, "ens192") || !hasTok(toks, "vs-l2") {
		t.Fatalf("抓包接口候选应含数据面设备：%v", toks)
	}
	if hasTok(toks, "ens160") {
		t.Fatalf("管理口（内核有、数据面没有）不应出现在抓包候选里：%v", toks)
	}

	// 旧名（兼容别名）仍用 VPP 端口候选（与旧契约一致，不因新名而改）
	oldToks := cliCandidateTokens(t, ts, token, "request,vpp,trace,start,interface")
	if !hasTok(oldToks, "ens192") {
		t.Fatalf("旧名抓包接口候选应含 VPP 端口：%v", oldToks)
	}
}
