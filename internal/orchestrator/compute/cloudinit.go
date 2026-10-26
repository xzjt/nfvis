package compute

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

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
		b.WriteString("hostname: " + yamlScalar(ci.Hostname) + "\n")
		fmt.Fprintf(&b, "fqdn: %s\n", yamlScalar(ci.Hostname))
	} else if vmName != "" {
		b.WriteString("hostname: " + yamlScalar(vmName) + "\n")
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

// yamlSafeScalarRe 「可以原样裸写」的值：RFC1123 主机名形状——首尾是字母数字、中间只允许
// 字母数字与 `.`/`-`。裸写这类值不会有任何歧义（既不是 YAML 的保留字面量，也不含指示符）。
var yamlSafeScalarRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

// yamlScalar 把宿主名一类的**自由文本值**渲染成安全的 YAML 标量（round49 缺陷修复）。
//
// 由来（真机形态）：`buildCloudConfig` 曾直接拼 `hostname: <值>`（`fqdn:` 与 `BuildMetaData`
// 的 `local-hostname:` 同样），而 cloud-init 的 hostname 当时没有格式校验 ⇒ 一次合法提交
// `set vnf … cloud-init hostname "branch: blue"` 生成的正是字面 `hostname: branch: blue`，
// YAML 解析直接失败（mapping values are not allowed here）：hostname 落不下去，**整段
// #cloud-config 一起被丢弃**（SSH 公钥、users 都没了），而产品侧提交成功、零提示。
//
// 转义策略（两段判据）：
//  1. **形状**：匹配 yamlSafeScalarRe 的值原样输出——合法 hostname 的历史输出形态逐字不变
//     （既有测试与真机夹具都按这个形状比对）；
//  2. **语义**：形状安全但裸写会被 YAML 解析成**非字符串**的值也加引号（纯数字 → 整数；
//     true/yes/on/null/~ → 布尔与空值）。cloud-init 的 hostname/local-hostname 是字符串
//     字段，拿到整数/布尔即 schema 校验失败——判据与同文件的 netplanScalar 同源（同一缺陷族，
//     那里是 netplan 的 ethernets 键不允许非字符串）。
//
// 其余一律走 YAML **双引号标量**（`quoteYAMLString`）：`\`、`"`、控制字符与不可打印字符
// 全部转义，故值里带换行/制表/冒号也不会破坏结构或注入新行。
//
// 注意：instance-id 的**摘要**仍取宿主名的原始字节（见 BuildMetaData）——转义只影响 YAML 文本，
// 不许改变 instance-id，否则既有 VM 重启后会被 cloud-init 当成新实例、重跑 user-data。
func yamlScalar(s string) string {
	if yamlSafeScalarRe.MatchString(s) && !allDigits(s) && !yamlNonStringLiterals[strings.ToLower(s)] {
		return s
	}
	return quoteYAMLString(s)
}

// quoteYAMLString 把任意字符串渲染成 YAML 双引号标量（可直接嵌进 `key: <结果>` 行）。
//
// 转义表按 YAML 的双引号风格：`\\`、`\"`、`\n`、`\r`、`\t`、`\0`，其余控制字符与不可打印
// 字符按 `\xNN`（≤0xFF）/ `\uNNNN`（≤0xFFFF）/ `\UNNNNNNNN` 写出；可打印字符（含非 ASCII）
// 原样保留。
//
// 非法 UTF-8 字节按字节 `\xNN` 转义。⚠️ **如实边界**：YAML 的 `\xNN` 是**码点**转义，
// 故这一步对非法字节不是**字节级**可逆的（解回来是 U+00NN）；这里的目标只是「结构不破、
// 解析得出来」——非法 UTF-8 本来就不可能是合法主机名。
func quoteYAMLString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			fmt.Fprintf(&b, `\x%02x`, s[i])
			i++
			continue
		}
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case 0:
			b.WriteString(`\0`)
		default:
			if r < 0x20 || r == 0x7f || !unicode.IsPrint(r) {
				switch {
				case r <= 0xff:
					fmt.Fprintf(&b, `\x%02x`, r)
				case r <= 0xffff:
					fmt.Fprintf(&b, `\u%04x`, r)
				default:
					fmt.Fprintf(&b, `\U%08x`, r)
				}
			} else {
				b.WriteRune(r)
			}
		}
		i += size
	}
	b.WriteByte('"')
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
	b.WriteString("local-hostname: " + yamlScalar(hostname) + "\n")
	return b.String()
}

func ensureTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// BuildNetworkConfig 生成 cloud-init **v2** 网络配置（O-4 / 报告 §6.3，决策 #503 / R47-1）。
//
// 由来：产品此前的 seed.iso 只含 `meta-data` + `user-data`（`cloud-localds` 只传两个文件），
// **没有 `network-config`**。对依赖 cloud-init 网络配置的镜像（Cirros、部分 Alpine 变体），
// guest 起来后网卡上没有 DHCP 客户端在跑 ⇒ 拿不到地址 ⇒ cloud-init 反复重试 metadata
// （串口刷 `failed N/0: request failed`），根本进不到登录态，而产品侧日志/告警毫无提示。
//
// 形态：每块 vNIC 一条，以 **vNIC 名**为键，按该 vNIC 的 **MAC 匹配**网卡、把 guest 内名字
// 设回同一个名字，再启用 DHCP：
//
//	ethernets:
//	  nic0:
//	    match:
//	      macaddress: "52:54:8e:b8:b3:7f"
//	    set-name: nic0
//	    dhcp4: true
//
// 为什么不再按 `ethN` 写（旧实现的实证缺陷，round47 真机）：旧实现按「vNIC 声明顺序 ⇒
// guest 内 PCI 槽序 ⇒ eth0/eth1/…」产出配置，这条对应关系只是**经验假设**。Debian 12+/13
// 有 systemd 可预测命名（`/lib/udev/rules.d/80-net-setup-link.rules`）会把 virtio 网卡改名成
// `enp1s0` ⇒ 渲染出的 netplan 指向**不存在的** eth0 ⇒ guest 一个 DHCP 报文都不发 ⇒ 租约永远
// 拿不到；而 VM 是 running、vNIC 是 up，故 `VNF_PORT_DOWN` 不报，产品侧唯一可见信号只是
// 「租约表空」，极易被当成「guest 自己没配网」。Alpine 根 fs 没有该 udev 规则（保持 eth0），
// 历轮夹具全是 Alpine 故未暴露。
//
// 按 MAC 匹配与 guest 的命名策略无关：可预测命名的镜像（enp1s0）、保持 eth0 的镜像（Alpine）、
// 以及镜像自带 udev 规则改名的情形都成立。`set-name` 用 vNIC 名（nic0/nic1…）让 guest 内名字
// 可预测、「按 vNIC 名找设备」成立，同时避开 `eth0` 这个名字在可预测命名镜像上的歧义。
//
// MAC 取值与域定义**同一函数**（DefaultMAC，见 domain_xml.go）：配置显式声明
// `interfaces[].mac` 时用它，否则用 `DefaultMAC(vm.Name, vNIC 名)`——与 buildVhostUserIface /
// buildBridgeIface 的取值完全同源，故 seed 与域定义必然一致。**不在这里自写一份推导**：
// 两处一旦算法漂移，netplan 就匹配不到任何网卡，而复现出来又是「guest 不发 DHCP」的老症状。
// DefaultMAC 是纯函数，seed 在 MAC 尚未下发到域定义之前生成也无碍。
//
// **如实边界**：契约的口径是「每块 vNIC 一条」，故这里对 sriov-vf 型 vNIC 同样产出一条
// （它走 PCI 直通、域定义里是 hostdev 而**没有 `<mac>` 元素**，MAC 由 VF 自身决定）——这类
// 条目里的派生 MAC 与 guest 实际看到的网卡地址未必相同，能否匹配上取决于镜像与 VF 的 MAC
// 策略。本轮按契约不为其做特例（真机环境无 PF/VF，无法验证）；产品侧不受影响的场合是
// vhost-user 型 vNIC（VPP 数据面与内核数据面两条路径都由 DefaultMAC 决定实际 MAC）。
//
// 无 vNIC 时返回**空串**（不产出 network-config，行为与引入时逐字一致）——没有网卡的 VM
// 写一份空的 v2 配置只会让 cloud-init 多一次无意义解析。
func BuildNetworkConfig(vm model.VMFunction) string {
	if len(vm.Interfaces) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("#cloud-init network-config v2（nfvis 生成：按 vNIC 的 MAC 匹配网卡并启用 DHCP）\n")
	b.WriteString("version: 2\n")
	b.WriteString("ethernets:\n")
	for _, nic := range vm.Interfaces {
		mac := nic.MAC
		if mac == "" {
			mac = DefaultMAC(vm.Name, nic.Name)
		}
		// 键与 set-name 都取 vNIC 名（不区分 vNIC 类型：契约 #503 的形态是「每块 vNIC 一条」）。
		name := netplanScalar(nic.Name)
		fmt.Fprintf(&b, "  %s:\n", name)
		b.WriteString("    match:\n")
		fmt.Fprintf(&b, "      macaddress: %q\n", mac)
		fmt.Fprintf(&b, "    set-name: %s\n", name)
		b.WriteString("    dhcp4: true\n")
	}
	return b.String()
}

// yamlNonStringLiterals YAML 1.1（netplan 经 libyaml/PyYAML 解析）里**不加引号会被解析成
// 非字符串**的裸字面量：布尔与空值。
var yamlNonStringLiterals = map[string]bool{
	"true": true, "false": true,
	"yes": true, "no": true,
	"on": true, "off": true,
	"null": true, "~": true,
}

// netplanScalar 把 vNIC 名渲染成 netplan 配置里的 YAML 键与取值。
//
// vNIC 名在提交期已按 `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$` 校验（internal/model 的 nameRe），
// 绝大多数名字可原样写出；但**纯数字**与上表的 YAML 裸字面量不加引号会被解析成非字符串——
// netplan 的 `ethernets` 键必须是字符串，届时整份配置校验失败，退回「guest 没有网络配置」
// 的老症状。这类名字加双引号即恢复字面量语义（普通名字保持逐字输出，读起来与契约一致）。
func netplanScalar(name string) string {
	if name == "" {
		return `""`
	}
	if allDigits(name) || yamlNonStringLiterals[strings.ToLower(name)] {
		return `"` + name + `"`
	}
	return name
}

// allDigits 判断 s 全为十进制数字（s 非空）。
func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// cloudLocaldsArgs 构造 cloud-localds 的参数（纯函数，便于守护「传参形态」）。
//
// 形态（按 `cloud-localds --help`，Ubuntu 26.04 的 cloud-image-utils）：
//
//	cloud-localds [options] output user-data [meta-data]
//	  -N | --network-config F   write network config file to local datasource
//
// networkConfigPath 为空 ⇒ 旧的两件形态（行为与修复前逐字一致）。
func cloudLocaldsArgs(isoPath, userData, metaData, networkConfigPath string) []string {
	args := []string{isoPath}
	if networkConfigPath != "" {
		args = append(args, "--network-config="+networkConfigPath)
	}
	return append(args, userData, metaData)
}
