// M5-4：tech-support 一键打包（FR-OPS-040）。
//
// 归档 tar.gz 分节：version.json、config.json（committed，**脱敏视图**）、audit.json、
// status.json（VPP/运行态快照）、logs.txt（nfvisd 日志尾部）、core-dumps.json（清单）、
// README.txt（生成信息）。各来源以函数注入，便于单测与运行态装配。
//
// 口径（FR-SEC-007 / 决策 #149）：诊断包是**要交给支持人员**的件，故其中不得出现口令——
// 配置分节按展示层同口径脱敏（口令哈希等敏感叶子整个移除），日志分节剥掉产品自己打印的
// 一次性凭据，归档本身以 0600 落盘、目录 0700。**下载端口的 class 不变**（read-only 可下载，
// 现场流程「operator 生成 → 自己下载送支持」不破）：需要**可恢复**的完整配置时用配置备份导出。
package system

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xzjt/nfvis/internal/model"
)

// DefaultTechSupportDir 诊断归档目录。
const DefaultTechSupportDir = "/var/lib/nfvis/tech-support"

// TechSupportSources 各分节数据来源（nil = 该节写占位说明）。
type TechSupportSources struct {
	Version func() any
	Config  func() (any, error)
	Audit   func() (any, error)
	Status  func() (any, error)
	Logs    func() ([]byte, error)
	Cores   func() []CoreDump
}

// TechSupport tech-support 归档管理。
type TechSupport struct {
	Dir     string
	Src     TechSupportSources
	now     func() time.Time
	version string
}

// NewTechSupport 构造。
func NewTechSupport(dir string, src TechSupportSources, version string) *TechSupport {
	if dir == "" {
		dir = DefaultTechSupportDir
	}
	return &TechSupport{Dir: dir, Src: src, now: time.Now, version: version}
}

// Generate 生成诊断归档并返回文件元数据（FR-OPS-040）。
func (t *TechSupport) Generate() (File, error) {
	// 归档目录 0700：归档是运维件，同机非 root 用户不得进入（既有安装上可能是 0755，
	// 故 MkdirAll 之后显式收紧一次——MkdirAll 不会改已存在目录的权限）。
	if err := os.MkdirAll(t.Dir, 0o700); err != nil {
		return File{}, fmt.Errorf("创建诊断归档目录: %w", err)
	}
	if err := os.Chmod(t.Dir, 0o700); err != nil {
		return File{}, fmt.Errorf("收紧诊断归档目录权限: %w", err)
	}
	created := t.now().UTC()
	name := "nfvis-tech-support-" + created.Format("20060102T150405Z") + ".tar.gz"
	path := filepath.Join(t.Dir, name)
	// 0600 落盘（与配置备份归档同口径）：不得用 os.Create（0666&~umask → 通常 0644）。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return File{}, fmt.Errorf("创建归档: %w", err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	cores := []CoreDump{}
	if t.Src.Cores != nil {
		cores = t.Src.Cores()
	}
	sections := []struct {
		name string
		body func() ([]byte, error)
	}{
		{"version.json", t.sectionVersion},
		{"config.json", t.sectionRedacted(t.Src.Config)},
		{"audit.json", t.sectionJSON(t.Src.Audit)},
		{"status.json", t.sectionJSON(t.Src.Status)},
		{"logs.txt", t.sectionLogs},
		{"core-dumps.json", t.sectionFixed(cores)},
		{"README.txt", t.sectionReadme(created, len(cores))},
	}
	for _, s := range sections {
		body, err := s.body()
		if err != nil {
			// 单节失败不中断归档：写入错误说明（诊断包必须尽量产出）
			body = []byte("section error: " + err.Error() + "\n")
		}
		if err := writeTarFile(tw, s.name, body, created); err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return File{}, err
		}
	}
	if err := tw.Close(); err != nil {
		return File{}, err
	}
	if err := gz.Close(); err != nil {
		return File{}, err
	}
	info, err := f.Stat()
	if err != nil {
		return File{}, err
	}
	return File{File: name, Kind: "tech-support", SizeBytes: info.Size(), CreatedAt: created}, nil
}

func (t *TechSupport) sectionVersion() ([]byte, error) {
	var v any = map[string]string{"nfvis": t.version}
	if t.Src.Version != nil {
		v = t.Src.Version()
	}
	return json.MarshalIndent(v, "", "  ")
}

func (t *TechSupport) sectionJSON(fn func() (any, error)) func() ([]byte, error) {
	return func() ([]byte, error) {
		if fn == nil {
			return []byte("null\n"), nil
		}
		v, err := fn()
		if err != nil {
			return nil, err
		}
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(b, '\n'), nil
	}
}

// sectionRedacted 与 sectionJSON 同，但先按**展示层同口径**脱敏
// （model.RedactSensitive → 敏感叶子整个移除，规则唯一落点是 model.IsSensitiveKey）。
//
// 诊断包里的配置是**脱敏视图**：口令哈希是设备上最敏感的秘密，而诊断包会被交给支持人员；
// 要**可恢复**的完整配置请用配置备份导出（那条是另一个端点、另一个 class，有意不脱敏）。
func (t *TechSupport) sectionRedacted(fn func() (any, error)) func() ([]byte, error) {
	return func() ([]byte, error) {
		if fn == nil {
			return []byte("null\n"), nil
		}
		v, err := fn()
		if err != nil {
			return nil, err
		}
		b, err := json.MarshalIndent(model.RedactSensitive(v), "", "  ")
		if err != nil {
			return nil, err
		}
		return append(b, '\n'), nil
	}
}

// bootstrapCredentialMarker 首次启动引导时产品打印的一次性口令**所在行的标识**
// （原文见 cmd/nfvisd/main.go 的启动提示，形如 `…一次性口令（仅显示一次，请立即修改）: <口令>`）。
//
// 为什么要在日志里动手：该提示打在 stdout，systemd 会收进 journal，于是它会出现在
// 诊断包的 logs.txt 里；而这一行的后半段是**明文口令**（super-user 级）。日志其余部分
// 是原文（诊断价值就在这里），只有产品自己打印的这一处凭据剥掉。
// main.go 的提示文本若改动，本标识会失配——TestBootstrapCredentialMarkerMatchesStartupMessage
// 会在源码里核对，改文案的人会看到一条要同步这里的提示。
const bootstrapCredentialMarker = "一次性口令（仅显示一次，请立即修改）"

// sectionLogs 日志尾部；剥掉产品自己打印的一次性凭据（见 bootstrapCredentialMarker）。
func (t *TechSupport) sectionLogs() ([]byte, error) {
	if t.Src.Logs == nil {
		return []byte("(logs unavailable)\n"), nil
	}
	b, err := t.Src.Logs()
	if err != nil {
		return nil, err
	}
	return scrubBootstrapCredential(b), nil
}

// scrubBootstrapCredential 把带一次性口令的行改为「标记 + 占位」：保留这行（引导发生过
// 是诊断事实），只把值换成占位符。命中不了标记时原样返回（不做任何猜测式改写）。
func scrubBootstrapCredential(b []byte) []byte {
	marker := []byte(bootstrapCredentialMarker)
	if !bytes.Contains(b, marker) {
		return b
	}
	lines := bytes.Split(b, []byte("\n"))
	for i, ln := range lines {
		idx := bytes.Index(ln, marker)
		if idx < 0 {
			continue
		}
		masked := make([]byte, 0, idx+len(marker)+len(": ")+len(model.RedactedPlaceholder))
		masked = append(masked, ln[:idx+len(marker)]...)
		masked = append(masked, ": "+model.RedactedPlaceholder...)
		lines[i] = masked
	}
	return bytes.Join(lines, []byte("\n"))
}

func (t *TechSupport) sectionFixed(v any) func() ([]byte, error) {
	return func() ([]byte, error) {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(b, '\n'), nil
	}
}

func (t *TechSupport) sectionReadme(created time.Time, cores int) func() ([]byte, error) {
	return func() ([]byte, error) {
		var b strings.Builder
		fmt.Fprintf(&b, "NFViS tech-support archive\ncreated_at: %s\nnfvis_version: %s\ncore_dumps: %d\n\n", created.Format(time.RFC3339), t.version, cores)
		b.WriteString("sections:\n  version.json    nfvis 与底座组件版本\n  config.json     committed 配置（口令等敏感字段已隐藏）\n  audit.json      审计日志\n  status.json     VPP/运行态快照\n  logs.txt        nfvisd 日志尾部\n  core-dumps.json core dump 清单\n")
		b.WriteString("\nnote: 本归档内的配置是脱敏视图，不能用于恢复；要可恢复的完整配置请导出配置备份。\n")
		return []byte(b.String()), nil
	}
}

func writeTarFile(tw *tar.Writer, name string, body []byte, mod time.Time) error {
	hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), ModTime: mod}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(body)
	return err
}

// List 归档列表（创建时间倒序）。
func (t *TechSupport) List() []File {
	entries, err := os.ReadDir(t.Dir)
	if err != nil {
		return []File{}
	}
	out := make([]File, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "nfvis-tech-support-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, File{File: e.Name(), Kind: "tech-support", SizeBytes: info.Size(), CreatedAt: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// ErrDiagNotFound 诊断归档不存在。
// 独立于 backup.go 的 ErrNotFound（文案「备份归档不存在」）——复用会把 tech-support
// 的报错说成备份归档（决策 #76 §4④）。
var ErrDiagNotFound = fmt.Errorf("诊断归档不存在")

// Path 解析归档路径（限定目录内）。
func (t *TechSupport) Path(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", fmt.Errorf("%w: %q", ErrDiagNotFound, name)
	}
	p := filepath.Join(t.Dir, name)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("%w: %s", ErrDiagNotFound, name)
	}
	return p, nil
}
