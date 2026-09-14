// M5-4：core dump 收集与滚动清理（FR-OPS-041）。
//
// 采集方式：扫描转储目录（缺省 /var/lib/nfvis/coredumps）中的 core 文件，
// 进程名从文件名推断（core.<process>.<pid>.<ts> / <process>.core / core-<process>-*）。
// 生产环境由内核 core_pattern 或 systemd-coredump 落到该目录（见附录 A #57）；
// 容量上限 + 滚动清理（Prune）按「总字节上限」保留最新转储。
package system

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultCoreDir core dump 目录。
const DefaultCoreDir = "/var/lib/nfvis/coredumps"

// DefaultCoreMaxBytes 目录容量上限（超出后按时间删除最旧，FR-OPS-041）。
const DefaultCoreMaxBytes = 2 << 30 // 2 GiB

// CoreDump 一条转储（契约 CoreDump）。
type CoreDump struct {
	File       string    `json:"file"`
	Process    string    `json:"process"`
	SizeBytes  int64     `json:"size_bytes"`
	OccurredAt time.Time `json:"occurred_at"`
}

// CoreDumps 转储目录管理。
type CoreDumps struct {
	Dir      string
	MaxBytes int64
}

// NewCoreDumps 构造（dir 空取缺省；maxBytes<=0 取缺省）。
func NewCoreDumps(dir string, maxBytes int64) *CoreDumps {
	if dir == "" {
		dir = DefaultCoreDir
	}
	if maxBytes <= 0 {
		maxBytes = DefaultCoreMaxBytes
	}
	return &CoreDumps{Dir: dir, MaxBytes: maxBytes}
}

// isCoreFile 判断是否转储文件（避免把 .txt/.json 等杂项计入）。
func isCoreFile(name string) bool {
	base := strings.ToLower(name)
	return strings.HasPrefix(base, "core.") || strings.HasPrefix(base, "core-") ||
		strings.HasSuffix(base, ".core") || strings.Contains(base, ".core.")
}

// processOf 从文件名推断进程名（尽力而为，契约 process 字段）。
func processOf(name string) string {
	base := filepath.Base(name)
	base = strings.TrimSuffix(base, ".core")
	if strings.HasPrefix(base, "core.") {
		base = strings.TrimPrefix(base, "core.")
	} else if strings.HasPrefix(base, "core-") {
		base = strings.TrimPrefix(base, "core-")
	}
	parts := strings.Split(base, ".")
	// core.<process>.<pid>.<ts>：取第二段作为进程名
	if len(parts) >= 2 && isDigits(parts[1]) {
		return parts[0]
	}
	if len(parts) >= 1 && parts[0] != "" {
		return parts[0]
	}
	return "unknown"
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// List 返回转储清单（occurred_at 倒序）。目录不存在时返回空。
func (c *CoreDumps) List() []CoreDump {
	entries, err := os.ReadDir(c.Dir)
	if err != nil {
		return []CoreDump{}
	}
	out := make([]CoreDump, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !isCoreFile(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, CoreDump{
			File: e.Name(), Process: processOf(e.Name()),
			SizeBytes: info.Size(), OccurredAt: info.ModTime().UTC(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OccurredAt.After(out[j].OccurredAt) })
	return out
}

// ErrCoreNotFound 转储文件不存在。
// 独立于 backup.go 的 ErrNotFound（其文案是「备份归档不存在」）——复用会让 core dump 的
// 报错说成备份归档，误导排查（决策 #76 §4④，全功能 CLI 测试发现）。
var ErrCoreNotFound = fmt.Errorf("core dump 不存在")

// Path 解析转储文件路径（限定目录内，防穿越）。
func (c *CoreDumps) Path(file string) (string, error) {
	if file == "" || strings.ContainsAny(file, `/\`) || strings.Contains(file, "..") {
		return "", fmt.Errorf("%w: %q", ErrCoreNotFound, file)
	}
	p := filepath.Join(c.Dir, file)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("%w: %s", ErrCoreNotFound, file)
	}
	return p, nil
}

// Delete 删除转储：file 空 = 全部；返回删除数量。
func (c *CoreDumps) Delete(file string) (int, error) {
	if file == "" {
		rows := c.List()
		n := 0
		for _, r := range rows {
			if err := os.Remove(filepath.Join(c.Dir, r.File)); err == nil {
				n++
			}
		}
		return n, nil
	}
	p, err := c.Path(file)
	if err != nil {
		return 0, err
	}
	if err := os.Remove(p); err != nil {
		return 0, err
	}
	return 1, nil
}

// Prune 按容量上限滚动清理（保留最新，删除最旧至总量 ≤ MaxBytes）。返回删除数量。
func (c *CoreDumps) Prune() int {
	rows := c.List() // occurred_at 倒序（最新在前）
	var total int64
	for _, r := range rows {
		total += r.SizeBytes
	}
	removed := 0
	for i := len(rows) - 1; i >= 0 && total > c.MaxBytes; i-- {
		if err := os.Remove(filepath.Join(c.Dir, rows[i].File)); err == nil {
			total -= rows[i].SizeBytes
			removed++
		}
	}
	return removed
}
