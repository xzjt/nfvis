package netkernel

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
)

// ifnameMax 内核接口名长度上限（IFNAMSIZ-1，不含结尾 NUL）。
const ifnameMax = 15

// linkNameHashDigits 截断名里的哈希位数（8 位十六进制 = FNV-1a 32 位全宽）。
//
// 旧实现用 4 位（16 位哈希）+ 10 字符前缀：两个合法的 13 字符交换机名只要共享 7 字符前缀、
// 且 16 位哈希相同，就派生出**同一个** `vr-…` 设备（同一张 VRF 表）——而 FNV-1a 可逆，
// 这样的名字对离线就能构造（round2 体检 R2-21 的反例）。加宽到全宽后该形态不再可构造。
const linkNameHashDigits = 8

// linkNamePrefixBudget 截断时保留的名字前缀字符数（总长 15 − 1 个分隔符 − 哈希位数）。
const linkNamePrefixBudget = ifnameMax - 1 - linkNameHashDigits

// LinkName 把产品侧对象名（交换机/隧道名）映射为内核接口名。
//
// 名字 ≤15 字节时原样使用（读视图与配置名一致，便于排障）；超长时按「前缀 + 全宽哈希」截断，
// 前缀也占长度预算——保证不同对象不会映射到同一个内核接口名（R2-21：旧实现的 16 位哈希
// 给得出可离线构造的撞名反例）。
func LinkName(name string) string {
	name = strings.TrimSpace(name)
	if len(name) <= ifnameMax {
		return name
	}
	sum := fnv.New32a()
	_, _ = sum.Write([]byte(name))
	// 前缀可能以 '-' 结尾（`vr-` 这类前缀被截到界线时），去掉避免出现 `--` 的怪名字；
	// 去尾不引入撞名：区分对象的是前缀后的**全宽哈希**（它取自完整名字）。
	prefix := strings.TrimRight(name[:linkNamePrefixBudget], "-")
	return fmt.Sprintf("%s-%08x", prefix, sum.Sum32())
}

// VRFTableID 由 VRF（L3 交换机）名确定性派生内核路由表号（1..2^31-1）。
//
// 内核 VRF 必须绑定一张路由表；产品侧没有「表号」这个配置项，故由名字派生——同一交换机
// 在任意一次装配/恢复重放中得到同一张表，读视图（`ip route show table <n>`）可复现。
// 名字不同而派生值相撞的概率可忽略（FNV-1a 32 位取低 31 位），且提交期会校验交换机名唯一。
func VRFTableID(name string) int {
	sum := fnv.New32a()
	_, _ = sum.Write([]byte(name))
	return int(sum.Sum32()&0x7fffffff) + 1
}

// GatewayVRFName L2 交换机网关（BVI 等价物）所在 VRF 的内核接口名。
//
// 与 VPP 侧同名约定（`vr-<交换机名>`）：网关地址不能落在默认表，否则与主机的默认路由
// 互相污染；无显式 vrf 声明时用它。
func GatewayVRFName(swName string) string { return LinkName("vr-" + swName) }

// TapName VM vNIC 的宿主侧 tap 名（内核数据面：virtio + vhost-net + tap）。
//
// libvirt 的 `<interface type='bridge'>` 会自行创建 tap 并挂 bridge，产品不预先创建；
// 本函数只用于读视图与排障提示（把 vNIC 名映射回可识别的 tap 前缀）。
func TapName(vmName, ifaceName string) string {
	return LinkName("nfv-" + vmName + "-" + ifaceName)
}

// VlanSubifName L3 接口的 VLAN 子接口名（`<iface>.<vid>`）。
//
// 返回值必须能被内核接受：`<iface>.<vid>` 超过 IFNAMSIZ-1 时**如实报错**（旧实现直接返回
// 一个内核装不上的名字，错误要到后面的 `ip link add` 才出现、且指不到「基口名太长」；
// 提交期对派生名的校验由校验侧另行负责，本函数的调用方一律上抛本错误）。
func VlanSubifName(iface string, vid int) (string, error) {
	name := fmt.Sprintf("%s.%d", iface, vid)
	if len(name) > ifnameMax {
		maxBase := ifnameMax - 1 - len(strconv.Itoa(vid))
		return "", fmt.Errorf("VLAN 子接口名 %q（基口 %s + vlan %d）超过内核接口名上限 %d 个字符；"+
			"内核数据面下请把基口名缩短到 %d 个字符以内，或改用不建 vlan 子接口的三层接口形态",
			name, iface, vid, ifnameMax, maxBase)
	}
	return name, nil
}

// alreadyExists 判断命令失败是否只是「对象已存在」（幂等路径）。
//
// 只收**真机观察到的精确短语**：旧实现里那条裸子串 "exist" 会把 `does not exist`
// （设备不存在）也算成「已存在」，于是 ipIdem/tcIdem 之类把真失败当成功（R2-13③）。
func alreadyExists(out string, err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(out)
	// 真机实测的「已存在」文案（不同 iproute2 子命令措辞不同）：
	//   ip link add      → RTNETLINK answers: File exists
	//   ip link add      → Error: device 'bond0' already exists
	//   ip addr add      → Error: ipv4: Address already assigned
	// 地址下发已改用 `ip addr replace`（原生幂等），这里保留匹配作为冗余保险。
	for _, m := range []string{"file exists", "already exists", "already assigned"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// notFound 判断命令失败是否只是「对象不存在」（删除路径的幂等容错）。
func notFound(out string, err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(out)
	// 真机实测的「不存在」文案（各工具/子命令措辞不同，逐条收进来）：
	//   ip link del/set      → Cannot find device
	//   tc qdisc del（无该 qdisc） → Cannot find specified qdisc on specified device
	//   tc filter del（无该 filter） → Cannot find specified filter chain
	//   tc qdisc del（父 qdisc 不存在） → Parent Qdisc doesn't exists.
	//   nft delete chain     → No such file or directory
	// 清理路径一律按"已达成"处理；**下发路径不用本函数**（用 ipReq/tcReq），故不会掩盖真失败。
	for _, m := range []string{"cannot find device", "cannot find specified", "no such",
		"not found", "does not exist", "doesn't exist"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
