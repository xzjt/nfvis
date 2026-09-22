package api

// cli_rest_coverage_test.go —— CLI 命令 ⇄ REST 端点覆盖守护（round42 核查的固化）。
//
// 由来：用户问"Web 控制台能否实现 CLI 的全部功能"。核查（docs/CLI-REST覆盖核查.md）的
// 结论是 258 个命令形态中 225 个已有类型化端点、14 个是真缺口、19 个是 CLI-only by design。
// 本测试把该核查变成机器可维护的三张表，使两类漂移自动现形：
//   ① 映射过期——覆盖表声明的端点被改名/删除（routes_contract 只判"路由 ⊆ 契约"的反方向，
//      判不出"CLI 还在指向一个已不存在的端点"）；
//   ② 新命令漏归类——新增契约命令必须落在三张表之一（否则 Web 控制台的功能覆盖悄悄出现空洞）。
//
// 纪律（与 contractCLICommands/deferred 同一模式）：
//   - 新增 CLI 命令须同步补：命令全表、contractCLICommands、以及本文件三张表之一；
//   - `/cli/execute` 与 `/cli/candidates` 是 x-internal（契约明言"仅 nfvis-cli 使用"），
//     按 round37 口径**不计入**覆盖——前端不碰它；
//   - 完整 258 形态的策展清单以 docs/NFViS-CLI命令全表.md 为准，这里的是机器可维护子集；
//   - 表 key 是**形态模式**：`<...>` 匹配任意单个 token（具体实例名如 ens224/vm1 因此
//     天然匹配 `<ifname>`/`<n>`），`[...]` 可选组已在匹配时剥除。

import (
	"regexp"
	"strings"
	"testing"
)

// optGroupRe 剥除 key 里的 [可选] 组（如 "[last <n>]"、"[id <id> | all]"）。
var optGroupRe = regexp.MustCompile(`\[[^\]]*\]`)

// cliFormMatch 形态匹配：key 剥除可选组后与 command 逐 token 比对，`<...>` 通配任意
// 单 token。返回 key 的有效 token 数（0 = 不匹配），供最长匹配消歧。
func cliFormMatch(command, key string) int {
	kt := strings.Fields(optGroupRe.ReplaceAllString(key, " "))
	ct := strings.Fields(command)
	if len(ct) != len(kt) {
		return 0
	}
	for i, k := range kt {
		if strings.HasPrefix(k, "<") && strings.HasSuffix(k, ">") {
			continue
		}
		if ct[i] != k {
			return 0
		}
	}
	return len(kt)
}

// cliRESTCoverage 命令形态 → 承载它的类型化 REST 端点（"METHOD /path"，多个用 " + " 连接）。
// 配置模式的 set/delete 语句不走本表——由 candidate API 架构性覆盖（见 classify）。
var cliRESTCoverage = map[string]string{
	// ---- show 族 ----
	"show version":                                     "GET /system/version",
	"show system uptime":                               "GET /system/status",
	"show system cpu":                                  "GET /system/status",
	"show system memory":                               "GET /system/status",
	"show system storage":                              "GET /system/status",
	"show system hugepages":                            "GET /system/status + GET /resource-pools",
	"show system kernel":                               "GET /system/kernel",
	"show system hardware":                             "GET /system/hardware",
	"show system core-dumps":                           "GET /system/core-dumps",
	"show system tech-support":                         "GET /system/tech-support",
	"show tech-support":                                "GET /system/tech-support",
	"show system configuration sessions":               "GET /system/configuration/sessions",
	"show configuration sessions":                      "GET /system/configuration/sessions",
	"show interfaces":                                  "GET /interfaces",
	"show interfaces physical":                         "GET /interfaces",
	"show interfaces management":                       "GET /interfaces",
	"show interfaces <ifname>":                         "GET /interfaces/{name}",
	"show interfaces physical <ifname>":                "GET /interfaces/{name}",
	"show interfaces <ifname> detail":                  "GET /interfaces/{name}",
	"show interfaces <ifname> statistics":              "GET /interfaces/{name}",
	"show interfaces physical <ifname> detail":         "GET /interfaces/{name}",
	"show interfaces physical <ifname> statistics":     "GET /interfaces/{name}",
	"show interfaces <ifname> sriov":                   "GET /interfaces/{name}",
	"show interfaces physical <ifname> sriov":          "GET /interfaces/{name}",
	"show virtual-switches":                            "GET /virtual-switches",
	"show virtual-switches <name>":                     "GET /virtual-switches/{name}",
	"show virtual-switches <name> detail":              "GET /virtual-switches/{name}",
	"show virtual-switches <name> ports":               "GET /virtual-switches/{name}/ports",
	"show virtual-switches <name> mac-table":           "GET /virtual-switches/{name}/mac-table",
	"show vrfs":                                        "GET /vrfs",
	"show vrfs <name>":                                 "GET /vrfs/{name}",
	"show vrfs <name> routes":                          "GET /vrfs/{name}/routes",
	"show acls":                                        "GET /acls",
	"show acls <name>":                                 "GET /acls/{name}",
	"show acls <name> detail":                          "GET /acls/{name}",
	"show nat":                                         "GET /nat + GET /nat/sessions",
	"show port-mirroring":                              "GET /port-mirroring",
	"show qos policies":                                "GET /qos/policies",
	"show vpp":                                         "GET /vpp/status",
	"show vpp threads":                                 "GET /vpp/status",
	"show vpp buffers":                                 "GET /vpp/status",
	"show vpp memory":                                  "GET /vpp/status",
	"show vpp capture":                                 "GET /vpp/capture + GET /vpp/capture/{file}",
	"show bonds":                                       "GET /bonds",
	"show bonds <name> detail":                         "GET /bonds/{name}",
	"show lldp neighbors":                              "GET /protocols/lldp/neighbors",
	"show protocols lldp neighbors":                    "GET /protocols/lldp/neighbors",
	"show virtual-machine-functions":                   "GET /virtual-machine-functions",
	"show virtual-machine-functions <name> detail":     "GET /virtual-machine-functions/{name}",
	"show virtual-machine-functions <name> interfaces": "GET /virtual-machine-functions/{name}",
	"show virtual-machine-functions <name> snapshots":  "GET /virtual-machine-functions/{name}/snapshots",
	"show container-functions":                         "GET /container-functions",
	"show container-functions <name>":                  "GET /container-functions/{name}",
	"show container-functions <name> interfaces":       "GET /container-functions/{name}",
	"show images":                                      "GET /images",
	"show images <name> detail":                        "GET /images/{name}",
	"show resource-pools":                              "GET /resource-pools",
	"show alarms":                                      "GET /alarms",
	"show log audit":                                   "GET /audit-logs",
	"show users":                                       "GET /system/login-users",
	"show configuration candidate":                     "GET /configuration/candidate",
	"show configuration":                               "GET /configuration",
	"show configuration compare rollback <n>":          "GET /configuration/diff + POST /configuration/rollback/{n}",
	// ---- request 族 ----
	"request virtual-machine-functions <n> start":             "POST /virtual-machine-functions/{name}:start",
	"request virtual-machine-functions <n> stop":              "POST /virtual-machine-functions/{name}:stop",
	"request virtual-machine-functions <n> restart":           "POST /virtual-machine-functions/{name}:restart",
	"request virtual-machine-functions <n> delete":            "DELETE /virtual-machine-functions/{name}",
	"request virtual-machine-functions <n> console":           "POST /virtual-machine-functions/{name}/console + GET /virtual-machine-functions/{name}/console/ws",
	"request virtual-machine-functions <n> snapshot create":   "POST /virtual-machine-functions/{name}/snapshots",
	"request virtual-machine-functions <n> snapshot rollback": "POST /virtual-machine-functions/{name}/snapshots/{snapshot}:rollback",
	"request virtual-machine-functions <n> snapshot delete":   "DELETE /virtual-machine-functions/{name}/snapshots/{snapshot}",
	"request container-functions <n> start":                   "POST /container-functions/{name}:start",
	"request container-functions <n> stop":                    "POST /container-functions/{name}:stop",
	"request container-functions <n> restart":                 "POST /container-functions/{name}:restart",
	"request container-functions <n> delete":                  "DELETE /container-functions/{name}",
	"request container-functions <n> log":                     "GET /container-functions/{name}/logs",
	"request images upload":                                   "POST /images",
	"request images download":                                 "POST /images",
	"request images delete name <n>":                          "DELETE /images/{name}",
	"request interfaces <ifname> enable":                      "PUT /interfaces/{name}",
	"request interfaces <ifname> disable":                     "PUT /interfaces/{name}",
	"request interfaces <ifname> bind-dpdk":                   "PUT /interfaces/{name}/dpdk",
	"request interfaces <ifname|pci> unbind-dpdk":             "PUT /interfaces/{name}/dpdk",
	"request sriov create-vfs <ifname> count <n>":             "PUT /interfaces/{name}/sriov",
	"request sriov delete-vfs <ifname> vf <n>":                "PUT /interfaces/{name}/sriov",
	"request vpp restart":                                     "POST /vpp/restart",
	"request vpp trace start":                                 "POST /vpp/capture",
	"request vpp trace stop":                                  "DELETE /vpp/capture",
	"request vpp trace export":                                "GET /vpp/capture/{file}",
	"request system software add":                             "POST /system/software",
	"request system software rollback":                        "POST /system/software:rollback",
	"request system reboot":                                   "POST /system:reboot",
	"request system shutdown":                                 "POST /system:shutdown",
	"request system poweroff":                                 "POST /system:shutdown",
	"request system kernel apply":                             "POST /system/kernel:apply",
	"request system kernel rollback":                          "POST /system/kernel:rollback",
	"request system configuration backup":                     "POST /system/backup + GET /system/backup/{file}",
	"request system configuration restore":                    "POST /system/restore",
	"request system tech-support generate":                    "POST /system/tech-support",
	"request system core-dumps delete":                        "DELETE /system/core-dumps",
	"request system zeroize":                                  "POST /system:zeroize",
	"request system api tls regenerate":                       "POST /system/tls:regenerate",
	"request system password change":                          "POST /system/login-users/{name}:change-password",
	"request system ntp sync":                                 "POST /system/ntp:sync",
	"request alarms clear":                                    "POST /alarms:clear",
	"request alarms clear all":                                "POST /alarms:clear",
	"request alarms clear id <id>":                            "POST /alarms:clear",
	// ---- 其余操作与管道 ----
	"configure":              "GET /configuration/candidate + POST /configuration/commit",
	"| compare":              "GET /configuration/diff",
	"| compare rollback <n>": "GET /configuration/diff + POST /configuration/rollback/{n}",
	// ---- 配置模式：导航与事务（set/delete 语句由 candidate API 架构性覆盖，见 classify） ----
	"show":             "GET /configuration/candidate",
	"commit":           "POST /configuration/commit",
	"commit confirmed": "POST /configuration/commit + POST /configuration/commit:confirm",
	"commit and-quit":  "POST /configuration/commit",
	"rollback":         "POST /configuration/rollback/{n}",
	"discard":          "DELETE /configuration/candidate",
	"load override":    "PUT /configuration/candidate",
	"save":             "GET /configuration/candidate + POST /system/backup",
}

// cliRESTExceptions CLI-only by design（REPL 交互形态或 CLI 侧渲染，Web 用等价形态，不需要 API）。
var cliRESTExceptions = map[string]string{
	"wizard":                      "决策 #107：CLI 端交互编排，明确无 API 端点（Web 等价物是向导式页面）",
	"monitor interfaces <ifname>": "决策 #92：CLI 轮询形态；Web 等价物是定时刷新 + GET /events 推送",
	"monitor vnf <name>":          "决策 #92：同上",
	"start shell":                 "本地控制台 shell，Web 无对应形态",
	"exit":                        "CLI 本地行为",
	"quit":                        "CLI 本地行为",
	"help":                        "REPL 帮助；Web 用表单与静态候选",
	"help <command>":              "REPL 帮助；Web 用表单与静态候选",
	"?":                           "上下文补全（按键即时）；Web 用表单与静态候选",
	"edit":                        "配置模式层级导航；数据操作已被 candidate API 覆盖",
	"up":                          "配置模式层级导航；同上",
	"top":                         "配置模式层级导航；同上",
	"annotate":                    "REPL 注释便利",
	"run":                         "配置模式内执行便利；被运行的命令本身都有端点",
	"show log vnf <name>":         "指引型命令（指向容器 log / VM console），等价物已存在",
	"| match":                     "CLI 文本过滤；Web 以前端过滤 + 分页查询参数实现",
	"| except":                    "同上",
	"| count":                     "同上",
	"| last":                      "同上",
	"| begin":                     "同上",
	"| display json":              "CLI 渲染；Web 原生消费 JSON",
	"| display xml":               "CLI 渲染；Web 原生消费 JSON",
	"show | display set":          "CLI 文本渲染；契约已登记未实现（决策 #84④）",
}

// cliRESTGaps 已登记的 REST 缺口（CLI 有、REST 无）→ 理由/归属。补一个划掉一个。
// 与 docs/CLI-REST覆盖核查.md §3 的 14 项一一对应。
var cliRESTGaps = map[string]string{
	"show log system":   "无日志端点；决策 #115 列入后续增量（日志视图），核查 #2",
	"ping <host>":       "无端点（CLI 经 VPP L3）；Web 排障刚需，核查 #3",
	"traceroute <host>": "无端点；同上，核查 #4",
	"show virtual-switches <name> statistics":          "VirtualSwitch 响应无统计字段（数据在服务端有），核查 #5",
	"show virtual-machine-functions <name> statistics": "VM 详情响应无统计字段，核查 #6",
	"clear interfaces statistics":                      "无端点，核查 #7",
	"request system ssh host-key regenerate":           "无端点（宿主 sshd 操作），核查 #8",
	"request system core-dumps export":                 "core-dumps 只有 GET/DELETE，无导出，核查 #9",
	"commit check":                                     "POST /configuration/commit 无 check 模式，核查 #10",
	"load merge":                                       "PUT candidate 只有 override 语义，核查 #11",
	"request system api token revoke":                  "V1 明确延期（决策 #76⑧）；仅 DELETE /login 吊销当前会话，核查 #12",
	"request system storage format-data":               "V1 有意延期（破坏性操作），核查 #13",
	"show vpp runtime":                                 "两边都未接入（附录 A #34），核查 #14",
}

// classify 返回命令的归属：A=覆盖（端点）、C=例外、B=缺口、""=未归类。
// set/delete 配置语句由 candidate API 架构性覆盖（一整轮 API 覆盖全部配置语句，
// 无需逐条登记——这正是 REST 事务模型相对 CLI 语句的本质）。
func classify(cmd string) (bucket, detail string) {
	bestLen := 0
	try := func(m map[string]string, b string) {
		for k, v := range m {
			if n := cliFormMatch(cmd, k); n > bestLen {
				bestLen, bucket, detail = n, b, v
			}
		}
	}
	try(cliRESTCoverage, "A")
	try(cliRESTExceptions, "C")
	try(cliRESTGaps, "B")
	if bestLen > 0 {
		return bucket, detail
	}
	if strings.HasPrefix(cmd, "set ") || strings.HasPrefix(cmd, "delete ") {
		return "A", "candidate API（配置语句：PUT /configuration/candidate，X-NFVIS-Auto-Commit 一次完成）"
	}
	return "", ""
}

// TestCLIRESTCoverageEndpointsExist 覆盖表声明的端点必须真实存在于契约——
// 端点改名/删除时这里红（防映射过期；routes_contract 判不出这个方向）。
func TestCLIRESTCoverageEndpointsExist(t *testing.T) {
	routes := contractRoutes(t)
	for cmd, ep := range cliRESTCoverage {
		for _, part := range strings.Split(ep, " + ") {
			m := strings.TrimSpace(part)
			if !routes[m] {
				t.Errorf("覆盖表声明 %q → %s，但契约里没有该路由（映射过期？命令还指向已不存在的端点）", cmd, m)
			}
		}
	}
}

// TestContractCLICommandsAreClassified 既有契约命令清单里每条都必须落在三张表之一
// （或 set/delete 前缀规则）——新增契约命令忘记归类即红。
func TestContractCLICommandsAreClassified(t *testing.T) {
	for _, cmd := range contractCLICommands {
		bucket, detail := classify(cmd)
		if bucket == "" {
			t.Errorf("契约命令 %q 未归类：请补入 cliRESTCoverage / cliRESTExceptions / cliRESTGaps 之一（Web 控制台的功能覆盖不能悄悄出现空洞）", cmd)
			continue
		}
		t.Logf("%-4s %s → %s", bucket, cmd, detail)
	}
}

// TestCLIRESTGapsAndExceptionsHaveReasons 缺口与例外必须写明理由（登记不写理由等于没登记）。
func TestCLIRESTGapsAndExceptionsHaveReasons(t *testing.T) {
	for cmd, reason := range cliRESTGaps {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("缺口 %q 未写理由", cmd)
		}
	}
	for cmd, reason := range cliRESTExceptions {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("例外 %q 未写理由", cmd)
		}
	}
}
