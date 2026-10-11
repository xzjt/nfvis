package netkernel

// 内核数据面抓包（tcpdump）。
//
// 与 VPP 侧 network.CaptureProvider **同一契约形状**（状态/清单、开始、停止并导出、下载路径），
// 只是把底座从 VPP pcap trace 换成宿主的 `tcpdump -i <dev> -w <file> -c <count>`：
//   - 起：后台进程（登记 pid 与工作文件），工作文件落在**系统临时目录**、以 `.part` 结尾
//     （不参与清单）——⚠️ **不能落在导出目录**：宿主 AppArmor 的 tcpdump 配置不放行
//     `/var/lib/nfvis/captures`（真机实测 `Couldn't change ownership of savefile` /
//     `Permission denied`），而 `/tmp` 放行（VPP 侧同样走 `/tmp/rxtx.pcap` 再搬）。
//     导出时搬进导出目录并改名（跨设备时退化为复制 + 删除）；不导出则删除；
//   - 停：终止该进程（先 SIGTERM——tcpdump 收到后写完 pcap 缓冲并退出；宽限内不退再 SIGKILL）；
//   - 单会话：已有会话时再次 start 报 ErrCaptureActive（API 409），与 VPP 侧同口径；
//   - `filter <acl>` 不接（BPF 过滤会让两数据面语义分叉），一律按既有「不支持」口径报错；
//   - `count` 是 tcpdump 的 `-c`（**达到报文数自动停止**）——与 VPP 的「环形缓冲深度」不同，
//     读视图/手册如实说明；未给时缺省 1000（与 VPP 侧同值）。
//
// 抓包设备按接口名解析（内核 bridge / 物理口），两道判据都在 resolveDevice 里：
// 必须是**数据面设备**（配置声明的业务口，或产品自持的虚拟交换机/VRF/bond/隧道），
// 且**此刻在内核里存在**（被 DPDK 接管过的口内核里没有它）——任一不满足即如实拒绝，不换设备。
//
// 跨重启收敛（决策 #466）：会话登记仍只在**本进程内**，但 start/冻结/取走会把会话落进持久记录
// （`/var/lib/nfvis/capture-sessions.json`），nfvisd 启动时按记录收敛——重启前冻结的会话重建为
// 「已停止待导出」；重启前在抓的会话先终止遗留的孤儿 tcpdump（记录里有 pid，按命令行核对身份）
// 再重建；无记录的工作文件残件归档为可见 pcap。宿主上仍可用 `pgrep -a tcpdump` 独立复核。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// 抓包进程的有界口径。
const (
	// captureStartProbe 启动后的「立即失败」探测窗：tcpdump 起不来（无该设备/无权限/参数非法）
	// 会在毫秒级退出——这段窗口内异常退出即按启动失败报出，不把空会话留给操作者。
	captureStartProbe = 250 * time.Millisecond
	// captureTermGrace 终止宽限：SIGTERM 后等这么久，仍不退再 SIGKILL，再等同样久。
	captureTermGrace = 3 * time.Second
	// captureOutputLimit 进程输出保留上限（tcpdump 的 "N packets captured" 摘要行在末尾）。
	captureOutputLimit = 8192
	// captureDefaultCount 未指定 count 时的缺省值（与 VPP 侧同值）。
	captureDefaultCount = 1000
)

// CaptureProc 一个已启动的后台抓包进程。
//
// 抽成接口是为了**可注入**：单测用假实现校验命令形状与生命周期，不真跑 tcpdump。
type CaptureProc interface {
	// Pid 进程号（取不到返回 0）。
	Pid() int
	// Done 进程退出后关闭的通道。
	Done() <-chan struct{}
	// Err 进程退出结果（未退出时为 nil；异常退出返回错误）。
	Err() error
	// Output 进程至今的输出（tcpdump 的统计摘要在这里）。
	Output() string
	// Terminate 终止进程并确认其退出（幂等：已退出直接返回 nil）。
	Terminate() error
}

// ProcStarter 启动后台抓包进程（可注入）。
type ProcStarter func(name string, args ...string) (CaptureProc, error)

// Capture 内核数据面抓包实现。
type Capture struct {
	run   Runner      // ip 命令（设备解析）
	start ProcStarter // 后台进程启动
	dir   string      // 导出目录
	cfg   func() model.Config
	now   func() time.Time

	// probe 启动后探测窗（测试可缩短；0 即不探测）。
	probe time.Duration

	// 决策 #466：跨 nfvisd 重启收敛。
	sessionsFile string             // 持久会话记录（两个数据面共用一个文件）
	tmp          string             // 工作文件目录（缺省系统临时目录；测试注入）
	termGrace    time.Duration      // 孤儿进程终止宽限（缺省 captureTermGrace；测试可缩短）
	scanProcs    func() []procEntry // 进程扫描（孤儿 tcpdump 判定；测试注入）
	killPID      func(pid int, sig syscall.Signal) error
	pidAlive     func(pid int) bool

	mu     sync.Mutex
	active *captureSession

	rmu              sync.Mutex // 收敛状态（与 mu 分开：收敛里会有有界等待，不能压着会话锁）
	reconcilePending bool       // 启动收敛未完成（孤儿终止失败等）——下次抓包操作补跑
}

// captureSession 抓包会话（进程内登记；退出即摘除，由持久记录支撑跨进程收敛——决策 #466）。
// ⚠️ 决策 #465：`stop` 后会话**不摘除**——标成 stopped（数据保留待导出），由 export 收尾。
// ⚠️ 决策 #466：冻结即把工作文件暂存进导出目录隐藏位（`path` 随之指向暂存位）。
type captureSession struct {
	ifname    string // 用户给出的接口名（读视图展示用）
	dev       string // 内核设备名（tcpdump -i 的目标）
	fileName  string // 导出文件名（沿用既有口径：nfvis-cap-<if>-<UTC>.pcap）
	path      string // 工作文件当前路径（在抓时＝临时目录 .part；冻结暂存后＝导出目录隐藏位）
	proc      CaptureProc
	started   time.Time
	count     int
	stopped   bool // 已停止采集、数据保留待导出（决策 #465）
	recovered bool // 由 nfvisd 启动时的重启收敛重建（决策 #466）
}

// NewCapture 构造内核数据面抓包实现（run 为 nil 时用真实宿主命令）。
func NewCapture(run Runner) *Capture {
	if run == nil {
		run = NewExecRunner()
	}
	return &Capture{
		run:              run,
		start:            startExecProc,
		dir:              network.DefaultCaptureDir,
		cfg:              func() model.Config { return model.Config{} },
		now:              time.Now,
		probe:            captureStartProbe,
		sessionsFile:     network.DefaultCaptureSessionsFile,
		tmp:              os.TempDir(),
		termGrace:        captureTermGrace,
		scanProcs:        defaultScanProcs,
		killPID:          defaultKillPID,
		pidAlive:         defaultPIDAlive,
		reconcilePending: true,
	}
}

// SetConfigSource 注入「当前 committed 配置」的来源（装配处接引擎）。
func (c *Capture) SetConfigSource(src func() (model.Config, error)) {
	if src == nil {
		return
	}
	c.cfg = func() model.Config {
		if cfg, err := src(); err == nil {
			return cfg
		}
		return model.Config{}
	}
}

// SetDir 覆盖导出目录（测试/工具用）。
func (c *Capture) SetDir(dir string) {
	if strings.TrimSpace(dir) != "" {
		c.dir = dir
	}
}

// SetStarter 覆盖后台进程启动器（测试用）。
func (c *Capture) SetStarter(s ProcStarter) {
	if s != nil {
		c.start = s
	}
}

// SetSessionsFile 覆盖会话记录文件（测试用）。
func (c *Capture) SetSessionsFile(path string) {
	if strings.TrimSpace(path) != "" {
		c.sessionsFile = path
	}
}

// SetWorkDir 覆盖工作文件目录（测试用；与 os.TempDir() 解耦）。
func (c *Capture) SetWorkDir(dir string) {
	if strings.TrimSpace(dir) != "" {
		c.tmp = dir
	}
}

// SetProcHooks 覆盖进程扫描/信号/存活判定（测试用）。
func (c *Capture) SetProcHooks(scan func() []procEntry, kill func(int, syscall.Signal) error, alive func(int) bool) {
	if scan != nil {
		c.scanProcs = scan
	}
	if kill != nil {
		c.killPID = kill
	}
	if alive != nil {
		c.pidAlive = alive
	}
}

// Dir 导出目录。
func (c *Capture) Dir() string { return c.dir }

// Status 返回活动会话与已导出文件（契约 CaptureStatus）。
//
// 会话的 captured 计数只在进程**已自行退出**（tcpdump 的 `-c` 到达）时给得出——取 tcpdump
// 打印的摘要；进行中一律 0（读数未知，不谎报计数）。
func (c *Capture) Status() (*network.CaptureSession, []network.CaptureFile) {
	c.tryReconcile(context.Background()) // 决策 #466：重启收敛未完成时补跑（读视图即触发点之一）
	c.mu.Lock()
	var active *network.CaptureSession
	if c.active != nil {
		active = &network.CaptureSession{
			Interface: c.active.ifname,
			Captured:  c.active.captured(),
			StartedAt: c.active.started,
			MaxDepth:  c.active.count,
			Stopped:   c.active.stopped, // 决策 #465：已停止待导出（读视图如实暴露）
			Recovered: c.active.recovered,
		}
	}
	c.mu.Unlock()
	return active, c.List()
}

// captured 已抓包数：进程已退出时取 tcpdump 摘要，进行中为 0（未知）。
func (s *captureSession) captured() int {
	select {
	case <-s.proc.Done():
		return captureCountFromOutput(s.proc.Output())
	default:
		return 0
	}
}

// Start 开始抓包：`tcpdump -i <dev> -w <file> -c <count>`（后台进程）。
func (c *Capture) Start(ctx context.Context, ifname string, count int, filterACL string) error {
	c.tryReconcile(ctx) // 决策 #466：重启收敛未完成时先补跑（恢复的冻结会话会经下方守卫拦下新会话）
	if strings.TrimSpace(ifname) == "" {
		return errors.New("抓包接口必填")
	}
	if filterACL != "" {
		// 与 VPP 侧同一错误值（API 按 400 分类），文案按内核口径补充说明。
		return fmt.Errorf("%w；内核数据面本轮不接 BPF 过滤（两数据面的过滤语义保持一致）",
			network.ErrCaptureFilterUnsupported)
	}
	if count <= 0 {
		count = captureDefaultCount
	}
	c.mu.Lock()
	if c.active != nil {
		cur, stopped := c.active.ifname, c.active.stopped
		c.mu.Unlock()
		if stopped {
			// 决策 #465：冻结中的会话仍占登记位——如实说清并给照做路径。
			return fmt.Errorf("已有已停止待导出的抓包会话（接口 %s）：请先 request capture export 取走它，再开始新抓包", cur)
		}
		return fmt.Errorf("%w（接口 %s）", network.ErrCaptureActive, cur)
	}
	c.mu.Unlock()

	dev, err := c.resolveDevice(ctx, ifname)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return fmt.Errorf("创建抓包导出目录 %s: %w", c.dir, err)
	}
	fileName := c.uniqueFileName(dev)
	work := c.workPath(fileName)
	_ = os.Remove(work) // 同名残留（上次会话异常结束）先清掉，避免导出到上一次的内容

	proc, err := c.start("tcpdump", "-i", dev, "-w", work, "-c", strconv.Itoa(count))
	if err != nil {
		return fmt.Errorf("启动抓包进程（tcpdump -i %s -w %s -c %d）: %w（宿主是否已安装 tcpdump？）",
			dev, work, count, err)
	}
	if err := c.probeStart(proc); err != nil {
		_ = os.Remove(work)
		return fmt.Errorf("开始抓包失败（tcpdump -i %s）: %w%s", dev, err, captureOutputHint(proc.Output()))
	}
	c.mu.Lock()
	c.active = &captureSession{
		ifname: ifname, dev: dev, fileName: fileName, path: work,
		proc: proc, started: c.now().UTC(), count: count,
	}
	sess := c.active
	c.mu.Unlock()
	c.saveRecord(sess) // 决策 #466：登记落盘（跨 nfvisd 重启可收敛；含孤儿判定用的 pid）
	return nil
}

// probeStart 启动后的立即失败探测：进程在探测窗内**异常退出**（非零）即按启动失败报出。
func (c *Capture) probeStart(proc CaptureProc) error {
	if c.probe <= 0 {
		return nil
	}
	select {
	case <-proc.Done():
		if err := proc.Err(); err != nil {
			return err
		}
		return nil // 正常退出（如 count 很小、已抓够）：会话照常登记，文件可供导出
	case <-time.After(c.probe):
		return nil
	}
}

// Stop 停止抓包。export=true 时把工作文件搬入导出目录并登记（**隐含 stop**，契约语义不变）；
// export=false 则**停止并冻结**（决策 #465：保留已抓数据、会话留在登记里待 export——
// 原实现这里 `os.Remove` 会把数据丢掉；决策 #466：冻结即把工作文件暂存进导出目录隐藏位）。
// 导出失败时**保留会话登记**（工作文件还在，操作者可重试），不假装已导出。
func (c *Capture) Stop(ctx context.Context, export bool) (network.CaptureFile, error) {
	c.tryReconcile(ctx) // 决策 #466：重启收敛未完成时先补跑（恢复出的冻结会话可在这里被 export 取走）
	c.mu.Lock()
	sess := c.active
	c.mu.Unlock()
	if sess == nil {
		return network.CaptureFile{}, network.ErrNoCapture
	}
	if !sess.stopped {
		if err := sess.proc.Terminate(); err != nil {
			// 进程没停干净：会话登记保留（重试仍能停），如实报错。
			return network.CaptureFile{}, fmt.Errorf("停止抓包（tcpdump pid %d，接口 %s，设备 %s）: %w",
				sess.proc.Pid(), sess.ifname, sess.dev, err)
		}
		c.mu.Lock()
		if s := c.active; s == sess {
			s.stopped = true
		}
		c.mu.Unlock()
	}
	if !export {
		// 冻结：决策 #466 起把数据暂存进导出目录隐藏位（宿主重启/临时目录清理都不丢），
		// 随后 export 在其上做同目录改名。暂存失败保留原位（不阻断；导出路径两处都认）。
		c.stageStopped(sess)
		c.saveRecord(sess)
		return network.CaptureFile{}, nil
	}
	info, err := os.Stat(sess.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 一个包也没抓到（tcpdump 不建文件）：无文件导出，与 VPP 侧同口径。
			c.clearActive(sess)
			c.clearRecord()
			return network.CaptureFile{}, nil
		}
		return network.CaptureFile{}, fmt.Errorf("读取抓包文件 %s: %w", sess.path, err)
	}
	if info.Size() == 0 {
		_ = os.Remove(sess.path)
		c.clearActive(sess)
		c.clearRecord()
		return network.CaptureFile{}, nil
	}
	dst := filepath.Join(c.dir, sess.fileName)
	if err := moveFile(sess.path, dst); err != nil {
		return network.CaptureFile{}, fmt.Errorf("导出 pcap（%s → %s）: %w", sess.path, dst, err)
	}
	st, err := os.Stat(dst)
	if err != nil {
		return network.CaptureFile{}, fmt.Errorf("导出 pcap 后回读 %s: %w", dst, err)
	}
	c.clearActive(sess)
	c.clearRecord()
	return network.CaptureFile{Name: sess.fileName, SizeBytes: st.Size(), CreatedAt: st.ModTime().UTC()}, nil
}

// stageStopped 冻结暂存：把工作文件搬进导出目录的隐藏暂存位。尽力而为——失败保留原位、
// 不阻断冻结（导出路径对原位与暂存位都认）。
func (c *Capture) stageStopped(sess *captureSession) {
	staged := c.stagedPath(sess.fileName)
	if sess.path == staged {
		return // 已暂存（冻结中再 stop 的幂等路径）
	}
	if _, err := os.Stat(sess.path); err != nil {
		return // 无数据文件：无物可暂存
	}
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return
	}
	if err := moveFile(sess.path, staged); err != nil {
		return
	}
	sess.path = staged
}

// saveRecord 把当前会话落盘（决策 #466；写失败不阻断抓包——退化为进程内登记）。
func (c *Capture) saveRecord(sess *captureSession) {
	_ = network.SaveCaptureRecord(c.sessionsFile, network.CapturePlaneKernel, &network.CaptureSessionRecord{
		Interface: sess.ifname, Dev: sess.dev, FileName: sess.fileName, WorkPath: sess.path,
		StartedAt: sess.started, Count: sess.count, Stopped: sess.stopped, PID: sess.proc.Pid(),
	})
}

func (c *Capture) clearRecord() {
	_ = network.SaveCaptureRecord(c.sessionsFile, network.CapturePlaneKernel, nil)
}

// clearActive 摘除**仍属本次会话**的登记（避免并发路径把新会话误清）。
func (c *Capture) clearActive(sess *captureSession) {
	c.mu.Lock()
	if c.active == sess {
		c.active = nil
	}
	c.mu.Unlock()
}

// ---------- 决策 #466：跨 nfvisd 重启收敛 ----------

// tryReconcile 启动收敛未完成时的补跑（如孤儿进程未终止成功）——在抓包操作入口触发。
// 报告在补跑路径没有落点——可见信号就是读视图里出现的恢复会话/归档文件。
func (c *Capture) tryReconcile(ctx context.Context) {
	c.rmu.Lock()
	need := c.reconcilePending
	c.rmu.Unlock()
	if !need {
		return
	}
	_ = c.Reconcile(ctx)
}

// Reconcile 启动收敛（决策 #466）：把重启前冻结/在抓的会话按持久记录重建为「已停止待导出」，
// 并把无记录的工作文件残件归档为可见 pcap。返回供 journal 记录的逐条报告。
//
// 语义（逐条都有单测钉住）：
//   - 记录说有 pid 且进程还活着（命令行与本会话工作文件匹配——防 pid 复用）⇒ 先有界终止
//     （SIGTERM→宽限→SIGKILL）；终止不掉则整体延后（待下次抓包操作重试），记录保持不动。
//   - 数据文件（记录路径 / 工作位 / 暂存位，取第一个存在的）非空 ⇒ 重建为冻结会话
//     （Recovered=true）；更新记录（stopped=true、work_path=实际位置）。
//   - 数据文件不存在/为空 ⇒ 清记录：在抓态报「已关闭、无捕获数据」；冻结态报「已无可导出数据」。
//   - 其余工作文件（无记录残件：旧版本遗留 / 记录写失败 / 换数据面）⇒ 先终止仍在写的进程，
//     非空归档为可见 pcap（不藏不删），空文件清掉。
func (c *Capture) Reconcile(ctx context.Context) []string {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	var rep []string

	// 本进程已有会话（补跑竞态）：不重复接管，会话结束后下次操作再收敛。
	c.mu.Lock()
	live := c.active != nil
	c.mu.Unlock()
	if live {
		return []string{"已有进行中的抓包会话，重启收敛跳过（会话结束后下次操作重试）"}
	}

	recs, err := network.LoadCaptureRecords(c.sessionsFile)
	if err != nil {
		rep = append(rep, "抓包会话记录无法解析（按无记录处理）："+err.Error())
	}
	rec := recs[network.CapturePlaneKernel]
	incomplete := false
	var adoptedWork string
	if rec != nil {
		// ① 孤儿 tcpdump：记录里有 pid——仍要按命令行核对身份（防 pid 复用）后才有界终止。
		if rec.PID > 0 && c.pidAlive(rec.PID) {
			if c.procMatches(rec.PID, rec.WorkPath) {
				if err := c.terminatePID(rec.PID); err != nil {
					rep = append(rep, fmt.Sprintf("重启前遗留的抓包进程 %d 未能终止（%v），收敛延后（下次抓包操作自动重试）",
						rec.PID, err))
					incomplete = true
				} else {
					rep = append(rep, fmt.Sprintf("已终止重启前遗留的抓包进程 %d（tcpdump，数据已写入工作文件）", rec.PID))
				}
			}
		}
	}
	if rec != nil && !incomplete {
		src := ""
		var size int64
		for _, cand := range c.recordFileCandidates(rec) {
			if cand != "" && c.exists(cand) {
				if info, serr := os.Stat(cand); serr == nil {
					src, size = cand, info.Size()
					break
				}
			}
		}
		if src != "" && size > 0 {
			work := src
			staged := c.stagedPath(rec.FileName)
			if src != staged {
				if err := os.MkdirAll(c.dir, 0o755); err == nil {
					if err := moveFile(src, staged); err == nil {
						work = staged
					}
				}
			}
			c.mu.Lock()
			c.active = &captureSession{
				ifname: rec.Interface, dev: rec.Dev, fileName: rec.FileName, path: work,
				proc: deadProc{}, started: rec.StartedAt, count: rec.Count,
				stopped: true, recovered: true,
			}
			c.mu.Unlock()
			rec.Stopped, rec.WorkPath = true, work
			_ = network.SaveCaptureRecord(c.sessionsFile, network.CapturePlaneKernel, rec)
			adoptedWork = work
			rep = append(rep, fmt.Sprintf("已恢复重启前的抓包会话（接口 %s，数据待导出 %s）：request capture export 取走",
				rec.Interface, rec.FileName))
		} else {
			_ = network.SaveCaptureRecord(c.sessionsFile, network.CapturePlaneKernel, nil)
			if rec.Stopped {
				rep = append(rep, "重启前冻结的抓包会话已无可导出数据（未捕获到报文或数据文件已被清理），登记已清除")
			} else {
				rep = append(rep, "重启前的在抓会话已关闭（无捕获数据）")
			}
		}
	}

	if incomplete {
		rep = append(rep, "无记录残件扫描延后（与孤儿终止同批重试）")
	} else {
		rep = append(rep, c.sweepWorkFiles(adoptedWork)...)
	}

	c.reconcilePending = incomplete
	return rep
}

// recordFileCandidates 记录指向的数据文件候选（按优先级）：记录路径 → 临时工作位 → 暂存位。
func (c *Capture) recordFileCandidates(rec *network.CaptureSessionRecord) []string {
	return []string{rec.WorkPath, c.workPath(rec.FileName), c.stagedPath(rec.FileName)}
}

// procMatches pid 进程的命令行是否与抓包工作文件匹配（防 pid 复用；hint 空则只认 tcpdump）。
func (c *Capture) procMatches(pid int, hint string) bool {
	for _, p := range c.scanProcs() {
		if p.PID != pid {
			continue
		}
		if !strings.Contains(p.Cmdline, "tcpdump") {
			return false
		}
		return hint == "" || strings.Contains(p.Cmdline, hint)
	}
	return false
}

// terminatePID 有界终止一个宿主进程：SIGTERM → 宽限 → SIGKILL → 宽限。
func (c *Capture) terminatePID(pid int) error {
	_ = c.killPID(pid, syscall.SIGTERM)
	if c.waitPIDGone(pid, c.termGrace) {
		return nil
	}
	_ = c.killPID(pid, syscall.SIGKILL)
	if c.waitPIDGone(pid, c.termGrace) {
		return nil
	}
	return fmt.Errorf("进程 %d 在 %s 内未退出（已发送 SIGTERM 与 SIGKILL）", pid, c.termGrace)
}

func (c *Capture) waitPIDGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if !c.pidAlive(pid) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// sweepWorkFiles 扫描两个工作位（临时目录 + 导出目录）里的工作文件残件。
// skip 是本次已采纳的会话工作文件（不动它）。
func (c *Capture) sweepWorkFiles(skip string) []string {
	var rep []string
	dirs := []string{c.tmp, c.dir}
	seen := map[string]bool{}
	for _, dir := range dirs {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			base, ok := network.CaptureWorkBase(e.Name())
			if !ok {
				continue
			}
			full := filepath.Join(dir, e.Name())
			if full == skip {
				continue
			}
			// 仍在写的进程（本进程内的会话不算——上面已判 live）：先有界终止，再归档。
			if c.killWriterOf(full) != nil {
				rep = append(rep, fmt.Sprintf("残件 %s 仍有进程在写（终止失败），本次不动它", full))
				continue
			}
			rep = append(rep, c.archiveResidue(full, base)...)
		}
	}
	return rep
}

// killWriterOf 终止命令行包含 path 的 tcpdump（没有则 nil）。
func (c *Capture) killWriterOf(path string) error {
	for _, p := range c.scanProcs() {
		if p.PID <= 0 || !strings.Contains(p.Cmdline, "tcpdump") || !strings.Contains(p.Cmdline, path) {
			continue
		}
		if err := c.terminatePID(p.PID); err != nil {
			return err
		}
	}
	return nil
}

// archiveResidue 把无记录的工作文件归档为可见 pcap（非空）或清掉（空）。
func (c *Capture) archiveResidue(full, base string) []string {
	info, err := os.Stat(full)
	if err != nil {
		return nil
	}
	if info.Size() == 0 {
		if err := os.Remove(full); err == nil {
			return []string{"清理空的工作文件残件 " + full}
		}
		return nil
	}
	name := network.UniquePcapName(c.dir, base)
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return []string{fmt.Sprintf("归档残件 %s：创建导出目录失败: %v", full, err)}
	}
	if err := moveFile(full, filepath.Join(c.dir, name)); err != nil {
		return []string{fmt.Sprintf("归档残件 %s 失败: %v", full, err)}
	}
	return []string{fmt.Sprintf("无记录残件已归档为 %s（源 %s）", name, full)}
}

// deadProc 已死进程的桩（重建的会话不持有进程句柄——它已停止，不参与 Terminate）。
// captured() 取不到输出 → 0；Terminate() 视为已退出（幂等语义）。
type deadProc struct{}

func (deadProc) Pid() int              { return 0 }
func (deadProc) Done() <-chan struct{} { c := make(chan struct{}); close(c); return c }
func (deadProc) Err() error            { return nil }
func (deadProc) Output() string        { return "" }
func (deadProc) Terminate() error      { return nil }

// Path 解析导出文件路径（限定目录内，防穿越）。
func (c *Capture) Path(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") || !strings.HasSuffix(name, ".pcap") {
		return "", fmt.Errorf("%w: %q", network.ErrCaptureNotFound, name)
	}
	full := filepath.Join(c.dir, name)
	if _, err := os.Stat(full); err != nil {
		return "", fmt.Errorf("%w: %s", network.ErrCaptureNotFound, name)
	}
	return full, nil
}

// List 已导出 pcap（创建时间倒序）。
func (c *Capture) List() []network.CaptureFile {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return []network.CaptureFile{}
	}
	out := make([]network.CaptureFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pcap") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, network.CaptureFile{Name: e.Name(), SizeBytes: info.Size(), CreatedAt: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// workPath 工作文件路径：导出目录内、以 `.part` 结尾（清单只认 `.pcap`，故不出现在读视图里）。
// 与导出文件同目录，导出即同目录改名——原子、不跨设备。
// workPath 工作文件路径：**系统临时目录**——宿主 AppArmor 的 tcpdump 配置只放行 /tmp 一类
// 路径、不放行产品的导出目录（真机实测落导出目录会被拒），导出时再搬进 c.dir。
// （决策 #466：冻结即把它暂存进导出目录隐藏位——搬移由 nfvisd 做，不经 tcpdump，AppArmor 不适用。）
func (c *Capture) workPath(fileName string) string {
	return filepath.Join(c.tmp, "."+fileName+".part")
}

// stagedPath 冻结暂存位（导出目录内、隐藏、`.part` 结尾——不参与已导出清单）。
func (c *Capture) stagedPath(fileName string) string {
	return filepath.Join(c.dir, "."+fileName+".part")
}

// moveFile 把工作文件搬进导出目录：优先同设备改名；跨设备（/tmp 与 /var 不同挂载）时
// 退化为「复制 + 删除源」。复制失败如实返回（源保留，可重试）。
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return os.Remove(src)
}

// uniqueFileName 导出文件名（沿用既有口径 `nfvis-cap-<if>-<UTC>.pcap`）。
//
// 同一秒内连开两次会话（脚本场景）时顺延时间戳：否则第二次导出会覆盖第一次的文件。
func (c *Capture) uniqueFileName(dev string) string {
	ts := c.now().UTC()
	for i := 0; i < 60; i++ {
		name := fmt.Sprintf("nfvis-cap-%s-%s.pcap", dev, ts.Add(time.Duration(i)*time.Second).Format("20060102T150405Z"))
		if !c.exists(filepath.Join(c.dir, name)) && !c.exists(c.workPath(name)) && !c.exists(c.stagedPath(name)) {
			return name
		}
	}
	return fmt.Sprintf("nfvis-cap-%s-%s.pcap", dev, ts.Format("20060102T150405Z"))
}

func (c *Capture) exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// kernelIfaceUnavailable 内核数据面版的「接口不在数据面」错误。
//
// 复用既有哨兵值（`errors.Is` 判定与 API 分类不变），但文案按**内核口径**：哨兵自身的文案是
// VPP 口径（「接口在 VPP 中不存在」），直接 `%w` 包一层会给操作者一句自相矛盾的话
// （内核数据面下根本没有 VPP）。Unwrap 仍指向同一个哨兵，故提交编排/API 的既有判定不受影响。
type kernelIfaceUnavailable struct{ msg string }

func (e *kernelIfaceUnavailable) Error() string { return e.msg }
func (e *kernelIfaceUnavailable) Unwrap() error { return network.ErrIfaceUnavailable }

// resolveDevice 把产品侧接口名解析为内核设备名。
//
// 两道判据（任一不过即按既有 ErrIfaceUnavailable 口径**如实拒绝**，不换设备、不静默成功）：
//   - 该名字是**数据面设备**：配置声明的业务口，或产品自持的虚拟设备（虚拟交换机 bridge /
//     L3 交换机 VRF / bond / VXLAN / vlan 子接口）。宿主机自己的网口（管理口、virbr0/docker0…）
//     不是数据面设备，不可抓包；
//   - 该设备**此刻在内核里存在**：被 DPDK 接管过的口内核里没有它——报错要点名照做路径。
func (c *Capture) resolveDevice(ctx context.Context, ifname string) (string, error) {
	if err := guardCaptureName(ifname); err != nil {
		return "", err
	}
	dev := LinkName(ifname)
	cfg := c.cfg()
	if !isCaptureDevice(cfg, dev) {
		return "", &kernelIfaceUnavailable{msg: fmt.Sprintf(
			"抓包接口 %s 不在内核数据面（须是配置中声明的业务口，或虚拟交换机/VRF/bond/隧道等数据面设备）", ifname)}
	}
	names, err := c.kernelLinkNames(ctx)
	if err != nil {
		return "", fmt.Errorf("读取内核接口清单失败（无法确认 %s 是否在内核数据面）: %w", dev, err)
	}
	if !names[dev] {
		return "", &kernelIfaceUnavailable{msg: fmt.Sprintf(
			"%s 不在内核中（可能仍被 DPDK 驱动占用——先 `request interfaces %s unbind-dpdk` 交还内核）", dev, dev)}
	}
	return dev, nil
}

// kernelLinkNames 内核当前的链路名集合（`ip -j link show`）。
func (c *Capture) kernelLinkNames(ctx context.Context) (map[string]bool, error) {
	out, err := c.run.Run(ctx, "ip", "-j", "link", "show")
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Ifname string `json:"ifname"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("解析 `ip -j link show` 输出: %w", err)
	}
	names := make(map[string]bool, len(rows))
	for _, r := range rows {
		if r.Ifname != "" {
			names[r.Ifname] = true
		}
	}
	return names, nil
}

// isCaptureDevice 该内核设备名是否属于数据面（配置声明的业务口 ∪ 产品自持的虚拟设备）。
func isCaptureDevice(cfg model.Config, dev string) bool {
	for _, iface := range cfg.Interfaces {
		if LinkName(iface.Name) == dev {
			return true
		}
	}
	return managedDeviceNames(cfg)[dev]
}

// guardCaptureName 拒绝会破坏参数语义的接口名（这些值原样进 tcpdump 的 argv）。
func guardCaptureName(name string) error {
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("接口名 %q 以 - 开头：会被 tcpdump 当作选项解析，请给出接口名（如 ens192 或虚拟交换机名）", name)
	}
	if strings.ContainsAny(name, "/\\ \t") || strings.Contains(name, "..") {
		return fmt.Errorf("接口名 %q 含非法字符：请给出接口名（如 ens192 或虚拟交换机名）", name)
	}
	return nil
}

// captureCountRe tcpdump 退出时的统计摘要（`N packets captured`；进程 locale 为 C 时如此）。
var captureCountRe = regexp.MustCompile(`(\d+)\s+packets?\s+captured`)

// captureCountFromOutput 从 tcpdump 输出里取已抓包数；解析不出返回 0（不猜）。
func captureCountFromOutput(out string) int {
	m := captureCountRe.FindStringSubmatch(out)
	if len(m) != 2 {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// captureOutputHint 把进程输出拼成可读提示（空输出返回空串）。
func captureOutputHint(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	return "（tcpdump 输出：" + out + "）"
}

// CaptureProvider 内核数据面的抓包实现（装配处按数据面选择 Provider 时用）。
//
// 与 Provider **共用同一个 Runner 与配置来源**：抓包设备解析要按当前 committed 配置判断
// 「这个口是不是数据面设备」，来源必须是同一条（否则提交后读视图会滞后一拍）。
func (p *Provider) CaptureProvider() *Capture {
	if p.capture == nil {
		c := NewCapture(p.run)
		c.SetConfigSource(func() (model.Config, error) { return p.config(), nil })
		p.capture = c
	}
	return p.capture
}

// ---------- 真实后台进程（exec） ----------

// startExecProc 启动一个真实后台进程（抽取为函数，便于装配处与测试替换）。
func startExecProc(name string, args ...string) (CaptureProc, error) {
	cmd := exec.Command(name, args...)
	buf := newTailBuffer(captureOutputLimit)
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &execCaptureProc{cmd: cmd, out: buf, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		close(p.done)
	}()
	return p, nil
}

type execCaptureProc struct {
	cmd  *exec.Cmd
	out  *tailBuffer
	done chan struct{}
	mu   sync.Mutex
	err  error
}

func (p *execCaptureProc) Pid() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *execCaptureProc) Done() <-chan struct{} { return p.done }

func (p *execCaptureProc) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *execCaptureProc) Output() string { return p.out.String() }

// Terminate：SIGTERM（tcpdump 收到后写完 pcap 缓冲并退出）→ 宽限 → SIGKILL → 宽限。
// 成功与否只看「进程是否确认退出」，不看退出码——被我们杀掉的进程退出码本就非零。
func (p *execCaptureProc) Terminate() error {
	select {
	case <-p.done:
		return nil
	default:
	}
	if p.cmd.Process == nil {
		return errors.New("抓包进程句柄无效（无进程）")
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	if p.waitExit(captureTermGrace) {
		return nil
	}
	_ = p.cmd.Process.Kill()
	if p.waitExit(captureTermGrace) {
		return nil
	}
	return fmt.Errorf("抓包进程 %d 在 %s 内未退出（已发送 SIGTERM 与 SIGKILL）", p.Pid(), captureTermGrace)
}

func (p *execCaptureProc) waitExit(d time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(d):
		return false
	}
}

// tailBuffer 有界输出缓冲（只保留最后 limit 字节；tcpdump 的摘要行在末尾）。
type tailBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func newTailBuffer(limit int) *tailBuffer { return &tailBuffer{limit: limit} }

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.limit {
		b.buf = append([]byte(nil), b.buf[len(b.buf)-b.limit:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
