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
// 如实边界（登记的已知限制）：会话登记只在**本进程内**。若 nfvisd 在抓包进行中被重启，
// 那个 tcpdump 会变成孤儿进程继续写工作文件（`.part`），重启后的读视图看不到它——
// 与 VPP 侧同族（那边的 `pcap trace` 同样只有重启数据面才停）。宿主上可用
// `pgrep -a tcpdump` 复核并手工处置。

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

	mu     sync.Mutex
	active *captureSession
}

// captureSession 活动抓包会话（进程内登记；退出即摘除，不跨进程存活）。
type captureSession struct {
	ifname   string // 用户给出的接口名（读视图展示用）
	dev      string // 内核设备名（tcpdump -i 的目标）
	fileName string // 导出文件名（沿用既有口径：nfvis-cap-<if>-<UTC>.pcap）
	path     string // 工作文件绝对路径（导出目录内、.part 结尾）
	proc     CaptureProc
	started  time.Time
	count    int
}

// NewCapture 构造内核数据面抓包实现（run 为 nil 时用真实宿主命令）。
func NewCapture(run Runner) *Capture {
	if run == nil {
		run = NewExecRunner()
	}
	return &Capture{
		run:   run,
		start: startExecProc,
		dir:   network.DefaultCaptureDir,
		cfg:   func() model.Config { return model.Config{} },
		now:   time.Now,
		probe: captureStartProbe,
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

// Dir 导出目录。
func (c *Capture) Dir() string { return c.dir }

// Status 返回活动会话与已导出文件（契约 CaptureStatus）。
//
// 会话的 captured 计数只在进程**已自行退出**（tcpdump 的 `-c` 到达）时给得出——取 tcpdump
// 打印的摘要；进行中一律 0（读数未知，不谎报计数）。
func (c *Capture) Status() (*network.CaptureSession, []network.CaptureFile) {
	c.mu.Lock()
	var active *network.CaptureSession
	if c.active != nil {
		active = &network.CaptureSession{
			Interface: c.active.ifname,
			Captured:  c.active.captured(),
			StartedAt: c.active.started,
			MaxDepth:  c.active.count,
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
		active := c.active.ifname
		c.mu.Unlock()
		return fmt.Errorf("%w（接口 %s）", network.ErrCaptureActive, active)
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
	c.mu.Unlock()
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
// false 则丢弃。导出失败时**保留会话登记**（工作文件还在，操作者可重试），不假装已导出。
func (c *Capture) Stop(_ context.Context, export bool) (network.CaptureFile, error) {
	c.mu.Lock()
	sess := c.active
	c.mu.Unlock()
	if sess == nil {
		return network.CaptureFile{}, network.ErrNoCapture
	}
	if err := sess.proc.Terminate(); err != nil {
		// 进程没停干净：会话登记保留（重试仍能停），如实报错。
		return network.CaptureFile{}, fmt.Errorf("停止抓包（tcpdump pid %d，接口 %s，设备 %s）: %w",
			sess.proc.Pid(), sess.ifname, sess.dev, err)
	}
	if !export {
		if err := os.Remove(sess.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return network.CaptureFile{}, fmt.Errorf("停止抓包后清理工作文件 %s: %w", sess.path, err)
		}
		c.clearActive(sess)
		return network.CaptureFile{}, nil
	}
	info, err := os.Stat(sess.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 一个包也没抓到（tcpdump 不建文件）：无文件导出，与 VPP 侧同口径。
			c.clearActive(sess)
			return network.CaptureFile{}, nil
		}
		return network.CaptureFile{}, fmt.Errorf("读取抓包文件 %s: %w", sess.path, err)
	}
	if info.Size() == 0 {
		_ = os.Remove(sess.path)
		c.clearActive(sess)
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
	return network.CaptureFile{Name: sess.fileName, SizeBytes: st.Size(), CreatedAt: st.ModTime().UTC()}, nil
}

// clearActive 摘除**仍属本次会话**的登记（避免并发路径把新会话误清）。
func (c *Capture) clearActive(sess *captureSession) {
	c.mu.Lock()
	if c.active == sess {
		c.active = nil
	}
	c.mu.Unlock()
}

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
func (c *Capture) workPath(fileName string) string {
	return filepath.Join(os.TempDir(), "."+fileName+".part")
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
		if !c.exists(filepath.Join(c.dir, name)) && !c.exists(c.workPath(name)) {
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
