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
