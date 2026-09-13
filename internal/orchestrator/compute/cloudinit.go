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

// BuildUserData 生成 cloud-init user-data：
// 声明了 user_data 时原样使用（允许用户自备 #cloud-config/脚本）；
// 否则据 hostname 与 ssh_keys 生成最小 #cloud-config。
func BuildUserData(vm model.VMFunction) string {
	ci := vm.CloudInit
	if ci == nil {
		return ""
	}
	if strings.TrimSpace(ci.UserData) != "" {
		return ensureTrailingNewline(ci.UserData)
	}
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	if ci.Hostname != "" {
		b.WriteString("hostname: " + ci.Hostname + "\n")
		fmt.Fprintf(&b, "fqdn: %s\n", ci.Hostname)
	} else if vm.Name != "" {
		b.WriteString("hostname: " + vm.Name + "\n")
	}
	if len(ci.SSHKeys) > 0 {
		b.WriteString("ssh_authorized_keys:\n")
		for _, k := range ci.SSHKeys {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			// YAML 标量用单引号包裹，内部单引号按 YAML 规则双写。
			b.WriteString("  - '" + strings.ReplaceAll(k, "'", "''") + "'\n")
		}
	}
	b.WriteString("users:\n")
	b.WriteString("  - name: nfvis\n")
	b.WriteString("    sudo: ALL=(ALL) NOPASSWD:ALL\n")
	b.WriteString("    shell: /bin/bash\n")
	if len(ci.SSHKeys) > 0 {
		b.WriteString("    ssh_authorized_keys:\n")
		for _, k := range ci.SSHKeys {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
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
