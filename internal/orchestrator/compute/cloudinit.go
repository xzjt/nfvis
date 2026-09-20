package compute

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

// ResolveUserData 把 cloud-init user-data 的「YAML 文本或文件」归一为文本（决策 #114）。
//
// 命令树承诺 user-data 支持「文本或文件」，但此前实现只把取值当内联文本：传文件路径时
// 会把**路径字符串本身**当作脚本注入 seed——guest 侧 cloud-init 直接 exec 该文件、报
// `Exec format error`，表现为「VM 起来了但初始化啥也没发生」（round34 真机实证）。
//
// 判据：单行（无换行）且是绝对路径或 `./` 开头 → 视为文件路径，读入其内容；
// 读不到即报错——**不静默回退成文本**，那正是本次要修的坑。
func ResolveUserData(v string) (string, error) {
	s := strings.TrimSpace(v)
	if s == "" || strings.ContainsAny(s, "\n\r") {
		return v, nil
	}
	if !filepath.IsAbs(s) && !strings.HasPrefix(s, "./") {
		return v, nil
	}
	b, err := os.ReadFile(s)
	if err != nil {
		return "", fmt.Errorf("读取 user-data 文件 %s: %w", s, err)
	}
	return string(b), nil
}

// ValidateUserDataType 校验 user-data 文本能被 cloud-init 正确识别（决策 #114）：
// 须以 `#cloud-config` 开头（YAML）或以 `#!` 开头（脚本）。
//
// cloud-init 对脚本段是**直接 exec**：缺 shebang 会报 `Exec format error`（真机实证）；
// 而既非 cloud-config 也非脚本的内容，cloud-init 既不执行也不报错——属静默丢弃。
// 两者都该在配置/装机侧提前拦住，而不是让操作者去 guest 里找原因。
func ValidateUserDataType(v string) error {
	s := strings.TrimSpace(v)
	if s == "" || strings.HasPrefix(s, "#cloud-config") || strings.HasPrefix(s, "#!") {
		return nil
	}
	return fmt.Errorf("user-data 既不是 #cloud-config 开头的 YAML，也不是 #! 开头的脚本：" +
		"cloud-init 不会执行它（脚本段必须带 shebang，如 #!/bin/sh）")
}

// cloud-init NoCloud 数据构建（FR-CMP-016）。
//
// 纯函数产出 `user-data` / `meta-data` 文本，由适配层写入目录并调用
// `cloud-localds` 生成卷标为 `cidata` 的 seed ISO（以 cloud-init 官方 NoCloud
// 约定为准：文件名 user-data / meta-data，卷标 cidata）。

// BuildUserData 生成 cloud-init user-data（FR-CMP-016：user-data 与 SSH 公钥均须注入）：
//   - 仅声明 user_data：原样使用（补尾换行）；
//   - 仅声明 hostname/ssh_keys：生成最小 #cloud-config；
//   - 两者都有：输出 cloud-init 支持的 **MIME multipart**（一份生成的 #cloud-config
//     承载 hostname/SSH 公钥/users + 一份用户 user_data，按 `#cloud-config` 前缀判定类型）。
//
// multipart 是 cloud-init 组合多段 user-data 的标准方式，避免在同份 YAML 中出现重复顶层键。
func BuildUserData(vm model.VMFunction) string {
	ci := vm.CloudInit
	if ci == nil {
		return ""
	}
	hasKeys := false
	for _, k := range ci.SSHKeys {
		if strings.TrimSpace(k) != "" {
			hasKeys = true
		}
	}
	hasUserData := strings.TrimSpace(ci.UserData) != ""
	if !hasUserData {
		return buildCloudConfig(ci, vm.Name)
	}
	if !hasKeys && ci.Hostname == "" {
		return ensureTrailingNewline(ci.UserData)
	}
	const boundary = "==NFVIS-BOUNDARY=="
	var b strings.Builder
	b.WriteString("Content-Type: multipart/mixed; boundary=\"" + boundary + "\"\n")
	b.WriteString("MIME-Version: 1.0\n\n")
	b.WriteString("--" + boundary + "\n")
	b.WriteString("Content-Type: text/cloud-config; charset=\"us-ascii\"\n\n")
	b.WriteString(buildCloudConfig(ci, vm.Name))
	b.WriteString("\n--" + boundary + "\n")
	ctype := "text/x-shellscript"
	if strings.HasPrefix(strings.TrimSpace(ci.UserData), "#cloud-config") {
		ctype = "text/cloud-config"
	}
	b.WriteString("Content-Type: " + ctype + "; charset=\"us-ascii\"\n\n")
	b.WriteString(ensureTrailingNewline(ci.UserData))
	b.WriteString("\n--" + boundary + "--\n")
	return b.String()
}

// buildCloudConfig 生成 nfvis 托管的最小 #cloud-config（hostname + SSH 公钥 + 默认用户）。
func buildCloudConfig(ci *model.CloudInit, vmName string) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	if ci.Hostname != "" {
		b.WriteString("hostname: " + ci.Hostname + "\n")
		fmt.Fprintf(&b, "fqdn: %s\n", ci.Hostname)
	} else if vmName != "" {
		b.WriteString("hostname: " + vmName + "\n")
	}
	keys := make([]string, 0, len(ci.SSHKeys))
	for _, k := range ci.SSHKeys {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) > 0 {
		b.WriteString("ssh_authorized_keys:\n")
		for _, k := range keys {
			b.WriteString("  - '" + strings.ReplaceAll(k, "'", "''") + "'\n")
		}
	}
	b.WriteString("users:\n")
	b.WriteString("  - name: nfvis\n")
	b.WriteString("    sudo: ALL=(ALL) NOPASSWD:ALL\n")
	b.WriteString("    shell: /bin/bash\n")
	if len(keys) > 0 {
		b.WriteString("    ssh_authorized_keys:\n")
		for _, k := range keys {
			b.WriteString("      - '" + strings.ReplaceAll(k, "'", "''") + "'\n")
		}
	}
	return b.String()
}

// BuildMetaData 生成 meta-data。
//
// instance-id 取「VM 名 + cloud-init 输入摘要」（决策 #114）：同名 VM 重启时 id 不变
// （不触发 cloud-init 的 instance 变更路径），但**改了 user-data/SSH 公钥/hostname 后
// 摘要随之变化**——否则 cloud-init 按 once-per-instance 跳过 user-data，操作者会看到
// 「改了 user-data、重启了，guest 里啥也没变」（round34 真机实证）。
// local-hostname 取 hostname，缺省 VM 名。
func BuildMetaData(vm model.VMFunction) string {
	hostname := vm.Name
	ciInputs := ""
	if vm.CloudInit != nil {
		if vm.CloudInit.Hostname != "" {
			hostname = vm.CloudInit.Hostname
		}
		ud, err := ResolveUserData(vm.CloudInit.UserData)
		if err != nil {
			ud = vm.CloudInit.UserData // 报错留给 seed 构建路径（那里有 VM 名上下文）
		}
		ciInputs = ud + "\x00" + strings.Join(vm.CloudInit.SSHKeys, "\x00")
	}
	sum := sha256.Sum256([]byte(hostname + "\x00" + ciInputs))
	var b strings.Builder
	b.WriteString(fmt.Sprintf("instance-id: %s-%x\n", vm.Name, sum[:4]))
	b.WriteString("local-hostname: " + hostname + "\n")
	return b.String()
}

func ensureTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
