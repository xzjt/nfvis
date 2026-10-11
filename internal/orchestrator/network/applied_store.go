package network

// 决策 #400：已应用 vpp 配置段哈希的持久落点。
//
// 为什么必须落盘：Manager.appliedHash 是**进程内**字段，而启动路径只 EnsureRunning（拉起磁盘上
// 已存在的 /etc/vpp/startup.conf）——整机重启后 appliedHash 恒空，PendingRestart 便把「存在 vpp
// 配置」一律判为待重启（真机三次复现：VPP 实际正按 committed 配置运行）。落一个小文件、启动时
// 载入即可让「重启后」如实为否。
//
// 属**本机运行态**（随机器走）：不入 committed 配置、不进备份/恢复语义，与 dpdk-bindings.json 同口径。

import (
	"os"
	"path/filepath"
	"strings"
)

// DefaultAppliedHashPath 已应用哈希缺省落点。
const DefaultAppliedHashPath = "/var/lib/nfvis/vpp-applied.hash"

// AppliedStore 已应用哈希的持久落点（纯文本一行）。
type AppliedStore struct {
	Path      string
	ReadFile  func(string) ([]byte, error)
	WriteFile func(string, []byte) error
}

// NewAppliedStore 构造（path 空取缺省）。
func NewAppliedStore(path string) *AppliedStore {
	if strings.TrimSpace(path) == "" {
		path = DefaultAppliedHashPath
	}
	return &AppliedStore{Path: path}
}

func (s *AppliedStore) read(path string) ([]byte, error) {
	if s.ReadFile != nil {
		return s.ReadFile(path)
	}
	return os.ReadFile(path)
}

func (s *AppliedStore) write(path string, data []byte) error {
	if s.WriteFile != nil {
		return s.WriteFile(path, data)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o644)
}

// Load 读取已应用哈希；文件缺失返回 ("", nil)（首启：尚未应用过任何配置）。
func (s *AppliedStore) Load() (string, error) {
	raw, err := s.read(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// Save 落盘已应用哈希；空哈希删除落点（表示尚未应用）。
func (s *AppliedStore) Save(hash string) error {
	hash = strings.TrimSpace(hash)
	if hash == "" {
		if err := os.Remove(s.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return s.write(s.Path, []byte(hash+"\n"))
}

// DefaultDataplaneModePath 上次生效的数据面实现的持久落点（纯文本一行：vpp|kernel）。
const DefaultDataplaneModePath = "/var/lib/nfvis/dataplane-mode"

// DataplaneModeStore 上次生效数据面实现的持久落点。
//
// 与 AppliedStore 同口径（属**本机运行态**：随机器走，不入 committed、不进备份/恢复语义）。
// 用途：数据面实现（vpp/kernel）是**配置 + 整机重启**才切换的；重启后进程内无从得知「上一代
// 是哪种数据面」，于是旧面的内核对象无人拆、与新面双活（R12-7）。持久化上一次生效的数据面后，
// 启动装配即可比对：代际变化 ⇒ 先拆旧面、再强制一次全量重建。
type DataplaneModeStore struct {
	Path      string
	ReadFile  func(string) ([]byte, error)
	WriteFile func(string, []byte) error
}

// NewDataplaneModeStore 构造（path 空取缺省）。
func NewDataplaneModeStore(path string) *DataplaneModeStore {
	if strings.TrimSpace(path) == "" {
		path = DefaultDataplaneModePath
	}
	return &DataplaneModeStore{Path: path}
}

func (s *DataplaneModeStore) read(path string) ([]byte, error) {
	if s.ReadFile != nil {
		return s.ReadFile(path)
	}
	return os.ReadFile(path)
}

func (s *DataplaneModeStore) write(path string, data []byte) error {
	if s.WriteFile != nil {
		return s.WriteFile(path, data)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o644)
}

// Load 读取上次生效的数据面；文件缺失返回 ("", nil)（首启：尚未切换过任何数据面）。
func (s *DataplaneModeStore) Load() (string, error) {
	raw, err := s.read(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// Save 落盘当前生效的数据面；空值删除落点（表示尚未生效过）。
func (s *DataplaneModeStore) Save(mode string) error {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		if err := os.Remove(s.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return s.write(s.Path, []byte(mode+"\n"))
}
