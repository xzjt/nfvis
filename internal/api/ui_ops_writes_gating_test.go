package api

// 决策 #327：**运维动作页家族的写控件按 class 收敛**的守护（修 round105 登记的 R105-1）。
//
// 由来：round105 用 Browser Use 以 operator 账号看到「运维动作」页仍渲染出 super-user 级写
// 入口（软件版本卡、内核基线卡），可见文本里还出现「恢复出厂 / 重置数据分区 / 回退版本 /
// 重签自签证书」。既有机制本身没坏——`body.role-nonsuper [data-write]:not([data-op])` 会把
// 未标 `data-op` 的写入口整类隐藏（真的隐藏，见下），漏的是**个别动态生成的写控件没经 `wbtn`
// 打 `data-write`**（本轮排查抓到「镜像列表的『删除』」这一条，见本文件末）。
//
// 与 ui_operator_gating_test.go（决策 #324：查 data-op 集合）互补——那条查"哪些被放行"，
// 这条查"**逐卡每个控件该不该标、标的 class 与服务端 class 是否一致**"：
//   ① 运维动作页家族（#/ops/actions、#/system/kernel、#/system/tls、#/ops/capture、
//      #/ops/diagnostics）与对象页家族（#/network、#/network/switches[/:name]、
//      #/network/vrfs|acls|bonds|qos|span/:name——配置页去中心化第一刀的就地增删改入口）
//      里每个可交互控件**逐条**列进清单，标注期望 class；
//   ② class 判据 = 该控件触发的 **REST 端点服务端声明的 class**（从 server.go 现读）：
//      S/Su → 只能 super 见（hide：`data-write` 且**无** `data-op`）；
//      O     → operator 可见（op：`data-write` **且** `data-op`）；
//      R / 无写 → 读入口（read：不带 `data-write`）。
//   ③ **新增/删除控件、或标错 class**，只要没同步更新本清单，测试即失败——这正是 R105-1
//      那类"加了控件忘了按 class 处理"能再次混过去的缺口。
//
// 真行为（浏览器里以各角色实际登录后看到什么）仍由人工浏览器验收负责（决策 #141）——
// 本用例替代不了它，只保证**源码这一层的清单与 class 判据自洽**。

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 写入口的三种期望 class（与 CSS 机制一一对应）。
const (
	uiClassHide = "hide" // S/Su：非 super 隐藏（data-write，无 data-op）
	uiClassOp   = "op"   // O：operator 可用（data-write + data-op）
	uiClassRead = "read" // 读/无写：不带 data-write
)

// uiPageControl：一个可交互控件的期望 class 与它触发的 REST 端点（"METHOD /path"，读入口留空）。
type uiPageControl struct {
	class string
	ep    string
}

var (
	// page 分段（非贪婪到 </section>；本文件不处理嵌套 section——控制台页面不嵌套）。
	uiPageSectionRe = regexp.MustCompile(`(?s)<section class="page" id="([^"]+)"[^>]*>(.*?)</section>`)
	// 可交互控件：button / input / select / textarea。
	uiControlTagRe = regexp.MustCompile(`<(?:button|input|select|textarea)\b[^>]*>`)
	// server.go 路由注册（把源码压成单行后匹配）：METHOD + 路径 + 其后的处理器实参文本。
	uiServerRouteRe = regexp.MustCompile(`mux\.Handle\("(GET|POST|PUT|DELETE|PATCH) "\+APIPrefix\+"([^"]+)", ([^)]*)\)`)
)

// uiOpsFamilyControls：**运维动作页家族的逐卡控件清单**（决策 #327 的登记表）。
// 每个条目就是一条"界面期望"，改动这里必须是有意的；漏标/多标/class 变了都会让用例失败。
var uiOpsFamilyControls = map[string]map[string]uiPageControl{
	// #/ops/actions 运维动作
	"page-ops": {
		"ops-backup-btn":      {uiClassHide, "POST /system/backup"},                  // 生成配置备份（S）
		"ops-sshkey-btn":      {uiClassHide, "POST /system/ssh-host-key:regenerate"}, // 重生成 SSH host key（Su）
		"ops-ntp-btn":         {uiClassHide, "POST /system/ntp:sync"},                // 立即同步时间（S）
		"ops-vpp-edit-btn":    {uiClassHide, "PUT /configuration/candidate"},         // 编辑数据面（VPP）配置（S；系统族第三刀就地编辑）
		"ops-techsupport-btn": {uiClassHide, "POST /system/tech-support"},            // 生成 tech-support（S）
		"ops-coredumps-btn":   {uiClassRead, "GET /system/core-dumps"},               // 列出 core dump（读）
		"ops-alarms-btn":      {uiClassHide, "POST /alarms:clear"},                   // 清除已恢复告警（S）
		"ops-export-url":      {uiClassOp, "POST /system/core-dumps:export"},         // 导出清单目标址（O）
		"ops-export-btn":      {uiClassOp, "POST /system/core-dumps:export"},         // 导出 core dump 清单（O）
		"ops-sw-pkg":          {uiClassHide, "POST /system/software"},                // 升级包路径（S）
		"ops-sw-sha":          {uiClassHide, "POST /system/software"},                // 升级包 sha256（S）
		"ops-sw-add-btn":      {uiClassHide, "POST /system/software"},                // 安装升级包（S）
		"ops-sw-rollback-btn": {uiClassHide, "POST /system/software:rollback"},       // 回退到上一版本（S）
		"ops-restore-file":    {uiClassHide, "POST /system/restore"},                 // 恢复所选归档（S）
		"ops-restore-refresh": {uiClassRead, "GET /system/backup"},                   // 刷新备份列表（读）
		"ops-restore-btn":     {uiClassHide, "POST /system/restore"},                 // 从所选归档恢复配置（S）
		"ops-zeroize-btn":     {uiClassHide, "POST /system:zeroize"},                 // 恢复出厂（S）
		"ops-formatdata-btn":  {uiClassHide, "POST /system:format-data"},             // 重置数据分区（S）
		"ops-vpprestart-btn":  {uiClassHide, "POST /vpp/restart"},                    // 重启数据面（Su）
		"ops-reboot-btn":      {uiClassHide, "POST /system:reboot"},                  // 重启主机（S）
		"ops-shutdown-btn":    {uiClassHide, "POST /system:shutdown"},                // 关机（S）
	},
	// #/system/kernel 内核基线
	"page-kernel": {
		"knl-edit-btn":     {uiClassHide, "PUT /configuration/candidate"}, // 编辑内核启动参数（S；系统族第三刀）
		"knl-apply-btn":    {uiClassHide, "POST /system/kernel:apply"},    // 写入基线（Su）
		"knl-rollback-btn": {uiClassHide, "POST /system/kernel:rollback"}, // 回退上一次基线（Su）
	},
	// #/system/pools 资源池（决策 #329：大页池回收；系统族第三刀：资源池声明就地编辑）
	"page-pools": {
		"pool-edit-btn":  {uiClassHide, "PUT /configuration/candidate"},   // 编辑资源池声明（S）
		"hp-reclaim-btn": {uiClassHide, "POST /system/hugepages:reclaim"}, // 回收空闲的多余大页（S）
	},
	// #/system/interfaces/:name 接口详情（决策 #389：端口安全白名单的写入口；
	// DPDK/SR-IOV 两条既有写入口一并在册——同页同机制，漏标会让非 super 角色看到；
	// 系统族第三刀：接口配置 + 数据面单网卡覆盖的就地编辑）
	"page-ifaceDetail": {
		"ifd-edit-btn":    {uiClassHide, "PUT /configuration/candidate"}, // 编辑接口配置（S）
		"ifd-uio":         {uiClassHide, "PUT /interfaces/{name}/dpdk"},  // DPDK 驱动选择（Su）
		"ifd-dpdk-bind":   {uiClassHide, "PUT /interfaces/{name}/dpdk"},  // 绑定到 DPDK（Su）
		"ifd-todrv":       {uiClassHide, "PUT /interfaces/{name}/dpdk"},  // 解绑交还的内核驱动名（Su）
		"ifd-dpdk-unbind": {uiClassHide, "PUT /interfaces/{name}/dpdk"},  // 解绑交还内核（Su）
		"ifd-vfs":         {uiClassHide, "PUT /interfaces/{name}/sriov"}, // SR-IOV VF 数量（Su）
		"ifd-sriov-btn":   {uiClassHide, "PUT /interfaces/{name}/sriov"}, // 设置 VF 数量（Su）
		"ifd-ps-mac":      {uiClassHide, "PUT /configuration/candidate"}, // 端口安全白名单 MAC（走候选事务，S）
		"ifd-ps-add-btn":  {uiClassHide, "PUT /configuration/candidate"}, // 添加端口安全 MAC（走候选事务，S）
	},
	// #/system 系统域一级页（系统族第三刀）：系统配置面板（六节合一面板）+ 主机防火墙面板。
	// 两处都把候选事务（PUT /configuration/candidate，S → hide）作为写路径；面板内的具体字段控件
	// 由 renderSection 动态生成（经 wbtn 打 data-write），不在静态清单里。
	"page-system": {
		"sysp-edit-btn": {uiClassHide, "PUT /configuration/candidate"}, // 编辑系统配置（S）
		"fw-edit-btn":   {uiClassHide, "PUT /configuration/candidate"}, // 编辑防火墙配置（S）
	},
	// #/system/users 用户与权限（系统族第三刀：登录与口令策略就地编辑）。
	// 本页的「修改我的口令」三条（usr-my-old / usr-my-new / usr-mypw-btn）**有意**不登记端点：
	// 它们走 POST /system/login-users/{tail...}——服务端注册为 ReadOnly（本人 + 验旧口令，按身份
	// 授权而不按 class），界面标 data-write 是因为本页对只读角色本就不列（决策 #145），改口令的
	// 只读路径是顶栏「我的账号」。端点 class 与界面标记不同源，故与既有客户端动作同口径留空
	// （空 = 不参与端点 class 交叉核对）。
	"page-users": {
		"usr-edit-btn":         {uiClassHide, "PUT /configuration/candidate"}, // 编辑登录与口令策略（S）
		"usr-banner-text":      {uiClassHide, "PUT /system/login-banner"},     // 登录横幅文本（S）
		"usr-banner-save-btn":  {uiClassHide, "PUT /system/login-banner"},     // 保存横幅（S）
		"usr-banner-clear-btn": {uiClassHide, "DELETE /system/login-banner"},  // 清除横幅（S）
		"usr-new-name":         {uiClassHide, "POST /system/login-users"},     // 新用户名（S）
		"usr-new-class":        {uiClassHide, "POST /system/login-users"},     // 新用户 class（S）
		"usr-new-pw":           {uiClassHide, "POST /system/login-users"},     // 新用户初始口令（S）
		"usr-create-btn":       {uiClassHide, "POST /system/login-users"},     // 创建用户（S）
		"usr-my-old":           {uiClassHide, ""},                             // 我的口令：自助端点（见上）
		"usr-my-new":           {uiClassHide, ""},                             // 同上
		"usr-mypw-btn":         {uiClassHide, ""},                             // 同上
	},
	// #/system/tls 证书
	"page-tls": {
		"tls-cert":        {uiClassHide, "PUT /system/tls"},             // 证书正文（S）
		"tls-key":         {uiClassHide, "PUT /system/tls"},             // 私钥正文（S）
		"tls-install-btn": {uiClassHide, "PUT /system/tls"},             // 安装并立即生效（S）
		"tls-regen-btn":   {uiClassHide, "POST /system/tls:regenerate"}, // 重签自签证书（S）
	},
	// #/ops/capture 抓包
	"page-capture": {
		"cap-iface":      {uiClassHide, "POST /capture"},   // 抓包接口（S）
		"cap-count":      {uiClassHide, "POST /capture"},   // 报文数（S）
		"cap-start-btn":  {uiClassHide, "POST /capture"},   // 开始抓包（S）
		"cap-stop-btn":   {uiClassHide, "DELETE /capture"}, // 停止（保留数据；决策 #465）（S）
		"cap-export-btn": {uiClassHide, "DELETE /capture"}, // 停止并导出 pcap（S）
	},
	// #/ops/diagnostics 诊断
	"page-diagnostics": {
		"diag-host":      {uiClassOp, "POST /diagnostics/ping"},              // ping/traceroute 目标（O）
		"diag-count":     {uiClassOp, "POST /diagnostics/ping"},              // ping 次数（O）
		"diag-source":    {uiClassOp, "POST /diagnostics/ping"},              // ping 源地址（O）
		"diag-vrf":       {uiClassOp, "POST /diagnostics/ping"},              // ping VRF（O，#402）
		"diag-ipv6":      {uiClassOp, "POST /diagnostics/ping"},              // IPv6 选择（O，#330）
		"diag-ping-btn":  {uiClassOp, "POST /diagnostics/ping"},              // 执行 ping（O）
		"diag-trace-btn": {uiClassOp, "POST /diagnostics/traceroute"},        // 执行 traceroute（O）
		"diag-clear-if":  {uiClassHide, "POST /interfaces:clear-statistics"}, // 清零接口（Su）
		"diag-clear-btn": {uiClassHide, "POST /interfaces:clear-statistics"}, // 清零统计（Su）
		"diag-log-btn":   {uiClassRead, "GET /system/logs"},                  // 刷新服务端日志（读）
	},

	// —— 对象页家族（配置页去中心化第一刀）：就地「+ 新建 / 编辑 / 删除」的写入口 ——
	//
	// 写入路径统一走 candidate 事务（PUT /configuration/candidate，S 级 → hide）；QoS / SPAN 的
	// 删除「改候选」一步走该对象自己的端点（DELETE /qos/policies|port-mirroring/{name}，同为
	// cfgAPI = S 级）后再统一 校验 → 提交 → 释放。列表里的行内写入口（VXLAN 的编辑/删除、各块
	// 右上角的「+ 新建」）是**动态生成**的、不在清单里——由 TestUIObjectPageDynamicWriteEntriesUseWbtn
	// 钉住它们必须经 wbtn；只读入口（大表拉取 / MAC 表 / 路由表刷新）照旧不带 data-write。
	"page-network": {
		"big-nat-btn":   {uiClassRead, ""}, // 查看 NAT 会话（读）
		"big-clear-btn": {uiClassRead, ""}, // 清空大表输出（读）
	},
	"page-switches": {
		"vs-new-btn": {uiClassHide, "PUT /configuration/candidate"}, // + 新建虚拟交换机（S）
	},
	"page-switchDetail": {
		"vsd-edit-btn":       {uiClassHide, "PUT /configuration/candidate"}, // 编辑该交换机（S）
		"vsd-del-btn":        {uiClassHide, "PUT /configuration/candidate"}, // 删除该交换机（S）
		"vsd-mac-limit":      {uiClassRead, ""},                             // MAC 表条数（读）
		"vsd-mac-btn":        {uiClassRead, ""},                             // 拉取 MAC 表（读）
		"vsd-mac-clear-btn":  {uiClassRead, ""},                             // 清空 MAC 表（读）
		"vsd-dhcp-btn":       {uiClassRead, ""},                             // 拉取租约（读）
		"vsd-dhcp-clear-btn": {uiClassRead, ""},                             // 清空租约表（读）
	},
	"page-vrfDetail": {
		"vrd-edit-btn":   {uiClassHide, "PUT /configuration/candidate"}, // 编辑该 VRF（S）
		"vrd-del-btn":    {uiClassHide, "PUT /configuration/candidate"}, // 删除该 VRF（S）
		"vrd-routes-btn": {uiClassRead, ""},                             // 刷新路由表（读）
	},
	"page-aclDetail": {
		"acd-edit-btn": {uiClassHide, "PUT /configuration/candidate"}, // 编辑该 ACL（S）
		"acd-del-btn":  {uiClassHide, "PUT /configuration/candidate"}, // 删除该 ACL（S）
	},
	"page-bondDetail": {
		"bnd-edit-btn": {uiClassHide, "PUT /configuration/candidate"}, // 编辑该 bond（S）
		"bnd-del-btn":  {uiClassHide, "PUT /configuration/candidate"}, // 删除该 bond（S）
	},
	"page-qosDetail": {
		"qsd-edit-btn": {uiClassHide, "PUT /configuration/candidate"}, // 编辑该策略（S）
		"qsd-del-btn":  {uiClassHide, "DELETE /qos/policies/{name}"},  // 删除该策略（S；改候选走该端点）
	},
	"page-spanDetail": {
		"spd-edit-btn": {uiClassHide, "PUT /configuration/candidate"},  // 编辑该会话（S）
		"spd-del-btn":  {uiClassHide, "DELETE /port-mirroring/{name}"}, // 删除该会话（S；改候选走该端点）
	},

	// —— 对象页家族（配置页去中心化第二刀·计算族）：VM / 容器 / 镜像的就地增删改 ——
	//
	// 与网络族同一套机制（写路径是 candidate 事务 = PUT /configuration/candidate，S 级 → hide；
	// 删除沿用它自己的对象端点，同为 cfgAPI = S 级）。列表行内与 objEditor 面板里**动态生成**的
	// 写入口（+ 新建 / 编辑 / 画像）由 TestUIObjectPageInPlaceEditors 与
	// TestUIDynamicWriteEntriesUseWbtn 钉住；这里登记静态骨架上的控件。
	"page-vms": {
		"vm-new-btn": {uiClassHide, "PUT /configuration/candidate"}, // + 新建虚拟机（S）
	},
	"page-vmDetail": {
		"vmd-edit-btn":       {uiClassHide, "PUT /configuration/candidate"},                     // 编辑该 VM（S）
		"vmd-delete-btn":     {uiClassHide, "DELETE /virtual-machine-functions/{name}"},         // 删除该 VM（S；confirm=true 与一次性事务由 TestUIDeleteEntriesGated 钉住）
		"vm-snap-new":        {uiClassHide, "POST /virtual-machine-functions/{name}/snapshots"}, // 新快照名（S）
		"vm-snap-create":     {uiClassHide, "POST /virtual-machine-functions/{name}/snapshots"}, // 创建快照（S）
		"vm-snap-refresh":    {uiClassRead, ""},                                                 // 刷新快照列表（读）
		"vm-console-connect": {uiClassHide, "POST /virtual-machine-functions/{name}/console"},   // 连接控制台（S）
		"vm-console-close":   {uiClassHide, ""},                                                 // 断开（客户端动作）
		"vm-console-in":      {uiClassHide, ""},                                                 // 串口输入（WS 管道，ticket 鉴权）
		"vm-console-send":    {uiClassHide, ""},                                                 // 发送（同上）
		"vm-console-enter":   {uiClassHide, ""},                                                 // 只发回车（同上）
	},
	"page-containers": {}, // 列表页没有静态控件（行内「编辑 / 删除」是动态生成的写入口）
	"page-containerDetail": {
		"ctd-edit-btn":     {uiClassHide, "PUT /configuration/candidate"},           // 编辑该容器（S）
		"ctd-delete-btn":   {uiClassHide, "DELETE /container-functions/{name}"},     // 删除该容器（S；同 VM）
		"ct-logs-refresh":  {uiClassRead, ""},                                       // 刷新容器日志（读）
		"ct-exec-cmd":      {uiClassHide, "POST /container-functions/{tail...}"},    // 执行命令（S，dispatch 路由）
		"ct-exec-timeout":  {uiClassHide, "POST /container-functions/{tail...}"},    // 命令超时（S）
		"ct-exec-run":      {uiClassHide, "POST /container-functions/{tail...}"},    // 执行（S）
		"ct-shell-connect": {uiClassHide, "POST /container-functions/{name}/shell"}, // 交互终端（S）
		"ct-shell-close":   {uiClassHide, ""},                                       // 断开（客户端动作）
		"ct-shell-in":      {uiClassHide, ""},                                       // 终端输入（WS 管道，ticket 鉴权）
		"ct-shell-send":    {uiClassHide, ""},                                       // 发送（同上）
		"ct-shell-enter":   {uiClassHide, ""},                                       // 只发回车（同上）
	},
	"page-images": {
		"img-name":         {uiClassHide, "POST /images"}, // 导入镜像的公共字段与三种方式（S）
		"img-type":         {uiClassHide, "POST /images"},
		"img-url":          {uiClassHide, "POST /images"},
		"img-sha":          {uiClassHide, "POST /images"},
		"img-url-btn":      {uiClassHide, "POST /images"},
		"img-incoming":     {uiClassHide, "POST /images"},
		"img-incoming-btn": {uiClassHide, "POST /images"},
		"img-file":         {uiClassHide, "POST /images"},
		"img-file-btn":     {uiClassHide, "POST /images"},
	},
	"page-imageDetail": {
		"imd-profile-btn": {uiClassHide, "PUT /configuration/candidate"}, // 编辑启动画像（S；画像按镜像名 upsert）
		"imd-del-btn":     {uiClassHide, "DELETE /images/{name}"},        // 删除该镜像（S，与列表行内删除同一动作）
	},
}

func TestUIOpsFamilyWriteControlClasses(t *testing.T) {
	html := readUITestAsset(t, "index.html")
	serverSrc := readAPISourceForTest(t, "server.go")
	serverClass := uiServerEndpointClasses(t, serverSrc)

	sections := map[string]string{}
	for _, m := range uiPageSectionRe.FindAllStringSubmatch(html, -1) {
		sections[m[1]] = m[2]
	}

	for page, want := range uiOpsFamilyControls {
		body, ok := sections[page]
		if !ok {
			t.Errorf("index.html 里找不到页面 section #%s（页面被删/改名？）", page)
			continue
		}
		got := uiSectionControls(t, page, body)
		// 逐条核对：漏标（清单有、页面无）、多标（页面有、清单无）、class 不符。
		for id, w := range want {
			g, ok := got[id]
			if !ok {
				t.Errorf("[%s] 清单里的控件 #%s 在页面里找不到（被删/改名？）", page, id)
				continue
			}
			if g != w.class {
				t.Errorf("[%s] #%s 的 class = %q，期望 %q（判据：端点 %s 的服务端 class）",
					page, id, g, w.class, w.ep)
			}
		}
		for id := range got {
			if _, ok := want[id]; !ok {
				t.Errorf("[%s] 页面里的控件 #%s 不在清单里——新增写控件必须登记它的 class 期望（%s/%s/%s），否则门禁可能漏标",
					page, id, uiClassHide, uiClassOp, uiClassRead)
			}
		}
	}

	// 与服务端 class 交叉核对：界面 class 必须与控件触发端点的服务端声明一致。
	wantByEndpoint := map[string]string{ // 服务端 class → 界面期望 class
		"S": uiClassHide, "O": uiClassOp, "R": uiClassRead,
	}
	for page, controls := range uiOpsFamilyControls {
		for id, w := range controls {
			if w.ep == "" {
				continue
			}
			sc, ok := serverClass[w.ep]
			if !ok {
				t.Errorf("[%s] #%s 声明的端点 %q 在 server.go 里查不到注册（端点改名？清单过期？）", page, id, w.ep)
				continue
			}
			if want := wantByEndpoint[sc]; want != w.class {
				t.Errorf("[%s] #%s：端点 %s 服务端 class=%s → 界面应为 %q，清单却写 %q（界面与服务端权限口径不一致）",
					page, id, w.ep, sc, want, w.class)
			}
		}
	}
}

// TestUIDynamicWriteEntriesUseWbtn：**动态生成**的写入口必须经 `wbtn` 打 `data-write`。
//
// 这是 R105-1 的实际漏点所在：静态入口一律在 index.html 上打标（上面那条用例覆盖），
// 而动态入口只能靠代码里用 `wbtn` 而不是 `el('button')`。本轮排查在整份 app.js 里找到
// 唯一一条"写动作却用 el('button') 生成"的：镜像列表的「删除」（DELETE /images/{name}，
// super-user 级）——read-only/operator 都能看到并按下去得 403，违反 #324/#141。已改为 wbtn。
func TestUIDynamicWriteEntriesUseWbtn(t *testing.T) {
	app := jsAll(t)

	// 镜像列表的「删除」：必须经 wbtn（带 data-write）。
	if !strings.Contains(app, "rowButton(wbtn({ type: 'button', class: 'danger small', text: '删除' }))") {
		t.Error("镜像列表的「删除」写入口没有经 wbtn 标记 data-write（read-only/operator 会看到它，决策 #327）")
	}
	// 反证：不许再出现"用 el('button') 生成的删除动作"（写入口漏打门禁的形态）。
	if strings.Contains(app, "rowButton(el('button', { type: 'button', class: 'danger small', text: '删除' }))") {
		t.Error("app.js 里仍有经 el('button') 生成的「删除」写入口——写入口必须用 wbtn（决策 #327）")
	}
}

// TestUIDeleteEntriesGated：VM / 容器 / core dump 三处删除入口的写门禁守护。
//
// 判据与服务端一致：三条端点在 server.go 里都经 cfgAPI 注册（= super-user 级），故界面入口
// 必须带 data-write 而**不带** data-op——漏标 data-write 会让 read-only/operator 都看得见，
// 误标 data-op 则把 operator 放进来点下去得 403（决策 #327 的两种漏法都钉住）。
// 端点→class 由 server.go 现读（不是人工声明）：端点改名/改 class 会让本用例先报"判据变了"。
func TestUIDeleteEntriesGated(t *testing.T) {
	html := readUITestAsset(t, "index.html")
	serverClass := uiServerEndpointClasses(t, readAPISourceForTest(t, "server.go"))

	for _, ep := range []string{
		"DELETE /virtual-machine-functions/{name}",
		"DELETE /container-functions/{name}",
		"DELETE /system/core-dumps",
	} {
		if got := serverClass[ep]; got != "S" {
			t.Fatalf("端点 %s 的服务端 class = %q（期望 S）——判据变了，请复核这三处删除入口的门禁", ep, got)
		}
	}

	// 静态入口（VM / 容器详情页的删除按钮）：带 data-write、不带 data-op。
	for _, id := range []string{"vmd-delete-btn", "ctd-delete-btn"} {
		tag := uiTagByID(html, id)
		if tag == "" {
			t.Errorf("index.html 里找不到 #%s（删除入口被删/改名？）", id)
			continue
		}
		if !strings.Contains(tag, "data-write") {
			t.Errorf("#%s 缺少 data-write（read-only/operator 会看到删除入口，决策 #327）：%s", id, tag)
		}
		if strings.Contains(tag, "data-op") {
			t.Errorf("#%s 被标了 data-op，但端点服务端 class 是 S（operator 点了只会得 403）：%s", id, tag)
		}
	}

	// 动态入口（VM / 容器列表行、core dump 行）：必须经 wbtn（带 data-write）。
	js := jsAll(t)
	for _, want := range []string{
		"const delBtn = rowButton(wbtn({ type: 'button', class: 'danger small', text: '删除' }));",
		"const btn = wbtn({ type: 'button', class: 'danger small', text: '删除' });",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("前端缺少经 wbtn 生成的删除入口（写入口必须带 data-write，决策 #327）：%q", want)
		}
	}

	// 服务端要求二次确认（confirm=true，见 vm.go/container.go 的 handleDeleteVM/Container）——
	// 界面必须带上它，否则删除会被 400 CONFIRM_REQUIRED 挡下（与 DPDK 绑定同款纪律：
	// 界面不得绕过服务端的显式要求）。
	for _, want := range []string{
		"'/virtual-machine-functions/' + encodeURIComponent(name) + '?confirm=true'",
		"'/container-functions/' + encodeURIComponent(name) + '?confirm=true'",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("删除调用缺少 confirm=true（服务端会返回 400 CONFIRM_REQUIRED）：%q", want)
		}
	}

	// 一次性事务（X-NFVIS-Auto-Commit: true）：删除必须**删完即生效**（与 CLI 的 commitMutate
	// 同口径）。漏了它 mutateCandidate 只写候选、不提交——对象仍在列表里，等于假删除。
	if n := strings.Count(js, "'X-NFVIS-Auto-Commit': 'true'"); n < 2 {
		t.Errorf("VM/容器删除缺少 X-NFVIS-Auto-Commit: true（只写候选不提交 ⇒ 对象删不掉）：出现 %d 处，期望 ≥2", n)
	}
}

// TestUIObjectPageInPlaceEditors：配置页去中心化（对象页就地增删改）的守护——第一刀（网络族）
// 与第二刀（计算族：VM / 容器 / 镜像）同一套机制，清单一起钉。
//
// 判据（机器提取，不靠人工表自洽）：
//
//	① index.html 里各面板落点（objEditor 画的节与结果提示都落在这些容器里）都在；
//	② js/views/ 各视图里 objEditor({slot:'…'}) 的 slot 与落点逐一对应——改名 / 漏接线即红
//	   （那正是「面板画到别处去了」的静默形态）；
//	③ 对象页的**动态写入口**必须经 wbtn（data-write）：VXLAN 行内「编辑 / 删除」与各块右上角
//	   「+ 新建 / 编辑」（与 #327 同口径：经 el('button') 生成的写入口会让非 super 角色看见并 403）。
func TestUIObjectPageInPlaceEditors(t *testing.T) {
	html := readUITestAsset(t, "index.html")
	slots := []string{
		"net-obj-edit", "vs-list-edit", "vsd-edit",
		"vrd-edit", "acd-edit", "bnd-edit", "qsd-edit", "spd-edit",
		// 第二刀·计算族
		"vm-list-edit", "vmd-edit", "ct-list-edit", "ctd-edit", "img-list-edit", "imd-edit",
		// 第三刀·系统族（system.js / ops.js）
		"sysp-edit", "fw-edit", "knl-edit", "usr-edit", "ifd-edit", "pool-edit", "ops-vpp-edit",
	}
	for _, id := range slots {
		if uiTagByID(html, id) == "" {
			t.Errorf("index.html 里找不到面板落点 #%s（对象页的就地编辑面板无处可落）", id)
		}
	}
	js := uiJSFile(t, "ui/js/views/network.js") + "\n" + uiJSFile(t, "ui/js/views/switches.js") +
		"\n" + uiJSFile(t, "ui/js/views/vms.js") + "\n" + uiJSFile(t, "ui/js/views/containers.js") +
		"\n" + uiJSFile(t, "ui/js/views/images.js") +
		"\n" + uiJSFile(t, "ui/js/views/system.js") + "\n" + uiJSFile(t, "ui/js/views/ops.js")
	re := regexp.MustCompile(`objEditor\(\{\s*slot:\s*'([^']+)'`)
	got := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(js, -1) {
		got[m[1]] = true
	}
	if len(got) != len(slots) {
		t.Errorf("视图里 objEditor 的 slot 有 %d 个（%v），期望 %d 个与 index.html 的落点逐一对应",
			len(got), got, len(slots))
	}
	for _, id := range slots {
		if !got[id] {
			t.Errorf("index.html 有落点 #%s，但没有任何视图把它接给 objEditor（面板画不到那一页）", id)
		}
	}
	// 动态写入口（不经 index.html，故只能钉代码形态）。
	for _, want := range []string{
		"rowButton(wbtn({ type: 'button', class: 'small', text: isSingle ? '编辑' : '+ 新建' }))",
		"rowButton(wbtn({ type: 'button', class: 'small', text: '编辑' }))",
		"rowButton(wbtn({ type: 'button', class: 'small danger', text: '删除' }))",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("对象页缺少经 wbtn 生成的动态写入口（写入口必须带 data-write，决策 #327）：%q", want)
		}
	}
	// 计算族列表的行内写入口（容器「编辑」、镜像「画像」）也必须经 wbtn（同上口径）。
	for _, want := range []string{
		"rowButton(wbtn({ type: 'button', class: 'small', text: '编辑' }))", // 容器列表行内「编辑」
		"rowButton(wbtn({ type: 'button', class: 'small', text: '画像' }))", // 镜像列表行内「画像」
	} {
		if !strings.Contains(js, want) {
			t.Errorf("计算族列表页缺少经 wbtn 生成的动态写入口：%q", want)
		}
	}
	// VM / 容器详情页的「编辑」按钮是静态骨架（data-write，见上面 uiOpsFamilyControls 的
	// page-vmDetail / page-containerDetail 清单），这里只钉它们的接线存在。
	for _, want := range []string{"$('vmd-edit-btn')", "$('ctd-edit-btn')"} {
		if !strings.Contains(js, want) {
			t.Errorf("详情页「编辑」按钮没有接线（点了没反应）：%q", want)
		}
	}
}

// uiTagByID 取 index.html 里某个 id 所在的整个标签（`<… id="x" …>`）。
func uiTagByID(html, id string) string {
	re := regexp.MustCompile(`<[^>]*\bid="` + regexp.QuoteMeta(id) + `"[^>]*>`)
	return re.FindString(html)
}

// readAPISourceForTest 读 internal/api 下**本包源码**（与读 ui/ 资源的 readUITestAsset 区分开）。
func readAPISourceForTest(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("读取 %s: %v", name, err)
	}
	return string(b)
}

// uiSectionControls 收集某页里的可交互控件 → 期望 class（hide/op/read）。
func uiSectionControls(t *testing.T, page, body string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, tag := range uiControlTagRe.FindAllString(body, -1) {
		m := uiIDInTagRe.FindStringSubmatch(tag)
		if m == nil {
			continue // 无 id 的控件不参与清单（页面里没有这类需要门禁的控件）
		}
		id := m[1]
		hasWrite := strings.Contains(tag, "data-write")
		hasOp := strings.Contains(tag, "data-op")
		var cls string
		switch {
		case hasWrite && hasOp:
			cls = uiClassOp
		case hasWrite:
			cls = uiClassHide
		default:
			cls = uiClassRead
		}
		// 同一 id 重复出现时保留更强的写标记（页面里不该重复；真重复由"清单外多余 id"暴露不了，
		// 但这里保守取并集，避免把写入口误判成读）。
		if old, ok := out[id]; ok && old != uiClassRead && cls == uiClassRead {
			continue
		}
		out[id] = cls
	}
	return out
}

// uiServerEndpointClasses 从 server.go 源码解析"METHOD /path → 服务端 class"（S/O/R）。
// cfgAPI 是 super-user 包装器（`s.auth(h, schema.ClassSuperUser, "configure")`），按 S 计。
func uiServerEndpointClasses(t *testing.T, src string) map[string]string {
	t.Helper()
	flat := strings.Join(strings.Fields(src), " ")
	out := map[string]string{}
	for _, m := range uiServerRouteRe.FindAllStringSubmatch(flat, -1) {
		method, path, args := m[1], m[2], m[3]
		var cls string
		switch {
		case strings.Contains(args, "cfgAPI("):
			cls = "S"
		case strings.Contains(args, "ClassSuperUser"):
			cls = "S"
		case strings.Contains(args, "ClassOperator"):
			cls = "O"
		case strings.Contains(args, "ClassReadOnly"):
			cls = "R"
		default:
			cls = "?" // 未知（未分类）——由交叉核对时报"界面与服务端口径不一致"暴露
		}
		out[method+" "+path] = cls
	}
	if len(out) < 100 {
		t.Fatalf("从 server.go 只解析出 %d 条路由（预期上百条）——解析正则可能已失效，别让交叉核对变成空转", len(out))
	}
	return out
}
