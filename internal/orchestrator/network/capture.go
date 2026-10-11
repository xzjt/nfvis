package network

// M5-3：数据面抓包（VPP pcap trace，FR-OPS-042）。
//
// 底座能力边界（附录 A #59，实测 VPP 26.06）：
//   - govpp v0.13.0 无 pcap binapi（仅有 pcap_trace_on/off 二进制消息但未生成绑定），
//     故与 ping 同法（决策 #36）经 `vppctl`（CLI socket）：`pcap trace rx tx intfc <if> max <n>`
//     开始、`pcap trace off` 停止并落盘。
//   - CLI 形式**不能指定文件名**，VPP 固定写 `/tmp/rxtx.pcap`；单会话（有活动会话时 409），
//     故导出时改名搬入导出目录。
//   - 决策 #465：`stop`（不导出）＝冻结（数据保留待 export），不再删文件；决策 #466：冻结即把
//     工作文件暂存进导出目录隐藏位（`.<终名>.part`），并由持久记录支撑跨 nfvisd 重启的启动收敛
//     （重启前冻结/在抓的会话重建为「已停止待导出」；VPP 侧在抓态补一次 `pcap trace off` 落盘）。
//   - `max <n>` 是 **VPP 缓冲深度**（环形，超出丢最旧），不是"达到 n 自动停止"；VPP 26.06
//     无已抓包计数 API/CLI，故不实现自动停止——契约 count 语义按实测调整（见决策 #59）。
//   - VPP 26.06 的 pcap trace 不支持 ACL 过滤（仅支持按 error 过滤），`filter_acl` 非空即报错。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultCaptureDir 导出 pcap 目录。
const DefaultCaptureDir = "/var/lib/nfvis/captures"

// VPP 固定输出路径（CLI 形式无 filename 参数）。
var vppPcapDefaultPath = filepath.Join(os.TempDir(), "rxtx.pcap")

// captureShellTimeout 单次 VPP CLI 调用上限（避免 vppctl 阻塞拖死请求）。
const captureShellTimeout = 15 * time.Second

var (
	// ErrCaptureActive 已有抓包会话进行中（契约 409）。
	ErrCaptureActive = errors.New("已有抓包会话进行中")
	// ErrNoCapture 当前无抓包会话。
	ErrNoCapture = errors.New("当前无抓包会话")
	// ErrCaptureFilterUnsupported ACL 过滤在 VPP 26.06 pcap trace 不支持。
	ErrCaptureFilterUnsupported = errors.New("VPP 26.06 pcap trace 不支持 ACL 过滤（仅支持按 error 过滤）")
	// ErrCaptureNotFound pcap 文件不存在。
	ErrCaptureNotFound = errors.New("pcap 文件不存在")
)

// CaptureSession 抓包会话（契约 CaptureStatus.active）。
//
// ⚠️ 决策 #465 起，登记里的会话有两种状态：**在抓**（Stopped=false）与**已停止、数据待导出**
// （Stopped=true）——`stop` 不再丢弃数据，只把它冻结；`export` 随后走同一 finalize 路径取走。
// 会话在停止后**仍留在登记里**，故 Start 的既有「已有会话」守卫天然要求操作者先 export。
//
// ⚠️ 决策 #466 起，会话还可由 nfvisd 启动时的**重启收敛**从持久记录重建（Recovered=true）：
// 重启前冻结/在抓的会话在重启后重建为「已停止待导出」，数据照常可 export。
type CaptureSession struct {
	Interface string    `json:"interface"`
	Captured  int       `json:"captured"`
	StartedAt time.Time `json:"started_at"`
	MaxDepth  int       `json:"max_depth,omitempty"`
	Stopped   bool      `json:"stopped,omitempty"`
	Recovered bool      `json:"recovered,omitempty"`
	filename  string
	work      string // 工作文件当前路径（在抓时＝vppOut；冻结暂存后＝导出目录隐藏暂存位）
}

// CaptureFile 已导出 pcap（契约 CaptureStatus.files）。
type CaptureFile struct {
	Name      string    `json:"name"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

// CaptureProvider 抓包编排。
type CaptureProvider struct {
	shell  VPPShell
	ifaces func(ifname string) (uint32, bool, error)
	dir    string // 导出目录
	vppOut string // VPP 输出文件（缺省 /tmp/rxtx.pcap）
	now    func() time.Time

	// 会话记录文件（决策 #466）：start/冻结/取走三处同步，供重启收敛重建会话。
	sessionsFile string

	mu     sync.Mutex
	active *CaptureSession

	rmu              sync.Mutex // 收敛状态（与 mu 分开：收敛里会跑 VPP CLI，不能压着会话锁）
	reconcilePending bool       // 启动收敛未完成（如 VPP 未就绪）——下次抓包操作补跑
}

// NewCaptureProvider 构造（shell 为 VPP CLI 执行器；dir 空取缺省）。
func NewCaptureProvider(shell VPPShell, ifaces func(string) (uint32, bool, error), dir string) *CaptureProvider {
	if dir == "" {
		dir = DefaultCaptureDir
	}
	return &CaptureProvider{shell: shell, ifaces: ifaces, dir: dir, vppOut: vppPcapDefaultPath,
		now: time.Now, sessionsFile: DefaultCaptureSessionsFile, reconcilePending: true}
}

// SetVPPOutputPath 覆盖 VPP 输出路径（测试用）。
func (p *CaptureProvider) SetVPPOutputPath(path string) { p.vppOut = path }

// SetSessionsFile 覆盖会话记录文件（测试用）。
func (p *CaptureProvider) SetSessionsFile(path string) {
	if strings.TrimSpace(path) != "" {
		p.sessionsFile = path
	}
}

// Dir 导出目录。
func (p *CaptureProvider) Dir() string { return p.dir }

// Start 开始抓包（count 为 VPP 缓冲深度，缺省 1000）。
func (p *CaptureProvider) Start(ctx context.Context, ifname string, count int, filterACL string) error {
	p.tryReconcile(ctx) // 决策 #466：重启收敛未完成时先补跑（恢复的冻结会话会经下方守卫拦下新会话）
	if strings.TrimSpace(ifname) == "" {
		return errors.New("抓包接口必填")
	}
	if filterACL != "" {
		return ErrCaptureFilterUnsupported
	}
	if count <= 0 {
		count = 1000
	}
	p.mu.Lock()
	if p.active != nil {
		cur := *p.active
		p.mu.Unlock()
		if cur.Stopped {
			// 决策 #465：冻结中的会话仍占登记位——如实说清，并给照做路径（别让它变成「为什么起不来」）。
			return fmt.Errorf("已有已停止待导出的抓包会话（接口 %s）：请先 request capture export 取走它，再开始新抓包",
				cur.Interface)
		}
		return fmt.Errorf("%w（接口 %s）", ErrCaptureActive, cur.Interface)
	}
	p.mu.Unlock()

	if p.ifaces != nil {
		if _, ok, err := p.ifaces(ifname); err != nil {
			return fmt.Errorf("解析抓包接口 %s: %w", ifname, err)
		} else if !ok {
			return fmt.Errorf("%w: 抓包接口 %s", ErrIfaceUnavailable, ifname)
		}
	}
	// 清理可能残留的旧文件，避免 off 时误搬上一次的抓包
	_ = os.Remove(p.vppOut)
	sctx, cancel := context.WithTimeout(ctx, captureShellTimeout)
	defer cancel()
	if _, err := p.shell.Run(sctx, "pcap", "trace", "rx", "tx", "intfc", ifname, "max", strconv.Itoa(count)); err != nil {
		return fmt.Errorf("开始抓包: %w", err)
	}
	sess := &CaptureSession{
		Interface: ifname, StartedAt: p.now().UTC(), MaxDepth: count,
		filename: fmt.Sprintf("nfvis-cap-%s-%s.pcap", ifname, p.now().UTC().Format("20060102T150405Z")),
		work:     p.vppOut,
	}
	p.mu.Lock()
	p.active = sess
	p.mu.Unlock()
	p.saveRecord(sess) // 决策 #466：登记落盘（跨 nfvisd 重启可收敛）
	return nil
}

var writtenRe = regexp.MustCompile(`(?i)Write\s+(\d+)\s+packets?`)

// Stop 停止抓包。export=true 时把 pcap 搬入导出目录并登记；export=false 则**停止并冻结**
// （决策 #465：保留已抓数据、会话留在登记里待 export——原实现这里 `os.Remove` 会把数据丢掉；
// 决策 #466：冻结即把工作文件暂存进导出目录隐藏位，不再依赖 /tmp 存活）。
func (p *CaptureProvider) Stop(ctx context.Context, export bool) (CaptureFile, error) {
	p.tryReconcile(ctx) // 决策 #466：重启收敛未完成时先补跑（恢复出的冻结会话可在这里被 export 取走）
	p.mu.Lock()
	sess := p.active
	p.mu.Unlock()
	if sess == nil {
		return CaptureFile{}, ErrNoCapture
	}
	if sess.work == "" {
		sess.work = p.vppOut
	}
	// 第一次停（仍在抓）：关掉 trace 并记下读数，然后把会话标成**冻结**（数据文件不动）。
	if !sess.Stopped {
		sctx, cancel := context.WithTimeout(ctx, captureShellTimeout)
		out, err := p.shell.Run(sctx, "pcap", "trace", "off")
		cancel()
		if m := writtenRe.FindStringSubmatch(out); len(m) == 2 {
			if n, cerr := strconv.Atoi(m[1]); cerr == nil {
				sess.Captured = n
			}
		}
		if err != nil && !strings.Contains(err.Error(), "No packets captured") {
			// 停不干净：登记保留（仍是「在抓」），如实报错，可重试。
			return CaptureFile{}, fmt.Errorf("停止抓包: %w", err)
		}
		p.mu.Lock()
		if s := p.active; s == sess {
			s.Stopped = true
		}
		p.mu.Unlock()
	}
	if !export {
		// 冻结：决策 #466 起把数据暂存进导出目录隐藏位（宿主重启/临时目录清理都不丢），
		// 随后 export 在其上做同目录改名。暂存失败保留原位（不阻断；导出路径两处都认）。
		p.stageStopped(sess)
		p.saveRecord(sess)
		return CaptureFile{}, nil
	}
	// —— finalize（export；对「仍在抓」与「已冻结」是同一条路径）——
	if _, serr := os.Stat(sess.work); serr != nil {
		// 一个包也没抓到（或文件已被取走）：无文件导出，与内核侧同口径；登记与记录照常摘除。
		p.clearActive(sess)
		p.clearRecord()
		return CaptureFile{}, nil
	}
	if err := os.MkdirAll(p.dir, 0o755); err != nil {
		return CaptureFile{}, fmt.Errorf("创建导出目录: %w", err)
	}
	dst := filepath.Join(p.dir, sess.filename)
	if err := moveFile(sess.work, dst); err != nil {
		return CaptureFile{}, fmt.Errorf("导出 pcap: %w", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		return CaptureFile{}, err
	}
	p.clearActive(sess)
	p.clearRecord()
	return CaptureFile{Name: sess.filename, SizeBytes: info.Size(), CreatedAt: info.ModTime().UTC()}, nil
}

// stageStopped 冻结暂存：把工作文件搬进导出目录的隐藏暂存位（`.<终名>.part`，不参与清单）。
// 尽力而为——失败保留原位、不阻断冻结（导出路径对原位与暂存位都认）。
func (p *CaptureProvider) stageStopped(sess *CaptureSession) {
	staged := filepath.Join(p.dir, "."+sess.filename+".part")
	if sess.work == staged {
		return // 已暂存（冻结中再 stop 的幂等路径）
	}
	if _, err := os.Stat(sess.work); err != nil {
		return // 无数据文件：无物可暂存
	}
	if err := os.MkdirAll(p.dir, 0o755); err != nil {
		return
	}
	if err := moveFile(sess.work, staged); err != nil {
		return
	}
	sess.work = staged
}

// saveRecord 把当前会话落盘（决策 #466；写失败不阻断抓包——退化为进程内登记）。
func (p *CaptureProvider) saveRecord(sess *CaptureSession) {
	_ = SaveCaptureRecord(p.sessionsFile, CapturePlaneVPP, &CaptureSessionRecord{
		Interface: sess.Interface, FileName: sess.filename, WorkPath: sess.work,
		StartedAt: sess.StartedAt, Count: sess.MaxDepth, Stopped: sess.Stopped,
	})
}

func (p *CaptureProvider) clearRecord() {
	_ = SaveCaptureRecord(p.sessionsFile, CapturePlaneVPP, nil)
}

// clearActive 摘除**仍属本次会话**的登记（避免并发路径把新会话误清）。
func (p *CaptureProvider) clearActive(sess *CaptureSession) {
	p.mu.Lock()
	if p.active == sess {
		p.active = nil
	}
	p.mu.Unlock()
}

// Status 返回活动会话与已导出文件（契约 CaptureStatus）。
func (p *CaptureProvider) Status() (*CaptureSession, []CaptureFile) {
	p.tryReconcile(context.Background()) // 决策 #466：重启收敛未完成时补跑（读视图即触发点之一）
	p.mu.Lock()
	var active *CaptureSession
	if p.active != nil {
		c := *p.active
		active = &c
	}
	p.mu.Unlock()
	return active, p.List()
}

// ---------- 决策 #466：跨 nfvisd 重启收敛 ----------

// tryReconcile 启动收敛未完成时的补跑（VPP 启停竞态：装配时 VPP 未就绪 ⇒ 首次抓包操作补）。
// 报告在补跑路径没有落点——可见信号就是读视图里出现的恢复会话/归档文件。
func (p *CaptureProvider) tryReconcile(ctx context.Context) {
	p.rmu.Lock()
	need := p.reconcilePending
	p.rmu.Unlock()
	if !need {
		return
	}
	_ = p.Reconcile(ctx)
}

// Reconcile 启动收敛（决策 #466）：把重启前冻结/在抓的会话按持久记录重建为「已停止待导出」，
// 并把无记录的工作文件残件归档为可见 pcap。返回供 journal 记录的逐条报告。
//
// 语义（逐条都有单测钉住）：
//   - 记录说「在抓」⇒ 先补一次 `pcap trace off`（幂等：already disabled 无害）把缓冲落盘；
//     VPP 未就绪则该次收敛整体延后（待下次抓包操作重试），记录保持不动。
//   - 数据文件（记录路径 / 原 vppOut / 隐藏暂存位，取第一个存在的）非空 ⇒ 重建为冻结会话
//     （Recovered=true）；更新记录（stopped=true、work_path=实际暂存位）。
//   - 数据文件不存在/为空 ⇒ 清记录：在抓态报「已关闭、无捕获数据」（VPP 若也重启过，数据
//     未落盘无法取回——如实说明）；冻结态报「数据文件已不存在」。
//   - 其余工作文件（无记录残件：旧版本遗留 / 记录写失败 / 换数据面）⇒ 非空归档为可见 pcap
//     （不藏不删），空文件清掉。
func (p *CaptureProvider) Reconcile(ctx context.Context) []string {
	p.rmu.Lock()
	defer p.rmu.Unlock()
	var rep []string

	recs, err := LoadCaptureRecords(p.sessionsFile)
	if err != nil {
		rep = append(rep, "抓包会话记录无法解析（按无记录处理）："+err.Error())
	}
	rec := recs[CapturePlaneVPP]
	incomplete := false
	var adoptedWork string
	if rec != nil {
		if !rec.Stopped {
			// 在抓态：数据还在 VPP 的 ring 里（文件只在 off 时落盘）——补一次 off。
			sctx, cancel := context.WithTimeout(ctx, captureShellTimeout)
			_, err := p.shell.Run(sctx, "pcap", "trace", "off")
			cancel()
			if err != nil {
				rep = append(rep, "VPP 未就绪，重启前的在抓会话收敛延后（下次抓包操作自动重试）："+err.Error())
				incomplete = true
			}
		}
	}
	if rec != nil && !incomplete {
		src := p.findRecordFile(rec)
		var size int64
		if src != "" {
			if info, serr := os.Stat(src); serr == nil {
				size = info.Size()
			}
		}
		if src != "" && size > 0 {
			work := src
			staged := filepath.Join(p.dir, "."+rec.FileName+".part")
			if src != staged {
				if err := os.MkdirAll(p.dir, 0o755); err == nil {
					if err := moveFile(src, staged); err == nil {
						work = staged
					}
				}
			}
			p.mu.Lock()
			p.active = &CaptureSession{
				Interface: rec.Interface, StartedAt: rec.StartedAt, MaxDepth: rec.Count,
				Stopped: true, Recovered: true, filename: rec.FileName, work: work,
			}
			p.mu.Unlock()
			rec.Stopped, rec.WorkPath = true, work
			_ = SaveCaptureRecord(p.sessionsFile, CapturePlaneVPP, rec)
			adoptedWork = work
			rep = append(rep, fmt.Sprintf("已恢复重启前的抓包会话（接口 %s，数据待导出 %s）：request capture export 取走",
				rec.Interface, rec.FileName))
		} else {
			_ = SaveCaptureRecord(p.sessionsFile, CapturePlaneVPP, nil)
			if rec.Stopped {
				rep = append(rep, "重启前冻结的抓包会话已无可导出数据（未捕获到报文或数据文件已被清理），登记已清除")
			} else {
				rep = append(rep, "重启前的在抓会话已关闭（无捕获数据；若期间 VPP 也重启过，数据未落盘无法取回）")
			}
		}
	}

	if incomplete {
		// VPP 未就绪：连残件扫描一并延后——扫描范围含本记录的暂存文件，先动它会把
		// 「有数据」误判成「文件不存在」（重试时数据已被自己归档走）。
		rep = append(rep, "无记录残件扫描延后（与在抓会话收敛同批重试）")
	} else {
		rep = append(rep, p.sweepWorkFiles(adoptedWork)...)
	}

	p.reconcilePending = incomplete
	return rep
}

// findRecordFile 记录指向的数据文件：按「记录路径 → 原 vppOut → 隐藏暂存位」取第一个存在的。
func (p *CaptureProvider) findRecordFile(rec *CaptureSessionRecord) string {
	cands := []string{
		rec.WorkPath,
		p.vppOut,
		filepath.Join(p.dir, "."+rec.FileName+".part"),
	}
	for _, c := range cands {
		if c != "" && fileExists(c) {
			return c
		}
	}
	return ""
}

// sweepWorkFiles 扫描两个工作位（导出目录 + vppOut 所在目录如 /tmp）里的工作文件残件。
// skip 是本次已采纳的会话工作文件（不动它）。
func (p *CaptureProvider) sweepWorkFiles(skip string) []string {
	var rep []string
	dirs := []string{p.dir, filepath.Dir(p.vppOut)}
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
			full := filepath.Join(dir, e.Name())
			if full == skip {
				continue
			}
			switch {
			case dir == filepath.Dir(p.vppOut) && e.Name() == filepath.Base(p.vppOut):
				// 裸 rxtx.pcap（无记录）：VPP 固定输出路径的遗留——归档为可见 pcap。
				rep = append(rep, p.archiveResidue(full, "")...)
			default:
				if base, ok := CaptureWorkBase(e.Name()); ok {
					rep = append(rep, p.archiveResidue(full, base)...)
				}
			}
		}
	}
	return rep
}

// archiveResidue 把无记录的工作文件归档为可见 pcap（非空）或清掉（空）；base 空则用
// `nfvis-cap-recovered-<mtime>` 命名。
func (p *CaptureProvider) archiveResidue(full, base string) []string {
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
	if base == "" {
		base = RecoveredPcapName(info.ModTime())
	}
	name := UniquePcapName(p.dir, base)
	if err := os.MkdirAll(p.dir, 0o755); err != nil {
		return []string{fmt.Sprintf("归档残件 %s：创建导出目录失败: %v", full, err)}
	}
	if err := moveFile(full, filepath.Join(p.dir, name)); err != nil {
		return []string{fmt.Sprintf("归档残件 %s 失败: %v", full, err)}
	}
	return []string{fmt.Sprintf("无记录残件已归档为 %s（源 %s）", name, full)}
}

// List 已导出 pcap（创建时间倒序）。
func (p *CaptureProvider) List() []CaptureFile {
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		return []CaptureFile{}
	}
	out := make([]CaptureFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pcap") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, CaptureFile{Name: e.Name(), SizeBytes: info.Size(), CreatedAt: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// moveFile 搬移文件：优先 rename，跨设备（如 /tmp tmpfs → 数据盘 EXDEV）时退化为复制+删除。
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}

// Path 解析导出文件路径（限定目录内，防穿越）。
func (p *CaptureProvider) Path(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") || !strings.HasSuffix(name, ".pcap") {
		return "", fmt.Errorf("%w: %q", ErrCaptureNotFound, name)
	}
	full := filepath.Join(p.dir, name)
	if _, err := os.Stat(full); err != nil {
		return "", fmt.Errorf("%w: %s", ErrCaptureNotFound, name)
	}
	return full, nil
}
