// Package system 系统运维能力：配置备份/恢复与恢复出厂（M5-6，FR-OPS-004~007）。
//
// 语义：
//   - 备份 = committed 配置（已含 login-users/class/口令策略）+ 镜像**清单**（元数据，
//     不含文件本体，FR-OPS-006）打包为单个 JSON 归档，落盘可下载。
//   - 恢复 = 解析归档 → 以 candidate 提交（走事务引擎全量校验与底座下发，FR-OPS-005）。
//     镜像文件不在归档内，恢复后需另行导入（镜像清单仅作对照）。
//   - 恢复出厂 = 提交空配置（applier 级联删除网络/VNF/容器）→ 删除全部镜像 →
//     账号随空配置回到初始化（下次启动重新引导 admin）（FR-OPS-007，需双重确认）。
package system

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/images"
	"github.com/xzjt/nfvis/internal/model"
)

// Format 归档标识（防误导入）。
const Format = "nfvis-config-backup"

// ArchiveVersion 归档结构版本（schema 兼容迁移由事务引擎负责，FR-OPS-003）。
const ArchiveVersion = 1

// Config Manager 配置。
type Config struct {
	Dir string // 备份目录
}

// DefaultConfig 生产缺省。
func DefaultConfig() Config { return Config{Dir: "/var/lib/nfvis/backup"} }

// Engine 事务引擎最小能力集（*config.Engine 满足；便于单测注入）。
type Engine interface {
	Committed() (model.Config, error)
	Edit(sess config.Session) error
	UpdateCandidate(sess config.Session, cfg model.Config) error
	Commit(ctx context.Context, sess config.Session, opts config.CommitOpts) (config.CommitResult, error)
	Release(sess config.Session) error
}

// ImageStore 镜像仓库最小能力集（*images.Store 满足）。
type ImageStore interface {
	List() []images.Meta
	Delete(name string, refCount int) error
}

// Archive 备份归档内容。
type Archive struct {
	Format        string        `json:"format"`
	ArchiveVer    int           `json:"archive_version"`
	Version       string        `json:"version"`
	CreatedAt     time.Time     `json:"created_at"`
	Config        model.Config  `json:"config"`
	Images        []images.Meta `json:"images"`
	SchemaVersion int           `json:"config_schema_version,omitempty"`
}

// File 归档文件元数据（契约 BackupArchive）。
type File struct {
	File      string    `json:"file"`
	Kind      string    `json:"kind"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

// Manager 备份/恢复/恢复出厂。
type Manager struct {
	cfg    Config
	engine Engine
	images ImageStore
	ver    string
	now    func() time.Time
}

// NewManager 构造（ver 为 nfvis 版本，写入归档便于跨版本恢复比对）。
func NewManager(cfg Config, engine Engine, imgs ImageStore, ver string) *Manager {
	if cfg.Dir == "" {
		cfg.Dir = DefaultConfig().Dir
	}
	return &Manager{cfg: cfg, engine: engine, images: imgs, ver: ver, now: time.Now}
}

// Backup 生成备份归档并返回文件元数据（FR-OPS-004）。
func (m *Manager) Backup() (File, error) {
	if err := os.MkdirAll(m.cfg.Dir, 0o755); err != nil {
		return File{}, fmt.Errorf("创建备份目录: %w", err)
	}
	cfg, err := m.engine.Committed()
	if err != nil {
		return File{}, err
	}
	metas := []images.Meta{}
	if m.images != nil {
		metas = m.images.List() // 仅清单，不含文件本体（FR-OPS-006）
	}
	arch := Archive{
		Format: Format, ArchiveVer: ArchiveVersion, Version: m.ver,
		CreatedAt: m.now().UTC(), Config: cfg, Images: metas,
	}
	data, err := json.MarshalIndent(arch, "", "  ")
	if err != nil {
		return File{}, fmt.Errorf("序列化归档: %w", err)
	}
	name := "nfvis-backup-" + arch.CreatedAt.Format("20060102T150405Z") + ".json"
	path := filepath.Join(m.cfg.Dir, name)
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return File{}, fmt.Errorf("写入归档: %w", err)
	}
	return File{File: name, Kind: "config-backup", SizeBytes: int64(len(data) + 1), CreatedAt: arch.CreatedAt}, nil
}

// List 备份归档列表（按创建时间倒序）。
func (m *Manager) List() []File {
	entries, err := os.ReadDir(m.cfg.Dir)
	if err != nil {
		return []File{}
	}
	out := make([]File, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "nfvis-backup-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, File{File: e.Name(), Kind: "config-backup", SizeBytes: info.Size(), CreatedAt: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

var (
	ErrNotFound    = fmt.Errorf("备份归档不存在")
	ErrBadFormat   = fmt.Errorf("归档格式不符（非 nfvis 配置备份）")
	ErrUnsupported = fmt.Errorf("归档版本不受支持")
)

// Path 解析归档文件路径（限定在备份目录内，防目录穿越）。
func (m *Manager) Path(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	p := filepath.Join(m.cfg.Dir, name)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return p, nil
}

// Restore 从归档数据恢复：解析 → 以 candidate 提交（FR-OPS-005）。
// 恢复后镜像文件本体不在归档内（FR-OPS-006），镜像清单仅作对照返回。
func (m *Manager) Restore(ctx context.Context, data []byte, user string) (config.CommitResult, []images.Meta, error) {
	var arch Archive
	if err := json.Unmarshal(data, &arch); err != nil {
		return config.CommitResult{}, nil, fmt.Errorf("%w: %v", ErrBadFormat, err)
	}
	if arch.Format != Format {
		return config.CommitResult{}, nil, fmt.Errorf("%w: format=%q", ErrBadFormat, arch.Format)
	}
	if arch.ArchiveVer > ArchiveVersion {
		return config.CommitResult{}, nil, fmt.Errorf("%w: archive_version=%d（本机支持 ≤%d）", ErrUnsupported, arch.ArchiveVer, ArchiveVersion)
	}
	sess := config.Session{User: user, Source: "system-restore"}
	if err := m.engine.Edit(sess); err != nil {
		return config.CommitResult{}, nil, fmt.Errorf("进入配置模式: %w", err)
	}
	defer func() { _ = m.engine.Release(sess) }()
	if err := m.engine.UpdateCandidate(sess, arch.Config); err != nil {
		return config.CommitResult{}, nil, fmt.Errorf("导入为 candidate: %w", err)
	}
	res, err := m.engine.Commit(ctx, sess, config.CommitOpts{Message: "restore from backup"})
	if err != nil {
		return config.CommitResult{}, nil, err
	}
	return res, arch.Images, nil
}

// ZeroizeResult 恢复出厂结果摘要。
type ZeroizeResult struct {
	RemovedImages int    `json:"removed_images"`
	Revision      int    `json:"revision"`
	Reason        string `json:"reason,omitempty"`
}

// Zeroize 恢复出厂（FR-OPS-007）：提交空配置 → 删除全部镜像 → 账号随空配置复位。
// 调用方须已完成双重确认（本方法不再询问）。
func (m *Manager) Zeroize(ctx context.Context, user string) (ZeroizeResult, error) {
	res := ZeroizeResult{}
	sess := config.Session{User: user, Source: "system-zeroize"}
	if err := m.engine.Edit(sess); err != nil {
		return res, fmt.Errorf("进入配置模式: %w", err)
	}
	defer func() { _ = m.engine.Release(sess) }()
	// 空配置：applier 级联删除网络对象、VNF/容器（FR-OPS-010 逆序补偿），账号随配置清空。
	if err := m.engine.UpdateCandidate(sess, model.Config{}); err != nil {
		return res, fmt.Errorf("置空 candidate: %w", err)
	}
	commitRes, err := m.engine.Commit(ctx, sess, config.CommitOpts{Message: "zeroize（恢复出厂）"})
	if err != nil {
		return res, fmt.Errorf("下发空配置: %w", err)
	}
	res.Revision = commitRes.Revision

	if m.images != nil {
		// 配置已清空 → 引用计数为 0，逐个删除（文件/ Docker 层）。
		for _, meta := range m.images.List() {
			if err := m.images.Delete(meta.Name, 0); err != nil {
				// 尽力而为：单个删除失败不阻塞其余（记录原因）
				res.Reason = err.Error()
				continue
			}
			res.RemovedImages++
		}
	}
	return res, nil
}
