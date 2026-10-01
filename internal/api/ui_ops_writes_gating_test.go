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
//      #/ops/diagnostics）里每个可交互控件**逐条**列进清单，标注期望 class；
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
		"knl-apply-btn":    {uiClassHide, "POST /system/kernel:apply"},    // 写入基线（Su）
		"knl-rollback-btn": {uiClassHide, "POST /system/kernel:rollback"}, // 回退上一次基线（Su）
	},
	// #/system/pools 资源池（决策 #329：大页池回收）
	"page-pools": {
		"hp-reclaim-btn": {uiClassHide, "POST /system/hugepages:reclaim"}, // 回收空闲的多余大页（S）
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
		"cap-iface":      {uiClassHide, "POST /vpp/capture"},   // 抓包接口（S）
		"cap-count":      {uiClassHide, "POST /vpp/capture"},   // 缓冲深度（S）
		"cap-start-btn":  {uiClassHide, "POST /vpp/capture"},   // 开始抓包（S）
		"cap-stop-btn":   {uiClassHide, "DELETE /vpp/capture"}, // 停止（丢弃）（S）
		"cap-export-btn": {uiClassHide, "DELETE /vpp/capture"}, // 停止并导出 pcap（S）
	},
	// #/ops/diagnostics 诊断
	"page-diagnostics": {
		"diag-host":      {uiClassOp, "POST /diagnostics/ping"},              // ping/traceroute 目标（O）
		"diag-count":     {uiClassOp, "POST /diagnostics/ping"},              // ping 次数（O）
		"diag-source":    {uiClassOp, "POST /diagnostics/ping"},              // ping 源地址（O）
		"diag-ipv6":      {uiClassOp, "POST /diagnostics/ping"},              // IPv6 选择（O，#330）
		"diag-ping-btn":  {uiClassOp, "POST /diagnostics/ping"},              // 执行 ping（O）
		"diag-trace-btn": {uiClassOp, "POST /diagnostics/traceroute"},        // 执行 traceroute（O）
		"diag-clear-if":  {uiClassHide, "POST /interfaces:clear-statistics"}, // 清零接口（Su）
		"diag-clear-btn": {uiClassHide, "POST /interfaces:clear-statistics"}, // 清零统计（Su）
		"diag-log-btn":   {uiClassRead, "GET /system/logs"},                  // 刷新服务端日志（读）
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
	app := readUITestAsset(t, "app.js")

	// 镜像列表的「删除」：必须经 wbtn（带 data-write）。
	if !strings.Contains(app, "rowButton(wbtn({ type: 'button', class: 'danger small', text: '删除' }))") {
		t.Error("镜像列表的「删除」写入口没有经 wbtn 标记 data-write（read-only/operator 会看到它，决策 #327）")
	}
	// 反证：不许再出现"用 el('button') 生成的删除动作"（写入口漏打门禁的形态）。
	if strings.Contains(app, "rowButton(el('button', { type: 'button', class: 'danger small', text: '删除' }))") {
		t.Error("app.js 里仍有经 el('button') 生成的「删除」写入口——写入口必须用 wbtn（决策 #327）")
	}
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
