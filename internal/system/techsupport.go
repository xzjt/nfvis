// M5-4：tech-support 一键打包（FR-OPS-040）。
//
// 归档 tar.gz 分节：version.json、config.json（committed）、audit.json、
// status.json（VPP/运行态快照）、logs.txt（nfvisd 日志尾部）、core-dumps.json（清单）、
// README.txt（生成信息）。各来源以函数注入，便于单测与运行态装配。
package system

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
	if err := os.MkdirAll(t.Dir, 0o755); err != nil {
		return File{}, fmt.Errorf("创建诊断归档目录: %w", err)
	}
	created := t.now().UTC()
	name := "nfvis-tech-support-" + created.Format("20060102T150405Z") + ".tar.gz"
	path := filepath.Join(t.Dir, name)
	f, err := os.Create(path)
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
		{"config.json", t.sectionJSON(t.Src.Config)},
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

func (t *TechSupport) sectionFixed(v any) func() ([]byte, error) {
	return func() ([]byte, error) {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(b, '\n'), nil
	}
}

func (t *TechSupport) sectionLogs() ([]byte, error) {
	if t.Src.Logs == nil {
		return []byte("(logs unavailable)\n"), nil
	}
	return t.Src.Logs()
}

func (t *TechSupport) sectionReadme(created time.Time, cores int) func() ([]byte, error) {
	return func() ([]byte, error) {
		var b strings.Builder
		fmt.Fprintf(&b, "NFViS tech-support archive\ncreated_at: %s\nnfvis_version: %s\ncore_dumps: %d\n\n", created.Format(time.RFC3339), t.version, cores)
		b.WriteString("sections:\n  version.json    nfvis 与底座组件版本\n  config.json     committed 配置\n  audit.json      审计日志\n  status.json     VPP/运行态快照\n  logs.txt        nfvisd 日志尾部\n  core-dumps.json core dump 清单\n")
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
