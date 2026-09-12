package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------- 模拟引擎（真实实现中替换为对 nfvisd 的调用） ----------

type Engine struct {
	Mode           string // oper | config
	cfgPath        []string
	candidate      map[string]string // path a|b|c -> value
	committed      map[string]string
	snapshots      []map[string]string // 历史快照（最新在末尾）
	confirmedTimer *time.Timer
	vmState        map[string]string // 模拟 VM 电源状态
	history        []string
	dirty          bool
}

func NewEngine() *Engine {
	committed := map[string]string{
		"system|hostname":                       "nfvis-node1",
		"resource-pools|hugepages|page-size":    "1G",
		"resource-pools|hugepages|count":        "32",
		"resource-pools|cpu|isolated-cores":     "4-15",
		"vpp|cpu|main-core":                     "4",
		"vpp|cpu|corelist-workers":              "5,7",
		"vpp|memory|hugepage-preference":        "1G",
		"virtual-switches|vs-app|type":          "l2",
		"virtual-switches|vs-app|vlan|access":   "100",
		"virtual-switches|vs-app|gateway|ip":    "192.168.100.1/24",
		"virtual-switches|vs-mgmt|type":         "l3",
		"virtual-machine-functions|fw-vm|image": "ubuntu22-vm",
		"virtual-machine-functions|fw-vm|vcpu|count":            "4",
		"virtual-machine-functions|fw-vm|memory|size-mb":        "8192",
		"virtual-machine-functions|fw-vm|memory|hugepage-size":  "1G",
		"virtual-machine-functions|fw-vm|memory|backing":        "hugepage",
		"virtual-machine-functions|fw-vm|interfaces|eth0|type":  "vhost-user",
		"virtual-machine-functions|fw-vm|interfaces|eth0|virtual-switch": "vs-app",
	}
	return &Engine{
		Mode:      "oper",
		candidate: cloneMap(committed),
		committed: committed,
		vmState:   map[string]string{"fw-vm": "running", "probe-vm": "stopped"},
	}
}

func cloneMap(m map[string]string) map[string]string {
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// dynamic 提供动态补全候选
func (e *Engine) dynamic(kind string) []string {
	switch kind {
	case "ifnames":
		return []string{"ens2f0", "ens2f1", "ens2f2", "ens2f3"}
	case "vswitches":
		return e.listKeys("virtual-switches")
	case "vmnames":
		return e.listKeys("virtual-machine-functions")
	case "ctnames":
		return []string{"sbc-ct1", "dns-ct1"}
	case "images":
		return []string{"ubuntu22-vm", "vrouter-vm", "alpine-ct"}
	case "vppplugins":
		return []string{"acl", "nat", "span", "linux-cp", "dpdk"}
	case "revisions":
		var out []string
		for i := 0; i < len(e.snapshots); i++ {
			out = append(out, strconv.Itoa(len(e.snapshots)-i))
		}
		return out
	}
	return nil
}

func (e *Engine) listKeys(head string) []string {
	seen := map[string]bool{}
	for k := range e.committed {
		p := strings.SplitN(k, "|", 3)
		if len(p) >= 2 && p[0] == head {
			seen[p[1]] = true
		}
	}
	var out []string
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Prompt 返回当前提示符
func (e *Engine) Prompt() string {
	if e.Mode == "config" {
		if len(e.cfgPath) == 0 {
			return "nfvis# "
		}
		return fmt.Sprintf("[edit %s] nfvis# ", strings.Join(e.cfgPath, " "))
	}
	return "nfvis> "
}

// Execute 执行一行命令（模式相关），返回输出文本
func (e *Engine) Execute(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	if e.Mode == "oper" {
		return e.execOper(line)
	}
	return e.execConfig(line)
}

// ---------- 操作模式 ----------

func (e *Engine) execOper(line string) string {
	t := strings.Fields(line)
	switch t[0] {
	case "show":
		return e.operShow(t[1:])
	case "request":
		return e.operRequest(t[1:])
	case "configure":
		e.Mode = "config"
		e.cfgPath = nil
		return ""
	case "ping":
		if len(t) < 2 {
			return "%% 语法: ping <host>\n"
		}
		return fmt.Sprintf("PING %s: 56 data bytes\n64 bytes from %s: seq=0 ttl=64 time=0.412 ms\n--- %s ping statistics ---\n1 packets transmitted, 1 received, 0%% loss\n", t[1], t[1], t[1])
	case "help":
		return "可用顶级命令: show  request  configure  ping  exit  help\n? 与 Tab 在任意位置可用\n"
	case "exit":
		return "\x00exit"
	}
	return fmt.Sprintf("%% 无效命令: %s（输入 ? 查看可用命令）\n", line)
}

func (e *Engine) operShow(t []string) string {
	switch {
	case len(t) == 0:
		return "  version  system  interfaces  virtual-switches  vrfs  vpp  virtual-machine-functions\n  container-functions  images  resource-pools  alarms  configuration  users\n"
	case t[0] == "version":
		return "NFViS 1.0.0 (prototype)\nUbuntu 26.04 LTS / VPP 26.06 / DPDK 25.11 / libvirt 11.x / QEMU 10.x / Docker 28.x\n"
	case t[0] == "system":
		if len(t) < 2 {
			return "  uptime  cpu  hugepages  storage\n"
		}
		switch t[1] {
		case "uptime":
			return " 12:34:56 up 3 days,  2:11,  1 user,  load average: 0.42, 0.38, 0.35\n"
		case "cpu":
			return "CPU total 24, isolated 4-15 (资源池), usage 12%\n"
		case "hugepages":
			return "1G pages: total 32, free 26 (分配: fw-vm 8)\n"
		case "storage":
			return "/data 200G total, 86G used (images 64G)\n"
		}
	case t[0] == "interfaces":
		if len(t) == 1 {
			return "Interface      Admin  Link  Speed   Description\n" +
				"ens2f0         up     up    10000   to-TOR\nens2f1         up     up    10000   \nmgmt0          up     up    1000    management\n"
		}
		return fmt.Sprintf("%s: admin up, link up, 10GbE, mtu 9000, driver ice-dpdk, numa 0\n  rx 1.2G pkts  tx 1.1G pkts  rx-drops 0  tx-drops 0\n", t[1])
	case t[0] == "virtual-switches":
		if len(t) == 1 {
			return "Name       Type  Ports  Description\nvs-app     l2    2      \nvs-mgmt    l3    0      \n"
		}
		return fmt.Sprintf("%s: type l2, access-vlan 100\n  ports: 1 interface ens2f0 (up), 2 vnf fw-vm/eth0 (up)\n  mac-table: 2 entries\n", t[1])
	case t[0] == "vrfs":
		return "Name       L3-Ifaces  Routes\nvs-mgmt    1          3\n"
	case t[0] == "virtual-machine-functions":
		if len(t) == 1 {
			return "Name       State     vCPU  Mem(MB)  Image\nfw-vm      running   4     8192     ubuntu22-vm\nprobe-vm   stopped   2     4096     ubuntu22-vm\n"
		}
		return fmt.Sprintf("%s: state %s, vcpu 4 pinned [4-7], memory 8192MB (1G pages, numa 0)\n  eth0: vhost-user -> vs-app, mac 52:54:00:aa:00:01, link up\n", t[1], e.vmState[t[1]])
	case t[0] == "container-functions":
		return "Name       State     Image\nsbc-ct1    running   alpine-ct\ndns-ct1    running   alpine-ct\n"
	case t[0] == "images":
		return "Name          Type            Size      Refs\nubuntu22-vm   vm-image        2.1G      2\nalpine-ct     container-image 5.4M      2\n"
	case t[0] == "resource-pools":
		return "Hugepages 1G: total 32, allocated 8, free 24\nIsolated cores: 4-15, allocated: vpp [4,5,7] (vpp-reserved), fw-vm [6,8-10]\n"
	case t[0] == "vpp":
		if len(t) == 1 {
			return fmt.Sprintf("VPP 26.06 (DPDK 25.11), state running\nthreads: main(4) + workers [5,7]\nbuffers: 16385/NUMA, in-use 2%%\nmain-heap 1G, hugepages 1G x32\nconfig revision: %d\n", len(e.snapshots))
		}
		switch t[1] {
		case "threads":
			return "ID  Name    Core  Lcore  Type\n0   vpp_main  4   4      workers\n1   vpp_wk_0  5   5      workers\n2   vpp_wk_1  7   7      workers\n"
		case "runtime":
			return "Thread  Vector  Calls  Vectors/Call  Clocks/Vector\nvpp_main  1.1e3  9.2e5  12.4  4312\nvpp_wk_0  6.4e3  4.1e6  18.9  2650\nvpp_wk_1  6.2e3  4.0e6  18.7  2678\n"
		case "buffers":
			return "Pool: numa 0, hugepage 1G, data 9.2KB, available 16384, in-use 312\n"
		case "memory":
			return "main-heap: 1G, used 112M\nhugepage: 1G x32, used 8 (fw-vm 8, vpp 0 共享)\n"
		case "capture":
			return "无进行中的抓包会话\npcap: cap-ens2f0-20260912.pcap (4.2MB)\n"
		}
		return fmt.Sprintf("%% 无效命令: show vpp %s\n", t[1])
	case t[0] == "alarms":
		return "Severity  Code          Source                        Raised\nwarning   VS-PORT-DOWN  virtual-switches/vs-app:3     2026-09-12 08:21:03\n"
	case t[0] == "configuration":
		if len(t) >= 2 && t[1] == "candidate" {
			return e.renderConfig(e.candidate) + "\n"
		}
		if len(t) >= 3 && t[1] == "rollback" {
			n, ok := e.snapshotByIndex(t[2])
			if !ok {
				return fmt.Sprintf("%% 无快照 %s\n", t[2])
			}
			return e.diffConfig(n, e.committed)
		}
		return e.renderConfig(e.committed) + "\n"
	case t[0] == "users":
		return "User    Class\nadmin   super-user\nnetop   operator\n"
	}
	return fmt.Sprintf("%% 无效命令: show %s\n", strings.Join(t, " "))
}

func (e *Engine) operRequest(t []string) string {
	if len(t) >= 3 && t[0] == "virtual-machine-functions" {
		name, action := t[1], t[2]
		if _, ok := e.vmState[name]; !ok {
			return fmt.Sprintf("%% VNF %s 不存在\n", name)
		}
		switch action {
		case "start":
			e.vmState[name] = "running"
			return fmt.Sprintf("VNF %s 已启动\n", name)
		case "stop":
			e.vmState[name] = "stopped"
			return fmt.Sprintf("VNF %s 已停止\n", name)
		case "restart":
			return fmt.Sprintf("VNF %s 重启中\n", name)
		case "console":
			return fmt.Sprintf("（原型）已连接 %s 串口，Ctrl-] 退出\nUbuntu 22.04 fw-vm ttyS0 login: ", name)
		}
	}
	if len(t) >= 2 && t[0] == "vpp" && t[1] == "restart" {
		return "（原型）startup.conf 已按 committed 配置重新生成，VPP 重启中... 网络配置将经恢复收敛重放\n"
	}
	if len(t) >= 2 && t[0] == "system" {
		switch t[1] {
		case "reboot":
			return "（原型）系统重启中...\n"
		case "backup":
			return "（原型）配置已备份至 /data/backup/nfvis-cfg-20260912.tar.gz\n"
		}
	}
	return fmt.Sprintf("%% 无效命令: request %s\n", strings.Join(t, " "))
}

// ---------- 配置模式 ----------

func (e *Engine) execConfig(line string) string {
	t := strings.Fields(line)
	switch t[0] {
	case "set":
		return e.cfgSet(t[1:], false)
	case "delete":
		return e.cfgSet(t[1:], true)
	case "show":
		sub := t[1:]
		m := filterByPath(e.candidate, e.cfgPath)
		if len(sub) > 0 && sub[0] == "configuration" {
			sub = sub[1:]
		}
		if len(sub) > 0 {
			m = filterByPath(e.candidate, append(append([]string{}, e.cfgPath...), sub...))
		}
		if len(m) == 0 {
			return "（无匹配配置）\n"
		}
		return e.renderConfig(m) + "\n"
	case "commit":
		return e.cfgCommit(t[1:])
	case "rollback":
		if len(t) < 2 {
			return "%% 语法: rollback <n>\n"
		}
		snap, ok := e.snapshotByIndex(t[1])
		if !ok {
			return fmt.Sprintf("%% 无快照 %s\n", t[1])
		}
		e.candidate = cloneMap(snap)
		e.dirty = true
		return fmt.Sprintf("candidate 已替换为快照 %s，需 commit 生效\n", t[1])
	case "compare":
		return e.diffConfig(e.committed, e.candidate)
	case "edit":
		if len(t) < 2 {
			return "%% 语法: edit <path>\n"
		}
		e.cfgPath = append(e.cfgPath, t[1:]...)
		return ""
	case "up":
		if len(e.cfgPath) > 0 {
			e.cfgPath = e.cfgPath[:len(e.cfgPath)-1]
		}
		return ""
	case "top":
		e.cfgPath = nil
		return ""
	case "run":
		return e.execOper(strings.Join(t[1:], " "))
	case "discard":
		e.candidate = cloneMap(e.committed)
		e.dirty = false
		return "candidate 已丢弃\n"
	case "exit":
		if e.dirty {
			return "%% 存在未提交变更，先 commit 或 discard\n"
		}
		e.Mode = "oper"
		e.cfgPath = nil
		return ""
	}
	return fmt.Sprintf("%% 无效命令: %s（输入 ? 查看可用命令）\n", line)
}

// cfgSet 处理 set/delete：路径按配置树逐 token 校验（关键字/参数实例/取值），
// 叶子以 path 为键存入 candidate。delete 允许以任意前缀路径删除子树。
func (e *Engine) cfgSet(t []string, del bool) string {
	if len(t) == 0 {
		return "%% 语法: set <path> <value>\n"
	}
	full := append(append([]string{}, e.cfgPath...), t...)
	cur := configPathTree()
	var path []string
	value, hasValue := "", false
	i := 0
	for i < len(full) {
		tok := full[i]
		if cur.Kind == KindValue { // 当前节点即取值节点：该 token 是它的值
			value, hasValue = tok, true
			i++
			if i < len(full) {
				return fmt.Sprintf("%% 多余参数: %q\n", full[i])
			}
			break
		}
		var c *Node
		for _, ch := range cur.Children { // 具名匹配
			if ch.Name == tok {
				c = ch
				break
			}
		}
		if c == nil { // 参数节点匹配（实例名）
			for _, ch := range cur.Children {
				if ch.Kind == KindParam {
					c = ch
					break
				}
			}
		}
		if c == nil { // 取值（KindValue 子节点）
			if vc := valueChild(cur); vc != nil {
				value, hasValue = tok, true
				i++
				if i < len(full) {
					return fmt.Sprintf("%% 多余参数: %q\n", full[i])
				}
				break
			}
			return e.unknownNodeHelp(tok, cur)
		}
		if c.Kind == KindParam && len(c.Children) == 0 {
			// 无子树的参数节点（<image>、<ip> 等）：该 token 即取值，不入路径
			value, hasValue = tok, true
			i++
			if i < len(full) {
				return fmt.Sprintf("%% 多余参数: %q\n", full[i])
			}
			break
		}
		path = append(path, tok) // 关键字名或参数实例名
		i++
		cur = c
	}
	if !del && !hasValue {
		switch {
		case cur == nil || len(cur.Children) == 0:
			// 标志型叶子（如 disable）或参数节点终止：允许无值
		case valueChild(cur) != nil:
			return "%% 缺少值\n"
		default:
			return "%% 配置不完整，缺少子节点或值\n"
		}
	}
	if del {
		prefix := strings.Join(path, "|")
		removed := 0
		for k := range e.candidate {
			if k == prefix || strings.HasPrefix(k, prefix+"|") {
				delete(e.candidate, k)
				removed++
			}
		}
		if removed == 0 {
			return fmt.Sprintf("%% 无匹配配置: %s\n", strings.Join(path, " "))
		}
		e.dirty = true
		return fmt.Sprintf("已删除 %d 条语句（未提交）\n", removed)
	}
	key := strings.Join(path, "|")
	e.candidate[key] = value
	e.dirty = true
	if value != "" {
		return fmt.Sprintf("[ok] %s %s\n", strings.Join(path, " "), value)
	}
	return fmt.Sprintf("[ok] %s\n", strings.Join(path, " "))
}

// valueChild 返回节点的取值子节点（若有）
func valueChild(n *Node) *Node {
	for _, ch := range n.Children {
		if ch.Kind == KindValue {
			return ch
		}
	}
	return nil
}

func (e *Engine) unknownNodeHelp(word string, n *Node) string {
	var cands []string
	for _, c := range n.candidates("", e) {
		cands = append(cands, c[0])
	}
	return fmt.Sprintf("%% 未知节点 %q。可用: %s\n", word, strings.Join(cands, "  "))
}

func (e *Engine) cfgCommit(t []string) string {
	confirmed := len(t) > 0 && t[0] == "confirmed"
	// 语义校验
	for k, v := range e.candidate {
		// 镜像必须存在
		if strings.HasSuffix(k, "|image") && !contains([]string{"ubuntu22-vm", "vrouter-vm", "alpine-ct"}, v) {
			return fmt.Sprintf("%% 校验失败: %s: 仓库中不存在镜像 %q\n", strings.ReplaceAll(k, "|", " "), v)
		}
	}
	// vhost-user 必须大页内存（FR-CFG-011①）
	for k, v := range e.candidate {
		if strings.HasSuffix(k, "|type") && v == "vhost-user" {
			p := strings.Split(k, "|") // virtual-machine-functions|<vm>|interfaces|<vnic>|type
			if backing, ok := e.candidate["virtual-machine-functions|"+p[1]+"|memory|backing"]; ok && backing != "hugepage" {
				return fmt.Sprintf("%% 校验失败: %s: vhost-user 要求大页内存（memory backing=%s）\n",
					strings.ReplaceAll(k, "|", " "), backing)
			}
		}
	}
	// VPP 线程核必须在隔离核池内（FR-SYS-010）
	iso, isoSet := e.candidate["resource-pools|cpu|isolated-cores"]
	for _, k := range []string{"vpp|cpu|main-core", "vpp|cpu|corelist-workers"} {
		v, ok := e.candidate[k]
		if !ok {
			continue
		}
		if !isoSet {
			return fmt.Sprintf("%% 校验失败: %s: 必须先配置 resource-pools cpu isolated-cores\n", strings.ReplaceAll(k, "|", " "))
		}
		for _, c := range parseCores(v) {
			if !containsInt(parseCores(iso), c) {
				return fmt.Sprintf("%% 校验失败: %s: 核 %d 不在隔离核池 [%s] 内\n", strings.ReplaceAll(k, "|", " "), c, iso)
			}
		}
	}
	// per-NIC dpdk dev 覆盖的接口必须是 DPDK 接管的物理口
	for k := range e.candidate {
		p := strings.Split(k, "|")
		if len(p) == 5 && p[0] == "vpp" && p[2] == "dev" && !contains(e.dynamic("ifnames"), p[3]) {
			return fmt.Sprintf("%% 校验失败: dpdk dev %s: 不是 DPDK 接管的物理口（show interfaces physical）\n", p[3])
		}
	}
	// VPP 大页偏好必须与资源池页大小一致（FR-SYS-010）
	if hp, ok := e.candidate["vpp|memory|hugepage-preference"]; ok {
		if ps, ok2 := e.candidate["resource-pools|hugepages|page-size"]; ok2 && hp != ps {
			return fmt.Sprintf("%% 校验失败: vpp memory hugepage-preference (%s) 与 resource-pools hugepages page-size (%s) 不一致\n", hp, ps)
		}
	}
	// VM 指定的页大小池必须有对应资源（FR-CFG-011⑪，原型为单池简化模型）
	for k, v := range e.candidate {
		if strings.HasSuffix(k, "|hugepage-size") && strings.HasPrefix(k, "virtual-machine-functions|") {
			if ps, ok := e.candidate["resource-pools|hugepages|page-size"]; ok && v != ps {
				return fmt.Sprintf("%% 校验失败: %s: 页大小 %s 无对应资源池（现有 %s 池）\n",
					strings.ReplaceAll(k, "|", " "), v, ps)
			}
		}
	}
	// 管理口自锁警告（FR-CFG-012）
	mgmtWarn := ""
	if e.candidate["system|management|ip|address"] != e.committed["system|management|ip|address"] {
		mgmtWarn = "%% 警告: 管理口地址变更可能中断当前 SSH 会话，建议使用 commit confirmed\n"
	}
	// vpp 相关键是否有变更（待重启生效，FR-SYS-009）
	restartWarn := ""
	for _, m := range []map[string]string{e.candidate, e.committed} {
		for k := range m {
			if strings.HasPrefix(k, "vpp|") && e.committed[k] != e.candidate[k] {
				restartWarn = "%% 警告: vpp 变更需 request vpp restart（或整机 reboot）后生效\n"
				break
			}
		}
		if restartWarn != "" {
			break
		}
	}
	e.snapshots = append(e.snapshots, cloneMap(e.committed))
	if len(e.snapshots) > 50 {
		e.snapshots = e.snapshots[1:]
	}
	e.committed = cloneMap(e.candidate)
	e.dirty = false
	rev := len(e.snapshots)
	if confirmed {
		prev := e.snapshots[len(e.snapshots)-1]
		if e.confirmedTimer != nil {
			e.confirmedTimer.Stop()
		}
		e.confirmedTimer = time.AfterFunc(15*time.Second, func() {
			e.committed = cloneMap(prev)
			e.candidate = cloneMap(prev)
			fmt.Print("\n%% commit confirmed 超时，已自动回滚并产生告警\n")
		})
		return fmt.Sprintf("commit 成功 (revision %d)，confirmed 模式：15 秒内再次 commit 确认，否则自动回滚\n%s%s", rev, mgmtWarn, restartWarn)
	}
	if e.confirmedTimer != nil {
		e.confirmedTimer.Stop()
		e.confirmedTimer = nil
		return fmt.Sprintf("commit 成功 (revision %d)，confirmed 已确认\n%s%s", rev, mgmtWarn, restartWarn)
	}
	return fmt.Sprintf("commit 成功 (revision %d)\n%s%s", rev, mgmtWarn, restartWarn)
}

// parseCores 解析核列表 "5,7,9-11"
func parseCores(s string) []int {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if strings.Contains(part, "-") {
			var a, b int
			if _, err := fmt.Sscanf(part, "%d-%d", &a, &b); err == nil && a <= b {
				for x := a; x <= b; x++ {
					out = append(out, x)
				}
			}
		} else if x, err := strconv.Atoi(part); err == nil {
			out = append(out, x)
		}
	}
	return out
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (e *Engine) snapshotByIndex(s string) (map[string]string, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > len(e.snapshots) {
		return nil, false
	}
	return e.snapshots[len(e.snapshots)-n], true
}

// ---------- 渲染 ----------

func filterByPath(m map[string]string, path []string) map[string]string {
	prefix := strings.Join(path, "|")
	out := map[string]string{}
	if prefix == "" {
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	for k, v := range m {
		if k == prefix || strings.HasPrefix(k, prefix+"|") {
			out[k] = v
		}
	}
	return out
}

// renderConfig 以 JunOS 风格渲染配置（原型简化版）
func (e *Engine) renderConfig(m map[string]string) string {
	var b strings.Builder
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := strings.Split(k, "|")
		for i, seg := range p {
			if i == len(p)-1 {
				fmt.Fprintf(&b, "%s%s;\n", strings.Repeat("    ", i), seg+" "+m[k])
			} else {
				fmt.Fprintf(&b, "%s%s {\n", strings.Repeat("    ", i), seg)
			}
		}
		for i := len(p) - 2; i >= 0; i-- {
			fmt.Fprintf(&b, "%s}\n", strings.Repeat("    ", i))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// diffConfig 输出 JunOS 风格 a ⇄ b 差异
func (e *Engine) diffConfig(a, b map[string]string) string {
	var bld strings.Builder
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	changed := false
	for _, k := range sorted {
		p := strings.Split(k, "|")
		ctx := "[edit " + strings.Join(p[:len(p)-1], " ") + "]"
		if len(p) == 1 {
			ctx = "[edit]"
		}
		av, aok := a[k]
		bv, bok := b[k]
		if aok && (!bok || av != bv) {
			fmt.Fprintf(&bld, "%s\n-   %s %s\n", ctx, p[len(p)-1], av)
			changed = true
		}
		if bok && (!aok || av != bv) {
			fmt.Fprintf(&bld, "%s\n+   %s %s\n", ctx, p[len(p)-1], bv)
			changed = true
		}
	}
	if !changed {
		return "（无差异）"
	}
	return bld.String()
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
