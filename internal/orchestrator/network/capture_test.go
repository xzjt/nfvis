package network

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakePcapShell 记录 VPP CLI 命令并模拟 pcap 输出文件。
type fakePcapShell struct {
	calls   []string
	outPath string // 模拟 VPP 固定输出文件
	written int    // "Write N packets" 的 N
	err     error
}

func (f *fakePcapShell) Run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if f.err != nil {
		return "", f.err
	}
	if len(args) > 0 && args[len(args)-1] == "off" {
		if f.written > 0 {
			_ = os.WriteFile(f.outPath, make([]byte, 64), 0o644)
			return "Write " + itoa(f.written) + " packets to " + f.outPath + ", and stop capture...", nil
		}
		return "No packets captured...", nil
	}
	return "", nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func newCaptureFixture(t *testing.T) (*CaptureProvider, *fakePcapShell, string, string) {
	t.Helper()
	exportDir := t.TempDir()
	vppDir := t.TempDir()
	out := filepath.Join(vppDir, "rxtx.pcap")
	sh := &fakePcapShell{outPath: out, written: 6}
	p := NewCaptureProvider(sh, func(ifname string) (uint32, bool, error) {
		if ifname == "ens192" {
			return 1, true, nil
		}
		return 0, false, nil
	}, exportDir)
	p.SetVPPOutputPath(out)
	// 决策 #466：会话记录与真实宿主解耦（否则启动收敛会读/写生产落点）。
	p.SetSessionsFile(filepath.Join(t.TempDir(), "capture-sessions.json"))
	return p, sh, exportDir, out
}

func TestCaptureStartStopExport(t *testing.T) {
	p, sh, exportDir, _ := newCaptureFixture(t)
	ctx := context.Background()

	// 接口不存在（此时无活动会话，先验接口解析）
	if err := p.Start(ctx, "ens999", 10, ""); err == nil || !strings.Contains(err.Error(), "抓包接口") {
		t.Fatalf("接口不存在应报错: %v", err)
	}
	if err := p.Start(ctx, "ens192", 50, ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(sh.calls) != 1 || sh.calls[0] != "pcap trace rx tx intfc ens192 max 50" {
		t.Fatalf("开始命令: %v", sh.calls)
	}
	// 重复开始 → 409 语义
	if err := p.Start(ctx, "ens192", 10, ""); err == nil || !strings.Contains(err.Error(), "已有抓包会话") {
		t.Fatalf("重复开始应报活动会话: %v", err)
	}
	// ACL 过滤不支持
	if err := p.Start(ctx, "ens224", 10, "acl-x"); err == nil || !strings.Contains(err.Error(), "不支持 ACL 过滤") {
		t.Fatalf("filter_acl 应明确不支持: %v", err)
	}

	active, files := p.Status()
	if active == nil || active.Interface != "ens192" || len(files) != 0 {
		t.Fatalf("Status: %+v %v", active, files)
	}

	f, err := p.Stop(ctx, true)
	if err != nil {
		t.Fatalf("Stop(export): %v", err)
	}
	if f.Name == "" || !strings.HasSuffix(f.Name, ".pcap") || f.SizeBytes == 0 {
		t.Fatalf("导出文件: %+v", f)
	}
	if _, err := os.Stat(filepath.Join(exportDir, f.Name)); err != nil {
		t.Fatalf("导出文件应存在: %v", err)
	}
	// 会话已结束
	if active, _ := p.Status(); active != nil {
		t.Fatalf("停止后不应有活动会话: %+v", active)
	}
	if _, err := p.Stop(ctx, true); err == nil {
		t.Fatal("无会话时 stop 应报错")
	}
	// 下载路径与穿越拒绝
	if _, err := p.Path(f.Name); err != nil {
		t.Fatalf("Path: %v", err)
	}
	if _, err := p.Path("../etc/passwd"); err == nil {
		t.Fatal("穿越应拒绝")
	}
	if _, err := p.Path("x.txt"); err == nil {
		t.Fatal("非 .pcap 应拒绝")
	}
}

// 决策 #465：`stop`（不导出）＝**冻结**——保留工作文件与会话登记（Stopped=true），
// 随后的 `export` 走同一 finalize 路径取走。改前这里删文件 + 摘登记（「先 stop 再 export」必失败）。
// 决策 #466：冻结即把工作文件**暂存**进导出目录隐藏位（`.<终名>.part`，不依赖 /tmp 存活）。
func TestCaptureStopWithoutExportFreezes(t *testing.T) {
	p, _, exportDir, out := newCaptureFixture(t)
	ctx := context.Background()
	if err := p.Start(ctx, "ens192", 10, ""); err != nil {
		t.Fatal(err)
	}
	f, err := p.Stop(ctx, false)
	if err != nil || f.Name != "" {
		t.Fatalf("不导出应返回空文件: %+v %v", f, err)
	}
	// ① 会话登记保留，且工作文件搬进导出目录隐藏暂存位（原 /tmp 位不再持有数据）
	active, files := p.Status()
	if active == nil || !active.Stopped || active.Recovered || len(files) != 0 {
		t.Fatalf("停止后会话应以「待导出」留在登记里、尚无导出: %+v %v", active, files)
	}
	staged := filepath.Join(exportDir, "."+active.filename+".part")
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("冻结后应有隐藏暂存文件: %v", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("冻结后原 /tmp 工作文件应已搬走: %v", err)
	}
	// 会话记录已落盘（stopped）。
	recs, rerr := LoadCaptureRecords(p.sessionsFile)
	if rerr != nil || recs[CapturePlaneVPP] == nil || !recs[CapturePlaneVPP].Stopped || recs[CapturePlaneVPP].FileName != active.filename {
		t.Fatalf("会话记录不符: %+v %v", recs, rerr)
	}
	// ② 冻结中起新会话：如实拒绝并给照做路径（别让它变成「为什么起不来」）
	if err := p.Start(ctx, "ens192", 10, ""); err == nil || !strings.Contains(err.Error(), "待导出") {
		t.Fatalf("冻结中应拒绝新会话并给照做路径: %v", err)
	}
	// ③ 再停一次是幂等（仍是「已停止」，不是「无会话」）
	if _, err := p.Stop(ctx, false); err != nil {
		t.Fatalf("冻结中再停应幂等: %v", err)
	}
	// ④ export 取走：文件从暂存位改名搬进导出目录、登记与记录摘除
	f2, err := p.Stop(ctx, true)
	if err != nil || f2.Name == "" {
		t.Fatalf("冻结后 export 应取到文件: %+v %v", f2, err)
	}
	if active2, _ := p.Status(); active2 != nil {
		t.Fatalf("导出后登记应摘除: %+v", active2)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("导出后暂存文件应已改名搬走: %v", err)
	}
	if got := p.List(); len(got) != 1 {
		t.Fatalf("导出目录应有 1 个文件: %v", got)
	}
	if recs, _ := LoadCaptureRecords(p.sessionsFile); recs[CapturePlaneVPP] != nil {
		t.Fatalf("导出后记录应已清除: %+v", recs)
	}
}

func TestCaptureNoPackets(t *testing.T) {
	p, sh, _, _ := newCaptureFixture(t)
	sh.written = 0
	ctx := context.Background()
	if err := p.Start(ctx, "ens192", 10, ""); err != nil {
		t.Fatal(err)
	}
	f, err := p.Stop(ctx, true)
	if err != nil || f.Name != "" {
		t.Fatalf("无包时导出应为空且不报错: %+v %v", f, err)
	}
}

// ---------- 决策 #466：跨 nfvisd 重启收敛 ----------

// 记录说「已冻结」：重启后按记录重建为「已停止待导出」，export 取到原数据，取走清记录。
func TestCaptureReconcileRecoversFrozenSession(t *testing.T) {
	p, _, exportDir, _ := newCaptureFixture(t)
	ctx := context.Background()
	name := "nfvis-cap-ens192-20261012T010203Z.pcap"
	staged := filepath.Join(exportDir, "."+name+".part")
	if err := os.WriteFile(staged, []byte("frozen-pcap"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveCaptureRecord(p.sessionsFile, CapturePlaneVPP, &CaptureSessionRecord{
		Interface: "ens192", FileName: name, WorkPath: staged,
		StartedAt: time.Now().UTC().Add(-time.Minute), Count: 500, Stopped: true,
	}); err != nil {
		t.Fatal(err)
	}
	rep := p.Reconcile(ctx)
	active, files := p.Status()
	if active == nil || !active.Stopped || !active.Recovered || active.Interface != "ens192" || active.MaxDepth != 500 || len(files) != 0 {
		t.Fatalf("重启收敛应重建冻结会话: %+v %v（报告 %v）", active, files, rep)
	}
	// 冻结会话仍占登记位：新 start 被拒并给照做路径。
	if err := p.Start(ctx, "ens192", 10, ""); err == nil || !strings.Contains(err.Error(), "待导出") {
		t.Fatalf("恢复的会话应拦下新 start: %v", err)
	}
	f, err := p.Stop(ctx, true)
	if err != nil || f.Name != name || f.SizeBytes == 0 {
		t.Fatalf("恢复的会话应可 export: %+v %v", f, err)
	}
	if recs, _ := LoadCaptureRecords(p.sessionsFile); recs[CapturePlaneVPP] != nil {
		t.Fatalf("取走后记录应清除: %+v", recs)
	}
}

// 记录说「在抓」：重启收敛补一次 `pcap trace off` 把缓冲落盘，再重建为冻结会话。
func TestCaptureReconcileActiveSessionFlushesTrace(t *testing.T) {
	p, sh, _, _ := newCaptureFixture(t)
	ctx := context.Background()
	// 会话建立后 nfvisd 被杀：工作文件尚未生成（VPP 只在 off 时落盘），记录停在「在抓」。
	if err := SaveCaptureRecord(p.sessionsFile, CapturePlaneVPP, &CaptureSessionRecord{
		Interface: "ens192", FileName: "nfvis-cap-ens192-20261012T010405Z.pcap",
		WorkPath: p.vppOut, StartedAt: time.Now().UTC().Add(-time.Minute), Count: 50,
	}); err != nil {
		t.Fatal(err)
	}
	rep := p.Reconcile(ctx)
	if len(sh.calls) == 0 || sh.calls[len(sh.calls)-1] != "pcap trace off" {
		t.Fatalf("在抓会话应收敛前补 off: %v", sh.calls)
	}
	active, _ := p.Status()
	if active == nil || !active.Stopped || !active.Recovered {
		t.Fatalf("应收敛为已停止待导出: %+v（报告 %v）", active, rep)
	}
	f, err := p.Stop(ctx, true)
	if err != nil || f.Name == "" || f.SizeBytes == 0 {
		t.Fatalf("收敛的数据应可 export: %+v %v", f, err)
	}
}

// 记录说「在抓」但没有任何数据（off 报无包/未启用）：如实关闭并清记录，不留假会话。
func TestCaptureReconcileActiveNoDataCloses(t *testing.T) {
	p, sh, _, _ := newCaptureFixture(t)
	ctx := context.Background()
	sh.written = 0 // off 返回 "No packets captured"、不写文件
	if err := SaveCaptureRecord(p.sessionsFile, CapturePlaneVPP, &CaptureSessionRecord{
		Interface: "ens192", FileName: "nfvis-cap-ens192-20261012T011405Z.pcap",
		WorkPath: p.vppOut, StartedAt: time.Now().UTC().Add(-time.Minute), Count: 50,
	}); err != nil {
		t.Fatal(err)
	}
	rep := p.Reconcile(ctx)
	if active, _ := p.Status(); active != nil {
		t.Fatalf("无数据不应留会话: %+v", active)
	}
	if recs, _ := LoadCaptureRecords(p.sessionsFile); recs[CapturePlaneVPP] != nil {
		t.Fatalf("无数据应清记录: %+v", recs)
	}
	found := false
	for _, line := range rep {
		if strings.Contains(line, "已关闭") {
			found = true
		}
	}
	if !found {
		t.Fatalf("报告应如实说明已关闭: %v", rep)
	}
}

// VPP 未就绪：收敛整体延后、记录不动；VPP 恢复后由下一次抓包操作（Status）自动补跑。
func TestCaptureReconcileDeferredWhenVPPDown(t *testing.T) {
	p, sh, _, _ := newCaptureFixture(t)
	ctx := context.Background()
	if err := SaveCaptureRecord(p.sessionsFile, CapturePlaneVPP, &CaptureSessionRecord{
		Interface: "ens192", FileName: "nfvis-cap-ens192-20261012T012405Z.pcap",
		WorkPath: p.vppOut, StartedAt: time.Now().UTC().Add(-time.Minute), Count: 50,
	}); err != nil {
		t.Fatal(err)
	}
	sh.err = errors.New("vppctl: connect: no such file or directory")
	_ = p.Reconcile(ctx)
	if active, _ := p.Status(); active != nil {
		t.Fatalf("VPP 未就绪不应接管会话: %+v", active) // （Status 的补跑同样失败）
	}
	if recs, _ := LoadCaptureRecords(p.sessionsFile); recs[CapturePlaneVPP] == nil || recs[CapturePlaneVPP].Stopped {
		t.Fatalf("记录应保持「在抓」不动: %+v", recs)
	}
	// VPP 恢复：下一次操作自动补跑（written=6 ⇒ off 写数据文件）。
	sh.err = nil
	active, _ := p.Status()
	if active == nil || !active.Stopped || !active.Recovered {
		t.Fatalf("补跑应收敛为已停止待导出: %+v", active)
	}
	if recs, _ := LoadCaptureRecords(p.sessionsFile); recs[CapturePlaneVPP] == nil || !recs[CapturePlaneVPP].Stopped {
		t.Fatalf("补跑后记录应标已停止: %+v", recs)
	}
}

// 无记录残件（旧版本遗留 / 记录写失败）：非空归档为可见 pcap（不藏不删），空件清掉。
func TestCaptureResidueSweepArchives(t *testing.T) {
	p, _, exportDir, out := newCaptureFixture(t)
	ctx := context.Background()
	stray := filepath.Join(exportDir, ".nfvis-cap-ens224-20261012T020304Z.pcap.part")
	if err := os.WriteFile(stray, []byte("stray-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte("bare-rxtx"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(exportDir, ".nfvis-cap-ens224-20261012T030405Z.pcap.part")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rep := p.Reconcile(ctx)
	files := p.List()
	names := map[string]bool{}
	for _, f := range files {
		names[f.Name] = true
	}
	if len(files) != 2 || !names["nfvis-cap-ens224-20261012T020304Z.pcap"] {
		t.Fatalf("带名残件应按源名归档为可见文件: %v（报告 %v）", names, rep)
	}
	recov := false
	for n := range names {
		if strings.HasPrefix(n, "nfvis-cap-recovered-") {
			recov = true
		}
	}
	if !recov {
		t.Fatalf("裸 rxtx.pcap 应以 recovered 命名归档: %v", names)
	}
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Fatalf("空残件应清掉: %v", err)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatalf("带名残件应已搬走: %v", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("裸残件应已搬走: %v", err)
	}
}
