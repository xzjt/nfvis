package netkernel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// ---------- 夹具：假进程启动器 / 假进程 ----------

type fakeProc struct {
	pid  int
	args []string

	mu      sync.Mutex
	done    chan struct{}
	err     error
	out     string
	termErr error
	termN   int
}

func newFakeProc(pid int, args ...string) *fakeProc {
	return &fakeProc{pid: pid, args: args, done: make(chan struct{})}
}

func (p *fakeProc) Pid() int              { return p.pid }
func (p *fakeProc) Done() <-chan struct{} { return p.done }
func (p *fakeProc) Err() error            { p.mu.Lock(); defer p.mu.Unlock(); return p.err }
func (p *fakeProc) Output() string        { p.mu.Lock(); defer p.mu.Unlock(); return p.out }

func (p *fakeProc) Terminate() error {
	p.mu.Lock()
	p.termN++
	err := p.termErr
	p.mu.Unlock()
	if err != nil {
		return err
	}
	p.finish(nil, "")
	return nil
}

// finish 让进程"退出"（幂等）。
func (p *fakeProc) finish(err error, out string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.done:
		return
	default:
	}
	p.err = err
	if out != "" {
		p.out = out
	}
	close(p.done)
}

type fakeStarter struct {
	mu    sync.Mutex
	pid   int
	calls [][]string
	procs []*fakeProc
	fail  error
	// onStart 在进程"启动"后调用（夹具按 -w 参数造出抓包文件等）。
	// 注意：在 start 的锁内调用，回调里不得再调 start 的方法。
	onStart func(args []string, p *fakeProc)
}

func (s *fakeStarter) start(name string, args ...string) (CaptureProc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return nil, s.fail
	}
	full := append([]string{name}, args...)
	s.calls = append(s.calls, full)
	s.pid++
	p := newFakeProc(1000+s.pid, full...)
	s.procs = append(s.procs, p)
	if s.onStart != nil {
		s.onStart(full, p)
	}
	return p, nil
}

func (s *fakeStarter) lastCall() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		return nil
	}
	return s.calls[len(s.calls)-1]
}

// newTestCapture 构造一份不触碰宿主的抓包实现：假 Runner（ip 命令）+ 假启动器 + 临时目录。
func newTestCapture(t *testing.T, cfg model.Config, s *fakeStarter) (*Capture, *fakeRunner) {
	t.Helper()
	f := &fakeRunner{}
	// `ip -j link show` 的输出：ens192（数据面业务口）、ens160（管理口，未声明）、vs-l2（bridge）。
	f.replies = append(f.replies, fakeReply{prefix: "ip -j link show", out: `[{"ifname":"lo"},{"ifname":"ens160"},{"ifname":"ens192"},{"ifname":"vs-l2"}]`})
	c := NewCapture(f)
	c.SetDir(t.TempDir())
	// 决策 #466：工作位与会话记录都与真实宿主解耦（否则启动收敛的残件扫描会越界到
	// 共享的系统临时目录/生产落点）。
	c.SetWorkDir(t.TempDir())
	c.SetSessionsFile(filepath.Join(t.TempDir(), "capture-sessions.json"))
	c.SetStarter(s.start)
	c.SetConfigSource(func() (model.Config, error) { return cfg, nil })
	c.probe = 5 * time.Millisecond
	return c, f
}

func kernelCfg() model.Config {
	return model.Config{
		Interfaces:      []model.InterfaceConfig{{Name: "ens192"}},
		VirtualSwitches: []model.VirtualSwitch{{Name: "vs-l2", Type: "l2"}},
	}
}

func mustErrIs(t *testing.T, err, want error, what string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s：期望 %v，得到 %v", what, want, err)
	}
}

// 起：命令形状正确（tcpdump -i <dev> -w <工作文件> -c <count>）、会话登记、count 缺省 1000。
func TestCaptureStartCommandAndSession(t *testing.T) {
	s := &fakeStarter{}
	c, _ := newTestCapture(t, kernelCfg(), s)

	if err := c.Start(context.Background(), "ens192", 100, ""); err != nil {
		t.Fatalf("开始抓包: %v", err)
	}
	call := strings.Join(s.lastCall(), " ")
	if !strings.HasPrefix(call, "tcpdump -i ens192 -w ") || !strings.HasSuffix(call, " -c 100") {
		t.Fatalf("命令形状不符：%s", call)
	}
	if !strings.Contains(call, ".nfvis-cap-ens192-") || !strings.Contains(call, ".pcap.part") {
		t.Fatalf("工作文件应是 .part 文件：%s", call)
	}
	// 工作文件落在**系统临时目录**、**不落导出目录**：宿主 AppArmor 的 tcpdump 配置不放行
	// `/var/lib/nfvis/captures`（真机实测被拒），而 /tmp 放行（VPP 侧同样走 /tmp 再搬）。
	if !strings.Contains(call, os.TempDir()) {
		t.Fatalf("工作文件应落在系统临时目录 %s：%s", os.TempDir(), call)
	}
	if strings.Contains(call, c.Dir()) {
		t.Fatalf("工作文件不得落在导出目录 %s（AppArmor 不放行 tcpdump 写该目录）：%s", c.Dir(), call)
	}
	active, files := c.Status()
	if active == nil || active.Interface != "ens192" || active.MaxDepth != 100 {
		t.Fatalf("会话未登记或形状不符：%+v", active)
	}
	if active.StartedAt.IsZero() || len(files) != 0 {
		t.Fatalf("会话时间/清单不符：active=%+v files=%+v", active, files)
	}

	// 缺省 count = 1000（与 VPP 侧同值）
	s2 := &fakeStarter{}
	c2, _ := newTestCapture(t, kernelCfg(), s2)
	if err := c2.Start(context.Background(), "vs-l2", 0, ""); err != nil {
		t.Fatalf("开始抓包（缺省 count）: %v", err)
	}
	if call := strings.Join(s2.lastCall(), " "); !strings.HasSuffix(call, " -c 1000") {
		t.Fatalf("缺省 count 应为 1000：%s", call)
	}
}

// 已有会话 ⇒ ErrCaptureActive（API 409 语义）。
func TestCaptureRejectsActiveSession(t *testing.T) {
	s := &fakeStarter{}
	c, _ := newTestCapture(t, kernelCfg(), s)
	if err := c.Start(context.Background(), "ens192", 10, ""); err != nil {
		t.Fatalf("第一次开始: %v", err)
	}
	err := c.Start(context.Background(), "vs-l2", 10, "")
	mustErrIs(t, err, network.ErrCaptureActive, "第二个会话")
	if len(s.calls) != 1 {
		t.Fatalf("第二个会话不得启动新进程：%+v", s.calls)
	}
}

// 停（不导出）：进程被终止，工作文件**暂存进导出目录隐藏位**、会话登记保留
// （决策 #465：冻结待 export 取走；决策 #466：冻结即暂存，不再依赖临时目录存活）；
// 冻结中再停 ⇒ 幂等成功（不是 ErrNoCapture——数据还在）。
func TestCaptureStopTerminatesAndFreezes(t *testing.T) {
	s := &fakeStarter{}
	work := ""
	s.onStart = func(args []string, p *fakeProc) {
		for i, a := range args {
			if a == "-w" && i+1 < len(args) {
				work = args[i+1]
				_ = os.WriteFile(work, []byte("pcap-bytes"), 0o600)
			}
		}
	}
	c, _ := newTestCapture(t, kernelCfg(), s)
	if err := c.Start(context.Background(), "ens192", 10, ""); err != nil {
		t.Fatalf("开始抓包: %v", err)
	}
	if _, err := c.Stop(context.Background(), false); err != nil {
		t.Fatalf("停止抓包: %v", err)
	}
	if n := s.procs[0].termN; n != 1 {
		t.Fatalf("停止应终止进程一次，实际 %d", n)
	}
	// 决策 #466：冻结即暂存——工作文件从临时目录搬进导出目录的隐藏位（`.<终名>.part`，不进清单）。
	base := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(work), "."), ".part")
	staged := filepath.Join(c.Dir(), "."+base+".part")
	if _, err := os.Stat(work); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("冻结后临时工作文件应已搬走（暂存）：%v", err)
	}
	if info, err := os.Stat(staged); err != nil || info.Size() == 0 {
		t.Fatalf("冻结后暂存位应有数据：%v", err)
	}
	active, files := c.Status()
	if active == nil || !active.Stopped || active.Recovered || len(files) != 0 {
		t.Fatalf("停止后应以「待导出」留在登记里、尚无导出：active=%+v files=%+v", active, files)
	}
	// 决策 #466：会话记录已落盘（stopped、含 pid）。
	recs, err := network.LoadCaptureRecords(c.sessionsFile)
	if err != nil {
		t.Fatalf("读会话记录: %v", err)
	}
	if rec := recs[network.CapturePlaneKernel]; rec == nil || !rec.Stopped || rec.PID != s.procs[0].pid || rec.FileName != base {
		t.Fatalf("会话记录不符：%+v", recs)
	}
	if _, err := c.Stop(context.Background(), false); err != nil {
		t.Fatalf("冻结中再停应幂等成功：%v", err)
	}
	// export 取走：文件从暂存位搬进导出目录终名、登记与记录摘除
	f, err := c.Stop(context.Background(), true)
	if err != nil || f.Name != base {
		t.Fatalf("冻结后 export 应取到文件：%+v %v", f, err)
	}
	if active, _ := c.Status(); active != nil {
		t.Fatalf("导出后登记应摘除：%+v", active)
	}
	if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("导出后暂存文件应已改名搬走：%v", err)
	}
	if recs, _ := network.LoadCaptureRecords(c.sessionsFile); recs[network.CapturePlaneKernel] != nil {
		t.Fatalf("导出后记录应已清除：%+v", recs)
	}
}

// 导出：工作文件搬入导出目录（沿用既有命名口径）、清单可查、下载路径可解析、会话摘除。
func TestCaptureExportMovesFileAndLists(t *testing.T) {
	s := &fakeStarter{}
	var work string
	s.onStart = func(args []string, p *fakeProc) {
		for i, a := range args {
			if a == "-w" && i+1 < len(args) {
				work = args[i+1]
				_ = os.WriteFile(work, []byte("pcap-bytes-1234"), 0o600)
			}
		}
	}
	c, _ := newTestCapture(t, kernelCfg(), s)
	if err := c.Start(context.Background(), "ens192", 10, ""); err != nil {
		t.Fatalf("开始抓包: %v", err)
	}
	row, err := c.Stop(context.Background(), true)
	if err != nil {
		t.Fatalf("导出: %v", err)
	}
	if !strings.HasPrefix(row.Name, "nfvis-cap-ens192-") || !strings.HasSuffix(row.Name, ".pcap") {
		t.Fatalf("导出文件名不符既有口径：%q", row.Name)
	}
	if row.SizeBytes != int64(len("pcap-bytes-1234")) || row.CreatedAt.IsZero() {
		t.Fatalf("导出文件行字段不符：%+v", row)
	}
	if _, err := os.Stat(work); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("导出后工作文件应已改名（err=%v）", err)
	}
	active, files := c.Status()
	if active != nil {
		t.Fatalf("导出后应无会话：%+v", active)
	}
	if len(files) != 1 || files[0].Name != row.Name || files[0].SizeBytes != row.SizeBytes {
		t.Fatalf("清单应含导出件：%+v", files)
	}
	path, err := c.Path(row.Name)
	if err != nil {
		t.Fatalf("下载路径应可解析: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "pcap-bytes-1234" {
		t.Fatalf("下载路径内容不符：%q", got)
	}
	// 穿越防护
	if _, err := c.Path("../" + row.Name); !errors.Is(err, network.ErrCaptureNotFound) {
		t.Fatalf("越界名字应报不存在：%v", err)
	}
}

// 一个包也没抓到（tcpdump 不建文件 / 空文件）：导出回报「无文件」，不留残渣。
func TestCaptureExportEmptyMeansNoFile(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write bool
	}{{"文件不存在", false}, {"空文件", true}} {
		t.Run(tc.name, func(t *testing.T) {
			s := &fakeStarter{}
			if tc.write {
				s.onStart = func(args []string, p *fakeProc) {
					for i, a := range args {
						if a == "-w" && i+1 < len(args) {
							_ = os.WriteFile(args[i+1], nil, 0o600)
						}
					}
				}
			}
			c, _ := newTestCapture(t, kernelCfg(), s)
			if err := c.Start(context.Background(), "ens192", 10, ""); err != nil {
				t.Fatalf("开始抓包: %v", err)
			}
			row, err := c.Stop(context.Background(), true)
			if err != nil {
				t.Fatalf("导出: %v", err)
			}
			if row.Name != "" || row.SizeBytes != 0 {
				t.Fatalf("未捕获到报文不应回文件行：%+v", row)
			}
			if active, files := c.Status(); active != nil || len(files) != 0 {
				t.Fatalf("无会话无导出件：active=%+v files=%+v", active, files)
			}
			if entries, _ := os.ReadDir(c.Dir()); len(entries) != 0 {
				t.Fatalf("导出目录不应留残渣：%+v", entries)
			}
		})
	}
}

// 设备解析：不在数据面（管理口/未声明口）⇒ ErrIfaceUnavailable；数据面设备不存在于内核 ⇒ 点名 DPDK 残留。
func TestCaptureDeviceResolution(t *testing.T) {
	s := &fakeStarter{}
	c, f := newTestCapture(t, kernelCfg(), s)

	// 管理口（内核里有、但不在数据面）——不可抓包
	mgmtErr := c.Start(context.Background(), "ens160", 10, "")
	mustErrIs(t, mgmtErr, network.ErrIfaceUnavailable, "管理口")
	// 文案按**内核口径**：哨兵自身的措辞是 VPP 口径（「接口在 VPP 中不存在」），
	// 内核数据面下直接 wrap 会给操作者一句自相矛盾的话（本机根本没有 VPP）。
	if strings.Contains(mgmtErr.Error(), "VPP") {
		t.Fatalf("内核口径的拒绝文案不得提 VPP：%v", mgmtErr)
	}
	// 完全不存在的名字
	unkErr := c.Start(context.Background(), "nope0", 10, "")
	mustErrIs(t, unkErr, network.ErrIfaceUnavailable, "未知口")
	if strings.Contains(unkErr.Error(), "VPP") {
		t.Fatalf("内核口径的拒绝文案不得提 VPP：%v", unkErr)
	}
	if len(s.calls) != 0 {
		t.Fatalf("被拒的设备不得启动进程：%+v", s.calls)
	}

	// 声明为数据面口、但内核里没有（DPDK 残留：ip link 清单无该口）
	f.replies = []fakeReply{{prefix: "ip -j link show", out: `[{"ifname":"lo"},{"ifname":"vs-l2"}]`}}
	err := c.Start(context.Background(), "ens192", 10, "")
	mustErrIs(t, err, network.ErrIfaceUnavailable, "DPDK 残留")
	if !strings.Contains(err.Error(), "unbind-dpdk") {
		t.Fatalf("应点名照做路径（先解绑）：%v", err)
	}
	// 长交换机名按 LinkName 映射到内核设备名
	longName := "a-very-long-switch-name"
	dev := LinkName(longName)
	c2, f2 := newTestCapture(t, model.Config{VirtualSwitches: []model.VirtualSwitch{{Name: longName, Type: "l2"}}}, &fakeStarter{})
	f2.replies = []fakeReply{{prefix: "ip -j link show", out: `[{"ifname":"lo"},{"ifname":"` + dev + `"}]`}}
	if err := c2.Start(context.Background(), longName, 10, ""); err != nil {
		t.Fatalf("按 LinkName 解析设备名（%s）: %v", dev, err)
	}
	// ip 读失败：无法确认 ⇒ 如实报错（不当作"在数据面"放行）
	f3 := &fakeRunner{failAll: errors.New("ip: command not found")}
	c3 := NewCapture(f3)
	c3.SetDir(t.TempDir())
	c3.SetStarter(s.start)
	c3.SetConfigSource(func() (model.Config, error) { return kernelCfg(), nil })
	c3.probe = time.Millisecond
	if err := c3.Start(context.Background(), "ens192", 10, ""); err == nil || !strings.Contains(err.Error(), "读取内核接口清单失败") {
		t.Fatalf("读不到内核接口清单应如实报错：%v", err)
	}
}

// filter <acl>：沿用既有「不支持」口径（内核侧本轮不接 BPF 过滤）。
func TestCaptureFilterUnsupported(t *testing.T) {
	s := &fakeStarter{}
	c, _ := newTestCapture(t, kernelCfg(), s)
	err := c.Start(context.Background(), "ens192", 10, "acl-in")
	mustErrIs(t, err, network.ErrCaptureFilterUnsupported, "ACL 过滤")
	if len(s.calls) != 0 {
		t.Fatalf("不支持过滤时不得启动进程：%+v", s.calls)
	}
}

// 启动即失败（tcpdump 起不来）：如实报错并清掉工作文件，不登记会话。
func TestCaptureStartProbeFailure(t *testing.T) {
	s := &fakeStarter{}
	var work string
	// 让"进程"立刻异常退出（模拟无权限/设备不可用），tcpdump 的报错在输出里。
	s.onStart = func(args []string, p *fakeProc) {
		for i, a := range args {
			if a == "-w" && i+1 < len(args) {
				work = args[i+1]
				_ = os.WriteFile(work, nil, 0o600)
			}
		}
		p.finish(errors.New("exit status 1"), "tcpdump: eth0: You don't have permission to capture")
	}
	c, _ := newTestCapture(t, kernelCfg(), s)
	err := c.Start(context.Background(), "ens192", 10, "")
	if err == nil || !strings.Contains(err.Error(), "开始抓包失败") {
		t.Fatalf("启动即失败应如实报错：%v", err)
	}
	if !strings.Contains(err.Error(), "tcpdump: eth0") {
		t.Fatalf("应带上底座输出便于排查：%v", err)
	}
	if active, _ := c.Status(); active != nil {
		t.Fatalf("启动失败不得登记会话：%+v", active)
	}
	if _, err := os.Stat(work); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("启动失败应清掉工作文件（err=%v）", err)
	}
	// 启动器本身失败（未安装 tcpdump）
	s2 := &fakeStarter{fail: errors.New("exec: \"tcpdump\": executable file not found in $PATH")}
	c2, _ := newTestCapture(t, kernelCfg(), s2)
	err2 := c2.Start(context.Background(), "ens192", 10, "")
	if err2 == nil || !strings.Contains(err2.Error(), "tcpdump") || !strings.Contains(err2.Error(), "是否已安装") {
		t.Fatalf("未安装 tcpdump 应给可读错误：%v", err2)
	}
}

// 停不掉（终止失败）：会话保留、如实报错——重试仍能停。
func TestCaptureStopKeepsSessionWhenTerminateFails(t *testing.T) {
	s := &fakeStarter{}
	c, _ := newTestCapture(t, kernelCfg(), s)
	if err := c.Start(context.Background(), "ens192", 10, ""); err != nil {
		t.Fatalf("开始抓包: %v", err)
	}
	s.procs[0].mu.Lock()
	s.procs[0].termErr = errors.New("抓包进程未退出")
	s.procs[0].mu.Unlock()
	if _, err := c.Stop(context.Background(), true); err == nil || !strings.Contains(err.Error(), "未退出") {
		t.Fatalf("终止失败应如实报错：%v", err)
	}
	if active, _ := c.Status(); active == nil {
		t.Fatalf("终止失败应保留会话（可重试）")
	}
	// 重试成功（走导出路径：终止 + 取走文件 + 摘登记）
	s.procs[0].mu.Lock()
	s.procs[0].termErr = nil
	s.procs[0].mu.Unlock()
	if _, err := c.Stop(context.Background(), true); err != nil {
		t.Fatalf("重试停止: %v", err)
	}
	if active, _ := c.Status(); active != nil {
		t.Fatalf("导出成功后应摘除会话：%+v", active)
	}
}

// 接口名守卫：以 `-` 开头（会被 tcpdump 当选项）与含非法字符的名字一律拒绝。
func TestCaptureGuardName(t *testing.T) {
	s := &fakeStarter{}
	c, _ := newTestCapture(t, kernelCfg(), s)
	for _, name := range []string{"-i", "ens/192", "a b"} {
		if err := c.Start(context.Background(), name, 10, ""); err == nil {
			t.Fatalf("名字 %q 应被拒绝", name)
		}
	}
	if len(s.calls) != 0 {
		t.Fatalf("被拒的名字不得启动进程：%+v", s.calls)
	}
}

// 进程自行退出（tcpdump `-c` 到达）后：状态如实给出「已抓 N 包」，会话仍可导出。
func TestCaptureStatusCapturedAfterSelfExit(t *testing.T) {
	s := &fakeStarter{}
	c, _ := newTestCapture(t, kernelCfg(), s)
	if err := c.Start(context.Background(), "ens192", 3, ""); err != nil {
		t.Fatalf("开始抓包: %v", err)
	}
	if active, _ := c.Status(); active == nil || active.Captured != 0 {
		t.Fatalf("进行中不应报计数（读数未知）：%+v", active)
	}
	s.procs[0].finish(nil, "3 packets captured\n3 packets received by filter\n")
	active, _ := c.Status()
	if active == nil || active.Captured != 3 {
		t.Fatalf("进程自行退出后应给 tcpdump 的摘要计数：%+v", active)
	}
	if n := captureCountFromOutput("123 packets captured"); n != 123 {
		t.Fatalf("摘要解析：%d", n)
	}
	if n := captureCountFromOutput("tcpdump: no packets captured"); n != 0 {
		t.Fatalf("解析不出应返回 0（不猜）：%d", n)
	}
}

// Provider 侧：抓包实现与 Provider 共用同一个 Runner 与配置来源。
func TestProviderCaptureProviderSharesConfig(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{prefix: "ip -j link show", out: `[{"ifname":"vs-l2"}]`}}}
	p := New(f)
	p.SetConfig(model.Config{VirtualSwitches: []model.VirtualSwitch{{Name: "vs-l2", Type: "l2"}}})
	c := p.CaptureProvider()
	if c != p.CaptureProvider() {
		t.Fatalf("同一 Provider 应复用同一抓包实现")
	}
	c.SetDir(t.TempDir())
	c.SetStarter(func(name string, args ...string) (CaptureProc, error) {
		return newFakeProc(1), nil
	})
	c.probe = time.Millisecond
	if err := c.Start(context.Background(), "vs-l2", 5, ""); err != nil {
		t.Fatalf("按 Provider 配置解析设备: %v", err)
	}
	// 配置来源随 Provider 变化（提交后读视图不滞后）
	p.SetConfig(model.Config{})
	// 决策 #465：会话要**取走**（export）后才不占登记位——stop 只冻结，留着会挡下一次 start（这是刻意的）。
	if _, err := c.Stop(context.Background(), true); err != nil {
		t.Fatalf("导出取走会话: %v", err)
	}
	if err := c.Start(context.Background(), "vs-l2", 5, ""); !errors.Is(err, network.ErrIfaceUnavailable) {
		t.Fatalf("配置来源变化后应拒绝已删设备：%v", err)
	}
}

// 计划性自检：导出文件名在同秒连开两次时不重名（不覆盖上一次的导出件）。
func TestCaptureUniqueFileName(t *testing.T) {
	c := NewCapture(&fakeRunner{})
	dir := t.TempDir()
	c.SetDir(dir)
	ts := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return ts }
	first := c.uniqueFileName("ens192")
	if first != "nfvis-cap-ens192-20261009T120000Z.pcap" {
		t.Fatalf("命名口径不符：%s", first)
	}
	if err := os.WriteFile(filepath.Join(dir, first), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := c.uniqueFileName("ens192")
	if second == first {
		t.Fatalf("同秒第二个会话应顺延时间戳，避免覆盖")
	}
	if second != fmt.Sprintf("nfvis-cap-ens192-%s.pcap", ts.Add(time.Second).Format("20060102T150405Z")) {
		t.Fatalf("顺延口径不符：%s", second)
	}
}

// ---------- 决策 #466：跨 nfvisd 重启收敛 ----------

// fakeProcs 宿主进程视野的注入桩：lines 为「存活进程」的 pid→命令行。
type fakeProcs struct {
	mu      sync.Mutex
	lines   map[int]string
	kills   []int
	killErr error
}

func (f *fakeProcs) scan() []procEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]procEntry, 0, len(f.lines))
	for pid, line := range f.lines {
		out = append(out, procEntry{PID: pid, Cmdline: line})
	}
	return out
}

func (f *fakeProcs) kill(pid int, sig syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kills = append(f.kills, pid)
	if f.killErr != nil {
		return f.killErr
	}
	delete(f.lines, pid) // 信号生效：进程消失
	return nil
}

func (f *fakeProcs) alive(pid int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.lines[pid]
	return ok
}

// 记录说「在抓」+ 遗留孤儿 tcpdump 还活着：重启收敛先按记录 pid + 命令行核对身份终止它，
// 再把工作文件重建为「已停止待导出」会话（数据经 export 取走）。
func TestCaptureReconcileTerminatesOrphanAndRecovers(t *testing.T) {
	c, _ := newTestCapture(t, kernelCfg(), &fakeStarter{})
	c.termGrace = 5 * time.Millisecond
	name := "nfvis-cap-ens192-20261012T010203Z.pcap"
	work := c.workPath(name)
	if err := os.WriteFile(work, []byte("orphan-pcap"), 0o600); err != nil {
		t.Fatal(err)
	}
	const pid = 4242
	procs := &fakeProcs{lines: map[int]string{pid: "tcpdump -i ens192 -w " + work + " -c 10"}}
	c.SetProcHooks(procs.scan, procs.kill, procs.alive)
	if err := network.SaveCaptureRecord(c.sessionsFile, network.CapturePlaneKernel, &network.CaptureSessionRecord{
		Interface: "ens192", Dev: "ens192", FileName: name, WorkPath: work,
		StartedAt: time.Now().UTC().Add(-time.Minute), Count: 10, PID: pid,
	}); err != nil {
		t.Fatal(err)
	}
	rep := c.Reconcile(context.Background())
	if len(procs.kills) == 0 || procs.alive(pid) {
		t.Fatalf("孤儿进程应被终止：kills=%v 报告=%v", procs.kills, rep)
	}
	active, files := c.Status()
	if active == nil || !active.Stopped || !active.Recovered || active.Interface != "ens192" || len(files) != 0 {
		t.Fatalf("应收敛为「已停止待导出」：%+v %v（报告 %v）", active, files, rep)
	}
	// 数据从暂存位取走（工作文件已被收敛搬进导出目录隐藏位）。
	f, err := c.Stop(context.Background(), true)
	if err != nil || f.Name != name || f.SizeBytes == 0 {
		t.Fatalf("恢复的数据应可导出：%+v %v", f, err)
	}
}

// 终止不掉（进程顽固存活）：收敛整体延后、记录不动；可终止后下一次操作自动补跑。
func TestCaptureReconcileDefersWhenOrphanStays(t *testing.T) {
	c, _ := newTestCapture(t, kernelCfg(), &fakeStarter{})
	c.termGrace = 5 * time.Millisecond
	name := "nfvis-cap-ens192-20261012T040506Z.pcap"
	work := c.workPath(name)
	if err := os.WriteFile(work, []byte("stubborn"), 0o600); err != nil {
		t.Fatal(err)
	}
	const pid = 4243
	procs := &fakeProcs{
		lines:   map[int]string{pid: "tcpdump -i ens192 -w " + work},
		killErr: errors.New("operation not permitted"),
	}
	c.SetProcHooks(procs.scan, procs.kill, procs.alive)
	if err := network.SaveCaptureRecord(c.sessionsFile, network.CapturePlaneKernel, &network.CaptureSessionRecord{
		Interface: "ens192", Dev: "ens192", FileName: name, WorkPath: work,
		StartedAt: time.Now().UTC().Add(-time.Minute), Count: 10, PID: pid,
	}); err != nil {
		t.Fatal(err)
	}
	rep := c.Reconcile(context.Background())
	if active, _ := c.Status(); active != nil {
		t.Fatalf("终止失败不应接管会话：%+v（报告 %v）", active, rep)
	}
	recs, _ := network.LoadCaptureRecords(c.sessionsFile)
	if recs[network.CapturePlaneKernel] == nil || recs[network.CapturePlaneKernel].Stopped {
		t.Fatalf("记录应保持「在抓」不动：%+v", recs)
	}
	// 进程可终止后：下一次抓包操作（Status）自动补跑。
	procs.mu.Lock()
	procs.killErr = nil
	procs.mu.Unlock()
	active, _ := c.Status()
	if active == nil || !active.Stopped || !active.Recovered {
		t.Fatalf("补跑应收敛为已停止待导出：%+v", active)
	}
}

// 无记录残件 + 仍在写的孤儿：残件扫描先有界终止写者，再把数据归档为可见 pcap。
func TestCaptureResidueSweepKillsWriterAndArchives(t *testing.T) {
	c, _ := newTestCapture(t, kernelCfg(), &fakeStarter{})
	c.termGrace = 5 * time.Millisecond
	name := "nfvis-cap-vs-l2-20261012T050607Z.pcap"
	work := c.workPath(name)
	if err := os.WriteFile(work, []byte("stray-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	const pid = 4321
	procs := &fakeProcs{lines: map[int]string{pid: "tcpdump -i vs-l2 -w " + work + " -c 1000"}}
	c.SetProcHooks(procs.scan, procs.kill, procs.alive)
	rep := c.Reconcile(context.Background())
	if procs.alive(pid) {
		t.Fatalf("仍在写的进程应被终止：kills=%v 报告=%v", procs.kills, rep)
	}
	files := c.List()
	if len(files) != 1 || files[0].Name != name {
		t.Fatalf("残件应按源名归档为可见文件：%v（报告 %v）", files, rep)
	}
	if _, err := os.Stat(work); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("残件应已搬走：%v", err)
	}
}
