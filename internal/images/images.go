// Package images 实现镜像仓库（M4-8，FR-CMP-030~033）。
//
// 仓库为目录 + index.json 元数据索引：导入/拉取写盘 + 索引，删除前校验引用。
// URL 拉取支持断点续传（HTTP Range）与 sha256 校验；/data/incoming 导入成功后清理源文件。
// 容器镜像的删除经 Docker API（注入 remover，见 SetDockerRemover）。
package images

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/model"
)

// 导入状态（契约 Image.import_state）。
const (
	StateReady       = "ready"
	StateDownloading = "downloading"
	StateFailed      = "failed"
)

// 镜像类型（契约 Image.type）。
const (
	TypeVM        = "vm-image"
	TypeContainer = "container-image"
)

// Config 仓库配置。
type Config struct {
	Dir         string // 本地仓库目录
	IncomingDir string // scp/sftp 落地目录（导入成功自动清理）
}

// DefaultConfig 生产缺省。
func DefaultConfig() Config {
	return Config{Dir: "/var/lib/nfvis/images", IncomingDir: "/data/incoming"}
}

// Meta 镜像元数据（契约 Image）。
type Meta struct {
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	SizeBytes   int64     `json:"size_bytes,omitempty"`
	SHA256      string    `json:"sha256,omitempty"`
	Format      string    `json:"format,omitempty"`
	Description string    `json:"description,omitempty"`
	ImportedAt  time.Time `json:"imported_at,omitempty"`
	ImportState string    `json:"import_state,omitempty"`
}

// Store 镜像仓库（并发安全）。
type Store struct {
	cfg Config

	mu    sync.Mutex
	index map[string]Meta
	now   func() time.Time

	// dockerRemove 删除容器镜像（经 Docker API）；nil = 不支持（删除容器镜像时报错）。
	dockerRemove func(ref string) error
}

// SetDockerRemover 注入容器镜像删除实现（Docker API）。
func (s *Store) SetDockerRemover(f func(ref string) error) { s.dockerRemove = f }

// Open 打开/初始化仓库（创建目录并载入 index.json）。
func Open(cfg Config) (*Store, error) {
	def := DefaultConfig()
	if cfg.Dir == "" {
		cfg.Dir = def.Dir
	}
	if cfg.IncomingDir == "" {
		cfg.IncomingDir = def.IncomingDir
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建镜像仓库目录: %w", err)
	}
	s := &Store{cfg: cfg, index: map[string]Meta{}, now: time.Now}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Config 返回生效配置。
func (s *Store) Config() Config { return s.cfg }

func (s *Store) indexPath() string { return filepath.Join(s.cfg.Dir, "index.json") }

func (s *Store) load() error {
	b, err := os.ReadFile(s.indexPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取镜像索引: %w", err)
	}
	m := map[string]Meta{}
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("解析镜像索引: %w", err)
	}
	s.index = m
	return nil
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.index, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.indexPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.indexPath())
}

// Lookup 实现 config.ImageResolver（FR-CFG-011⑤：镜像存在性与类型匹配）。
func (s *Store) Lookup(name string) (config.ImageInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.index[name]
	if !ok || m.ImportState == StateFailed {
		return config.ImageInfo{}, false
	}
	return config.ImageInfo{Name: m.Name, Type: m.Type}, true
}

// List 返回全部镜像（按名升序）。
func (s *Store) List() []Meta {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Meta, 0, len(s.index))
	for _, m := range s.index {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get 返回单个镜像元数据。
func (s *Store) Get(name string) (Meta, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.index[name]
	return m, ok
}

// Path 返回镜像文件路径（容器镜像无本地文件，返回空）。
func (s *Store) Path(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.index[name]
	if !ok || m.Type != TypeVM {
		return ""
	}
	return filepath.Join(s.cfg.Dir, name)
}

// ErrReferenced 镜像被 VNF/容器引用，不可删除（FR-CMP-033）。
var ErrReferenced = errors.New("镜像被引用，不可删除")

// ErrNotFound 镜像不存在。
var ErrNotFound = errors.New("镜像不存在")

// Delete 删除镜像（refCount>0 时报 ErrReferenced；容器镜像经 Docker API 删除）。
func (s *Store) Delete(name string, refCount int) error {
	if refCount > 0 {
		return fmt.Errorf("%w（%d 个引用）", ErrReferenced, refCount)
	}
	s.mu.Lock()
	m, ok := s.index[name]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if m.Type == TypeContainer {
		if s.dockerRemove == nil {
			return fmt.Errorf("删除容器镜像 %s：未接入 Docker", name)
		}
		if err := s.dockerRemove(name); err != nil {
			return fmt.Errorf("删除容器镜像 %s: %w", name, err)
		}
	} else {
		if err := os.Remove(filepath.Join(s.cfg.Dir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除镜像文件 %s: %w", name, err)
		}
	}
	s.mu.Lock()
	delete(s.index, name)
	err := s.saveLocked()
	s.mu.Unlock()
	return err
}

func (s *Store) setMeta(m Meta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.index[m.Name] = m
	return s.saveLocked()
}

// ImportIncoming 从 incoming 目录导入镜像（源文件须位于 IncomingDir 内，成功后清理）。
func (s *Store) ImportIncoming(name, typ, incomingFile, description string) (Meta, error) {
	if strings.TrimSpace(name) == "" {
		return Meta{}, fmt.Errorf("镜像名不能为空")
	}
	if typ != TypeVM && typ != TypeContainer {
		return Meta{}, fmt.Errorf("type 必须为 %s 或 %s", TypeVM, TypeContainer)
	}
	abs, err := filepath.Abs(incomingFile)
	if err != nil {
		return Meta{}, err
	}
	incAbs, err := filepath.Abs(s.cfg.IncomingDir)
	if err != nil {
		return Meta{}, err
	}
	if !strings.HasPrefix(abs, incAbs+string(filepath.Separator)) {
		return Meta{}, fmt.Errorf("文件 %s 必须位于 %s 内（先经 scp/sftp 传入）", incomingFile, s.cfg.IncomingDir)
	}
	if typ == TypeContainer {
		return Meta{}, fmt.Errorf("容器镜像导入经 Docker（docker load/pull），不支持 incoming 文件导入")
	}
	sha, size, err := fileSHA256(abs)
	if err != nil {
		return Meta{}, err
	}
	dst := filepath.Join(s.cfg.Dir, name)
	if err := os.Rename(abs, dst); err != nil {
		return Meta{}, fmt.Errorf("导入镜像 %s: %w", name, err)
	}
	m := Meta{Name: name, Type: typ, SizeBytes: size, SHA256: sha,
		Format: formatOf(name), Description: description, ImportedAt: s.now().UTC(), ImportState: StateReady}
	if err := s.setMeta(m); err != nil {
		return Meta{}, err
	}
	return m, nil
}

// refCountOf 统计配置中对镜像的引用数（VM/容器）。
func RefCount(cfg model.Config, name string) int {
	n := 0
	for _, vm := range cfg.VirtualMachineFunctions {
		if vm.Image == name {
			n++
		}
	}
	for _, ct := range cfg.ContainerFunctions {
		if ct.Image == name {
			n++
		}
	}
	return n
}

// formatOf 由文件名推断格式（qcow2/iso/raw…）。
func formatOf(name string) string {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	if ext == "" {
		return "unknown"
	}
	return ext
}

// fileSHA256 计算文件 sha256 与大小（流式）。
func fileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	h, err := hashReader(f)
	if err != nil {
		return "", 0, err
	}
	return h, st.Size(), nil
}
