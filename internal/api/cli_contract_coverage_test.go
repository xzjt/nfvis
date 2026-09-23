package api

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
)

// cli_contract_coverage_test.go —— CLI 契约命令覆盖守护（M5-9 收尾）。
//
// 由来：M5-9 声称「不再有占位提示」，但实际 `show interfaces/port-mirroring/qos/vpp`、
// `monitor vnf`、`help`、`start shell`、`request interfaces/sriov` 仍落回通用 fallback
// （契约 §1.1/§1.2/§1.3 声明、实现无分支）。本测试逐条执行契约命令，断言**不得**命中
// 通用 fallback；真正延期到 V2 的命令必须列入 deferred 白名单（含原因），使其可见且需
// 显式维护。

// 通用 fallback 标记（分发缺分支的信号）。
const (
	fallbackShow    = "该 show 命令形式未支持"
	fallbackRequest = "该命令依赖底座运行态"
)

// deferred 明确延期的命令（返回自身说明而非通用 fallback，且已记入决策）。
var deferred = map[string]string{
	"request system storage format-data": "破坏性操作，V1 仅重置数据分区，待数据分区定义后开放",
	"request system password change":     "需交互式口令输入（CLI 前端 prompt），待前端交互落地",
}

// contractCLICommands 契约 §1.1/§1.2/§1.3 的代表命令（新增命令须同步补入）。
var contractCLICommands = []string{
	// §1.1 show 族
	"show version", "show system uptime", "show system cpu", "show system memory",
	"show system storage", "show system hugepages", "show system hardware",
	"show system core-dumps", "show system tech-support", "show system configuration sessions",
	"show interfaces", "show interfaces physical", "show interfaces physical ens224",
	"show interfaces ens224", "show interfaces ens224 detail", "show interfaces ens224 statistics",
	"show interfaces ens224 sriov", "show interfaces management",
	"show virtual-switches", "show virtual-switches vs1", "show virtual-switches vs1 mac-table",
	"show vrfs", "show vrfs vr1", "show vrfs vr1 routes",
	"show vpp", "show vpp threads", "show vpp buffers", "show vpp memory", "show vpp capture",
	"show acls", "show acls a1", "show bonds", "show nat",
	"show port-mirroring", "show qos policies",
	"show protocols lldp neighbors", "show lldp neighbors",
	"show alarms", "show users", "show images", "show resource-pools",
	"show log system", "show log audit", "show log vnf vm1",
	"show virtual-machine-functions", "show container-functions", "show configuration",
	"show configuration history",
	// §1.2 request 族
	"request virtual-machine-functions vm1 start", "request virtual-machine-functions vm1 stop",
	"request virtual-machine-functions vm1 console",
	"request container-functions ct1 start", "request container-functions ct1 log",
	"request images delete name img1",
	"request interfaces ens224 enable", "request interfaces ens224 disable",
	"request sriov create-vfs ens224 count 2", "request sriov delete-vfs ens224 vf 1",
	"request system reboot", "request system zeroize", "request system software rollback",
	"request system configuration backup", "request system tech-support generate",
	"request system ntp sync", "request system api tls regenerate",
	"request system ssh host-key regenerate",
	"request alarms clear all", "request vpp restart", "request vpp trace stop",
	// §1.3 其余
	"ping 192.0.2.1", "traceroute 192.0.2.1", "monitor interfaces ens224", "monitor vnf vm1",
	"clear interfaces statistics", "help", "help show", "start shell",
}

func TestCLIContractCommandCoverage(t *testing.T) {
	x, _ := newCLIKit(t)
	for _, cmd := range contractCLICommands {
		cmd := cmd
		t.Run(cmd, func(t *testing.T) {
			res := x.Execute("admin", aaa.ClassSuperUser, "ssh", cmd)
			out := res.Output
			if reason, ok := deferred[cmd]; ok {
				t.Logf("已登记延期：%s（%s）", cmd, reason)
				return
			}
			if strings.Contains(out, fallbackShow) || strings.Contains(out, fallbackRequest) {
				t.Fatalf("契约命令命中通用 fallback（缺少分发分支）：%s", strings.TrimSpace(out))
			}
			if strings.Contains(out, "无效命令") {
				t.Fatalf("契约命令被判为无效：%s", strings.TrimSpace(out))
			}
		})
	}
}

// TestCLIHelpAndStartShellSemantics help 与 start shell 的行为语义（§1.3）。
func TestCLIHelpAndStartShellSemantics(t *testing.T) {
	x, _ := newCLIKit(t)
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "help").Output
	if !strings.Contains(out, "configure") || !strings.Contains(out, "show") {
		t.Fatalf("help 应列出顶层命令: %s", out)
	}
	out = x.Execute("admin", aaa.ClassSuperUser, "ssh", "help show").Output
	if !strings.Contains(out, "子命令") {
		t.Fatalf("help show 应列出子命令: %s", out)
	}
	// SSH 会话必须拒绝 start shell（契约 §1.3 安全语义）
	out = x.Execute("admin", aaa.ClassSuperUser, "ssh", "start shell").Output
	if !strings.Contains(out, "仅允许本地 console") {
		t.Fatalf("SSH 会话下 start shell 应被拒绝: %s", out)
	}
}
