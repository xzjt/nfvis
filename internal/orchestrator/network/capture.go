package network

// M5-3：数据面抓包（VPP pcap trace，FR-OPS-042）。
//
// 底座能力边界（附录 A #59，实测 VPP 26.06）：
//   - govpp v0.13.0 无 pcap binapi（仅有 pcap_trace_on/off 二进制消息但未生成绑定），
//     故与 ping 同法（决策 #36）经 `vppctl`（CLI socket）：`pcap trace rx tx intfc <if> max <n>`
//     开始、`pcap trace off` 停止并落盘。
//   - CLI 形式**不能指定文件名**，VPP 固定写 `/tmp/rxtx.pcap`；单会话（有活动会话时 409），
//     故导出时改名搬入导出目录。
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

// CaptureSession 活动抓包会话（契约 CaptureStatus.active）。
type CaptureSession struct {
	Interface string    `json:"interface"`
	Captured  int       `json:"captured"`
	StartedAt time.Time `json:"started_at"`
	MaxDepth  int       `json:"max_depth,omitempty"`
	filename  string
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

	mu     sync.Mutex
	active *CaptureSession
}

// NewCaptureProvider 构造（shell 为 VPP CLI 执行器；dir 空取缺省）。
func NewCaptureProvider(shell VPPShell, ifaces func(string) (uint32, bool, error), dir string) *CaptureProvider {
	if dir == "" {
		dir = DefaultCaptureDir
	}
	return &CaptureProvider{shell: shell, ifaces: ifaces, dir: dir, vppOut: vppPcapDefaultPath, now: time.Now}
}

// SetVPPOutputPath 覆盖 VPP 输出路径（测试用）。
func (p *CaptureProvider) SetVPPOutputPath(path string) { p.vppOut = path }

// Dir 导出目录。
func (p *CaptureProvider) Dir() string { return p.dir }

// Start 开始抓包（count 为 VPP 缓冲深度，缺省 1000）。
func (p *CaptureProvider) Start(ctx context.Context, ifname string, count int, filterACL string) error {
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
		p.mu.Unlock()
		return fmt.Errorf("%w（接口 %s）", ErrCaptureActive, p.active.Interface)
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
	p.mu.Lock()
	p.active = &CaptureSession{
		Interface: ifname, StartedAt: p.now().UTC(), MaxDepth: count,
		filename: fmt.Sprintf("nfvis-cap-%s-%s.pcap", ifname, p.now().UTC().Format("20060102T150405Z")),
	}
	p.mu.Unlock()
	return nil
}

var writtenRe = regexp.MustCompile(`(?i)Write\s+(\d+)\s+packets?`)

// Stop 停止抓包。export=true 时把 pcap 搬入导出目录并登记；false 则丢弃（契约 DELETE 语义）。
func (p *CaptureProvider) Stop(ctx context.Context, export bool) (CaptureFile, error) {
	p.mu.Lock()
	sess := p.active
	p.mu.Unlock()
	if sess == nil {
		return CaptureFile{}, ErrNoCapture
	}
	sctx, cancel := context.WithTimeout(ctx, captureShellTimeout)
	defer cancel()
	out, err := p.shell.Run(sctx, "pcap", "trace", "off")
	// 即使 off 报错（如无包）也继续清理会话状态，避免卡死
	captured := 0
	if m := writtenRe.FindStringSubmatch(out); len(m) == 2 {
		if n, cerr := strconv.Atoi(m[1]); cerr == nil {
			captured = n
		}
	}
	p.mu.Lock()
	p.active = nil
	p.mu.Unlock()
	if err != nil && !strings.Contains(err.Error(), "No packets captured") {
		return CaptureFile{}, fmt.Errorf("停止抓包: %w", err)
	}
	if captured == 0 {
		_ = os.Remove(p.vppOut)
		return CaptureFile{}, nil
	}
	if !export {
		_ = os.Remove(p.vppOut)
		return CaptureFile{}, nil
	}
	if err := os.MkdirAll(p.dir, 0o755); err != nil {
		return CaptureFile{}, fmt.Errorf("创建导出目录: %w", err)
	}
	dst := filepath.Join(p.dir, sess.filename)
	if err := moveFile(p.vppOut, dst); err != nil {
		return CaptureFile{}, fmt.Errorf("导出 pcap: %w", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		return CaptureFile{}, err
	}
	return CaptureFile{Name: sess.filename, SizeBytes: info.Size(), CreatedAt: info.ModTime().UTC()}, nil
}

// Status 返回活动会话与已导出文件（契约 CaptureStatus）。
func (p *CaptureProvider) Status() (*CaptureSession, []CaptureFile) {
	p.mu.Lock()
	var active *CaptureSession
	if p.active != nil {
		c := *p.active
		active = &c
	}
	p.mu.Unlock()
	return active, p.List()
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
