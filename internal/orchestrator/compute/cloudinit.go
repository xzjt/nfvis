package compute

import (
	"fmt"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

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

// BuildMetaData 生成 meta-data：instance-id 用 VM 名（稳定，重启不再触发
// cloud-init 的 instance 变更路径），local-hostname 取 hostname 缺省 VM 名。
func BuildMetaData(vm model.VMFunction) string {
	hostname := vm.Name
	if vm.CloudInit != nil && vm.CloudInit.Hostname != "" {
		hostname = vm.CloudInit.Hostname
	}
	var b strings.Builder
	b.WriteString("instance-id: " + vm.Name + "\n")
	b.WriteString("local-hostname: " + hostname + "\n")
	return b.String()
}

func ensureTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
