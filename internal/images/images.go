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

	// 下载进度与失败原因（R84-6）：URL 拉取是**异步**的，操作者只能经
	// `show images <名> detail`（同一元数据）观察——此前只在开始/结束时写状态，
	// 中断后既看不到进度、也看不到失败原因，只能看着 downloading 干等。
	// 三个字段均为 omitempty，既有 index.json 与既有响应字段不受影响。
	DownloadedBytes int64  `json:"downloaded_bytes,omitempty"` // 已下载字节（含续传的断点）
	TotalBytes      int64  `json:"total_bytes,omitempty"`      // 服务端声明的总字节（未知为 0）
	LastError       string `json:"last_error,omitempty"`       // 失败原因（failed 必填；ready 时可载清理告警）
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
	dockerLoad func(path, name string) error
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
// name 为仓库目录项名：load 后按它重打标签 `<名>:latest`，使容器引用（=目录项名）
// 与 Docker 运行名闭环——tar 内嵌 tag 必含冒号而目录项名白名单禁冒号，两者天然不等，
// 不重打标签则容器镜像端到端不可用（决策 #160）。
func (s *Store) SetDockerLoader(f func(path, name string) error) { s.dockerLoad = f }

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
	if err := s.reconcileStaleDownloads(); err != nil {
		return nil, err
	}
	return s, nil
}

// reconcileStaleDownloads 把索引里残留的 downloading 改判 failed 并写明原因。
//
// URL 拉取在守护进程的协程里跑（受理即返回），进程一退出拉取就没了，且没有任何东西
// 会再写这条状态——于是中断后 `show images` 会**永远**显示 downloading（R84-6 真机现象：
// 无进度、无失败原因、无重试指引）。启动时改判是这条状态的兜底收口：操作者看到的
// 至少是 failed + 可照做的原因，而不是一个永远不会变的状态。
func (s *Store) reconcileStaleDownloads() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for name, m := range s.index {
		if m.ImportState != StateDownloading {
			continue
		}
		m.ImportState = StateFailed
		m.LastError = "拉取未完成（下载中断或守护进程重启）；已下载部分保留在 .part，重跑同一命令即从断点续传"
		s.index[name] = m
		changed = true
	}
	if !changed {
		return nil
	}
	return s.saveLocked()
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

// Names 返回仓库现有镜像名（按名升序）。实现 config.ImageResolver，
// 供「镜像不存在」的报错列出可选项（附录 A #98）。
// 与 Lookup 同口径：**不列 failed**——拉取失败/校验失败留下的条目不算可用名，
// 列出来等于把操作者指向一个必然被拒的名字（R84-6 后 failed 条目更常见，故一并收口）。
func (s *Store) Names() []string {
	metas := s.List()
	out := make([]string, 0, len(metas))
	for _, m := range metas {
		if m.ImportState == StateFailed {
			continue
		}
		out = append(out, m.Name)
	}
	return out
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
	incAbs, err := filepath.Abs(s.cfg.IncomingDir)
	if err != nil {
		return Meta{}, err
	}
	// 相对路径按 incoming 目录解析（R84-8）：此前直接用 filepath.Abs，相对名会按**守护进程
	// 的 CWD** 解析成 `/alpine.qcow2`，与候选描述「incoming 内的文件路径」不符，照候选敲必被拒。
	// 越界判定不变：Join 会做 Clean，`../` 逃逸出去后仍被下面的前缀检查拒绝。
	abs := incomingFile
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(incAbs, abs)
	} else if abs, err = filepath.Abs(abs); err != nil {
		return Meta{}, err
	}
	if !strings.HasPrefix(abs, incAbs+string(filepath.Separator)) {
		return Meta{}, fmt.Errorf("文件 %s 必须位于 %s 内（相对路径按该目录解析；先经 scp/sftp 传入）",
			incomingFile, s.cfg.IncomingDir)
	}
	// 容器镜像：归档（docker save 产物）经 Docker `image load` 入本地分层存储，
	// 仓库只登记元数据、不留文件（FR-CMP-030）；name 即仓库目录项名，load 后按它重打标签
	// `<name>:latest`（决策 #160），故配置里唯一可用的名字就是仓库中的镜像名——
	// tar 内嵌 tag 与之无关（R84-7：旧文案宣称可用名是内嵌 tag，与实现不符）。
	if typ == TypeContainer && s.dockerLoad == nil {
		return Meta{}, fmt.Errorf("导入容器镜像 %s：未接入 Docker", name)
	}
	sha, size, err := fileSHA256(abs)
	if err != nil {
		return Meta{}, err
	}
	if typ == TypeContainer {
		if err := s.dockerLoad(abs, name); err != nil {
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
