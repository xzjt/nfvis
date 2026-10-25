package compute

import (
	"context"
	"fmt"
	"io"
	"log"
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

// ReadTail 读取文件末尾 maxLines 行（决策 #311：libvirt 域日志摘录）。
// 读不到即返回 error（调用方如实说「取不到」，不编造内容）。日志可能很大，按块从尾部读。
func (qemuStorage) ReadTail(p string, maxLines int) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if maxLines <= 0 {
		maxLines = 20
	}
	const chunk = 32 << 10
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := st.Size()
	var collected []byte
	off := size
	for int64(len(collected)) < 64<<10 && off > 0 {
		n := int64(chunk)
		if off < n {
			n = off
		}
		off -= n
		buf := make([]byte, n)
		if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
			return "", err
		}
		collected = append(buf, collected...)
		if strings.Count(string(collected), "\n") > maxLines {
			break
		}
	}
	lines := strings.Split(strings.TrimRight(string(collected), "\n"), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n"), nil
}

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
	// O-4（报告 §6.3）：有 vNIC 时追加 `--network-config`，让依赖 cloud-init 网络的镜像
	// （Cirros 等）拿到 DHCP 配置——此前 seed 只有 user-data/meta-data，guest 无地址 ⇒
	// 无限重试 metadata（串口刷 `failed N/20: request failed`）、进不到登录态。
	//
	// **网络配置是增强，不是必需**：写盘/传参失败时**降级为旧的两件形态**（打一行日志），
	// 不让「多一份网络配置」把 VM 定义整个挡住。cloud-localds 的形态已按其 --help 核实：
	//   -N | --network-config F   write network config file to local datasource
	netCfg := BuildNetworkConfig(vm)
	netCfgPath := ""
	if netCfg != "" {
		netCfgPath = filepath.Join(dir, "network-config")
		if werr := os.WriteFile(netCfgPath, []byte(netCfg), 0o600); werr != nil {
			log.Printf("VM %s: 写 network-config 失败，seed 降级为不含网络配置（guest 需镜像自带 DHCP 客户端）: %v",
				vm.Name, werr)
			netCfgPath = ""
		}
	}
	out, err := exec.CommandContext(ctx, "cloud-localds",
		cloudLocaldsArgs(isoPath, userData, metaData, netCfgPath)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("cloud-localds %s: %w: %s", isoPath, err, strings.TrimSpace(string(out)))
	}
	return nil
}
