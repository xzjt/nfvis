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
	"regexp"
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
	// dockerLoad 载入容器镜像归档（docker load）；nil = 不支持（导入容器镜像时报错）。
	dockerLoad func(path string) error
	// progress 下载进度（M5-1 image-import-progress 事件；nil = 不上报）。
	progress func(name string, written, total int64)
	// stateSink 导入状态变化（downloading/ready/failed；nil = 不上报）。
	stateSink func(name, typ, state string)
}

// SetProgressSink 注入下载进度回调（M5-1）。
func (s *Store) SetProgressSink(f func(name string, written, total int64)) { s.progress = f }

// SetStateSink 注入导入状态回调（M5-1）。
func (s *Store) SetStateSink(f func(name, typ, state string)) { s.stateSink = f }

func (s *Store) emitState(name, typ, state string) {
	if s.stateSink != nil {
		s.stateSink(name, typ, state)
	}
}

// SetDockerRemover 注入容器镜像删除实现（Docker API）。
func (s *Store) SetDockerRemover(f func(ref string) error) { s.dockerRemove = f }

// SetDockerLoader 注入容器镜像载入实现（Docker API `docker load`，FR-CMP-030/031）。
func (s *Store) SetDockerLoader(f func(path string) error) { s.dockerLoad = f }

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
	m, ok := s.index[name]
	s.mu.Unlock()
	if !ok || m.Type != TypeVM {
		return ""
	}
	if validateName(name, TypeVM) != nil {
		return "" // 纵深防御：历史索引可能含非法名
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
	// 纵深防御：历史索引可能含非法名，删除前把关（路径穿越防护）。
	if err := validateName(name, m.Type); err != nil {
		return err
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

// vmNameRe VM 镜像名白名单（文件名，与 model.nameRe 同口径：字母数字开头，仅限字母数字 - _ .，≤64 字符）。
var vmNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// containerNameRe 容器镜像名白名单：name 即 Docker ref 的单段形式（允许 tag 分隔符 ":"）。
var containerNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// validateName 校验镜像名（导入/拉取/删除/取路径的统一入口，路径穿越防护）。
//
// name 会参与 filepath.Join 拼接仓库路径——URL 拉取的归档（含容器镜像）也先落地
// <dir>/<name> 再经 docker load / rename，因此任何类型都绝不能含 "/"、"\\"、".."；
// index.json（及写索引用的 .tmp）为仓库保留名，防止镜像文件与索引互相覆盖后仓库整体不可用。
func validateName(name, typ string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("镜像 name %q 不合法：不得为空或包含路径成分", name)
	}
	if name == "index.json" || name == "index.json.tmp" {
		return fmt.Errorf("镜像 name %q 为仓库保留名", name)
	}
	if typ == TypeContainer {
		if !containerNameRe.MatchString(name) {
			return fmt.Errorf("容器镜像 name %q 不合法（字母数字开头，仅限字母数字 - _ . :）", name)
		}
		return nil
	}
	if !vmNameRe.MatchString(name) {
		return fmt.Errorf("镜像 name %q 不合法（字母数字开头，仅限字母数字 - _ .，≤64 字符）", name)
	}
	return nil
}

// ImportIncoming 从 incoming 目录导入镜像（源文件须位于 IncomingDir 内，成功后清理）。
func (s *Store) ImportIncoming(name, typ, incomingFile, description string) (Meta, error) {
	if err := validateName(name, typ); err != nil {
		return Meta{}, err
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
	// 容器镜像：归档（docker save 产物）经 Docker `image load` 入本地分层存储，
	// 仓库只登记元数据、不留文件（FR-CMP-030）；name 须与归档内的镜像引用一致
	// （容器 VNF 的 image 直接作为 Docker ref 使用）。
	if typ == TypeContainer && s.dockerLoad == nil {
		return Meta{}, fmt.Errorf("导入容器镜像 %s：未接入 Docker", name)
	}
	sha, size, err := fileSHA256(abs)
	if err != nil {
		return Meta{}, err
	}
	if typ == TypeContainer {
		if err := s.dockerLoad(abs); err != nil {
			return Meta{}, fmt.Errorf("docker load %s: %w", name, err)
		}
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return Meta{}, fmt.Errorf("导入后清理 %s: %w", incomingFile, err)
		}
		m := Meta{Name: name, Type: typ, SizeBytes: size, SHA256: sha, Format: "docker-archive",
			Description: description, ImportedAt: s.now().UTC(), ImportState: StateReady}
		if err := s.setMeta(m); err != nil {
			return Meta{}, err
		}
		s.emitState(name, typ, StateReady)
		return m, nil
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
	s.emitState(name, typ, StateReady)
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
