// 守护：打开 VM 串口 pty 必须带 O_NOCTTY（决策 #183）。
//
// 由来（round86 真机实测，P0 级）：`request virtual-machine-functions <vm> console`
// 打开的 VM 串口 pty 是**不带 O_NOCTTY** 打开的。nfvisd 由 systemd 拉起时是「无控制终端的
// 会话首进程」，此时打开 tty 不带该标志会让它成为本进程的**控制终端**；此后该 pty 的 master
// （QEMU）一关闭——例如这台 VM 被 stop——内核就向会话前台进程组发 SIGHUP，**nfvisd 当场退出**
// （systemd 再按 Restart=always 拉起）。现象是「一条 stop 请求收到 EOF + 服务重启计数 +1」，
// 从现象推不到根因；而漏洞本身只在「先用过 console、之后又停那台 VM」时才出现——
// 单测/冒烟都碰不到。
//
// 为什么用源码结构判据而不是行为用例：要复现「打开 tty 是否会夺取控制终端」，测试进程自己
// 必须是**无控制终端的会话首进程**（setsid），Go 的 go test 进程做不到；而这条规则一旦丢掉，
// 行为用例（哪怕有 pty 夹具）也只会表现为「偶发 SIGHUP」，无法稳定判红。故与
// TestUIConsoleRoleGating / TestAllowNoSuperUserAssignedOnlyByZeroize 同款「读源码断言」：
// 串口 pty 的打开点唯一（internal/orchestrator/compute/console_libvirt.go），
// 断言那里的 os.OpenFile 带 syscall.O_NOCTTY。
package archtest

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// consolePtyFile 唯一允许打开 VM 串口 pty 的源文件。
const consolePtyFile = "internal/orchestrator/compute/console_libvirt.go"

// ptyOpenWithNoCttY 带 O_NOCTTY 的 pty 打开写法。
var ptyOpenWithNoCttY = regexp.MustCompile(`os\.OpenFile\([^)]*O_NOCTTY`)

// ptyOpenAny 任何 os.OpenFile 打开（用于确认该文件里的打开点都能被上面的规则覆盖）。
var ptyOpenAny = regexp.MustCompile(`os\.OpenFile\(`)

func TestConsolePtyOpenedWithNoCttY(t *testing.T) {
	root := filepath.Join("..", "..")
	path := filepath.Join(root, filepath.FromSlash(consolePtyFile))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v（串口 console 的 pty 打开点换文件了？本守护需同步）", consolePtyFile, err)
	}
	src := string(b)
	if !strings.Contains(src, "SerialPtyPath") {
		t.Fatalf("%s 里找不到 SerialPtyPath：串口 pty 打开点可能已搬走，请同步本守护", consolePtyFile)
	}
	opens := ptyOpenAny.FindAllString(src, -1)
	noCttys := ptyOpenWithNoCttY.FindAllString(src, -1)
	if len(opens) == 0 {
		t.Fatalf("%s 里没有 os.OpenFile：打开点被搬走或改名，请同步本守护", consolePtyFile)
	}
	if len(noCttys) != len(opens) {
		t.Fatalf("串口 pty 必须以 O_NOCTTY 打开（决策 #183：否则会夺取控制终端，"+
			"VM 一停就 SIGHUP 杀死 nfvisd）: %s 里 %d 个 os.OpenFile、其中 %d 个带 O_NOCTTY",
			consolePtyFile, len(opens), len(noCttys))
	}
}
