package compute

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

// 底座薄适配（qemu-img / cloud-localds / 文件系统）。
// 文件名以 `_libvirt.go` 结尾 → 覆盖率排除，由 nfvis-vm 集成测试覆盖。

// qemuStorage storageAPI 真实实现。
type qemuStorage struct{}

func newQemuStorage() storageAPI { return qemuStorage{} }

// EnsureDir 建目录；权限 0755：QEMU 以 libvirt-qemu 用户运行，需可穿越目录
// 访问盘/seed（libvirt 动态属主仅改文件属主，不解决目录穿越）。
func (qemuStorage) EnsureDir(p string) error { return os.MkdirAll(p, 0o755) }

func (qemuStorage) Exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Clone qemu-img create -f qcow2 -F qcow2 -b <image> <disk>（backing 克隆，共享只读镜像底层）。
func (qemuStorage) Clone(ctx context.Context, imagePath, diskPath string) error {
	out, err := exec.CommandContext(ctx, "qemu-img", "create",
		"-f", "qcow2", "-F", "qcow2", "-b", imagePath, diskPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img create backing %s -> %s: %w: %s",
			imagePath, diskPath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// CreateBlank qemu-img create -f qcow2 <disk> <size>G（空数据盘，FR-CMP-018）。
func (qemuStorage) CreateBlank(ctx context.Context, diskPath string, sizeGB int) error {
	if sizeGB <= 0 {
		sizeGB = 1
	}
	out, err := exec.CommandContext(ctx, "qemu-img", "create",
		"-f", "qcow2", diskPath, fmt.Sprintf("%dG", sizeGB)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img create %s %dG: %w: %s", diskPath, sizeGB, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (qemuStorage) RemoveAll(p string) error { return os.RemoveAll(p) }

// cloudLocaldsSeed seedBuilder 真实实现：写 user-data/meta-data → cloud-localds
// 生成卷标 cidata 的 NoCloud seed ISO（NoCloud 约定，FR-CMP-016）。
type cloudLocaldsSeed struct{}

func newCloudLocaldsSeed() seedBuilder { return cloudLocaldsSeed{} }

func (cloudLocaldsSeed) Build(ctx context.Context, vm model.VMFunction, isoPath string) error {
	dir := filepath.Dir(isoPath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	userData := filepath.Join(dir, "user-data")
	metaData := filepath.Join(dir, "meta-data")
	// user-data 支持「文本或文件」（决策 #114）：文件路径在此解析；内容类型在此校验——
	// 让「路径写错/脚本缺 shebang」在 VM 启动时就报清楚，而不是 guest 里静默无效果。
	ud := ""
	if vm.CloudInit != nil {
		ud = vm.CloudInit.UserData
	}
	resolved, err := ResolveUserData(ud)
	if err != nil {
		return err
	}
	if err := ValidateUserDataType(resolved); err != nil {
		return fmt.Errorf("VM %s %w", vm.Name, err)
	}
	vmForSeed := vm
	if vm.CloudInit != nil {
		ci := *vm.CloudInit
		ci.UserData = resolved
		vmForSeed.CloudInit = &ci
	}
	if err := os.WriteFile(userData, []byte(BuildUserData(vmForSeed)), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(metaData, []byte(BuildMetaData(vm)), 0o600); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, "cloud-localds", isoPath, userData, metaData).CombinedOutput()
	if err != nil {
		return fmt.Errorf("cloud-localds %s: %w: %s", isoPath, err, strings.TrimSpace(string(out)))
	}
	return nil
}
