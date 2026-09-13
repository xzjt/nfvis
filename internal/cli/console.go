package cli

// M4-12：`request virtual-machine-functions <n> console` 的本地终端接管
// （FR-CMP-014，附录 A #50）。
//
// 服务端签发一次性 ticket 并回传 CLIEResult.Console；REPL 据此临时退出行编辑的
// raw 模式、把 stdin 逐字节转发到 WebSocket、stdout 回显服务端帧，Ctrl-] 退出并
// 恢复原终端状态与提示符（与 Ctrl-C 中的 monitor 退出法同源）。
//
// 非 TTY（管道/脚本）不做接管——打印明确提示，避免脚本挂死（与 monitor 一致）。

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/xzjt/nfvis/pkg/cliclient"
)

// consoleExitByte Ctrl-]：退出 console 的按键（FR-CMP-014 契约）。
const consoleExitByte = 0x1d

// confirmFlag 确认意图标记：REPL 收到问询并经用户答复 yes 后，以 `<命令> --yes`
// 重发（执行期内部约定，不进入命令树/契约语义）。
const confirmFlag = "--yes"

// runConsole 接管终端并与串口双向透传，Ctrl-] 退出。返回后调用方续打提示符。
func (r *REPL) runConsole(req *cliclient.ConsoleRequest) {
	if !r.editor.IsRaw() {
		// 非 TTY：不做终端接管（透传会阻塞脚本）；给出可复制的手工方式。
		fmt.Fprintf(r.out, "%% 当前环境不支持交互式串口接管（非 TTY）。\n")
		fmt.Fprintf(r.out, "%% 如需脚本接入：WebSocket %s（需一次性 ticket，请经 API 客户端连接）。\n", req.WSURL)
		return
	}
	stream, err := r.session.DialConsole(req.WSURL)
	if err != nil {
		fmt.Fprintf(r.out, "%% %v\n", err)
		return
	}
	defer stream.Close()

	fmt.Fprintf(r.out, "\r\n[已进入 %s 串口，Ctrl-] 退出]\r\n", req.VM)

	// 服务端 → 本地：原样回显（串口字节流）。EOF/错误即结束。
	done := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(r.out, stream)
		done <- struct{}{}
	}()

	// 本地 → 服务端：逐字节读，Ctrl-] 触发退出。
	in := bufio.NewReader(os.Stdin)
	buf := make([]byte, 1)
	for {
		select {
		case <-done:
			fmt.Fprintf(r.out, "\r\n[串口已断开]\r\n")
			return
		default:
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if buf[0] == consoleExitByte {
				fmt.Fprintf(r.out, "\r\n[已退出串口]\r\n")
				return
			}
			if _, werr := stream.Write(buf[:n]); werr != nil {
				fmt.Fprintf(r.out, "\r\n[串口写入失败: %v]\r\n", werr)
				return
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				fmt.Fprintf(r.out, "\r\n[读取本地输入失败: %v]\r\n", rerr)
			}
			return
		}
	}
}

// readConfirm 读取一行交互确认（`Delete VNF 'x'? [yes,no] `）。返回是否确认。
// 非 TTY/无输入/异常一律返回 false（破坏性动作不执行，安全默认）。
func (r *REPL) readConfirm(question string) bool {
	line, err := r.editor.ReadLine(question)
	if err != nil {
		fmt.Fprintln(r.out)
		return false
	}
	return strings.EqualFold(strings.TrimSpace(line), "yes")
}
