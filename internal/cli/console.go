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

	// 服务端 → 本地：原样回显（串口字节流）。EOF/错误即结束，close(done) 通知主循环。
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(r.out, stream)
		close(done)
	}()

	r.copyConsole(stream, os.Stdin, done)
}

// consoleChunk 本地输入的一小片字节（data 非空）或结束信号（err 非 nil）。
type consoleChunk struct {
	data []byte
	err  error
}

// pumpConsoleInput 在独立协程中读本地输入，逐片投递到通道。
//
// 决策 #310：主循环此前**直接阻塞**在 `in.Read` 上；服务端断开只会让 `done` 置位，
// 阻塞在 Read 的主循环根本看不到——必须再按一次键才会从 Read 返回、回到 select。
// 把读取搬进协程后，主循环得以同时等待「服务端断开」与「本地按键」，断开即退、无需按键。
// stop 关闭后协程在**当前 Read 返回时**尽快结束（不能取消阻塞的 os.Stdin 读，故最多再消费
// 一个已在途的字节——这是本修法的有界代价，见决策 #310 边界④）。
func pumpConsoleInput(stop <-chan struct{}, in io.Reader) <-chan consoleChunk {
	ch := make(chan consoleChunk, 1)
	go func() {
		defer close(ch)
		buf := make([]byte, 1)
		for {
			n, err := in.Read(buf)
			if n > 0 {
				b := append([]byte(nil), buf[:n]...)
				select { // 已请求停止：丢弃在途字节直接退出，不再吞后续输入
				case <-stop:
					return
				default:
				}
				select {
				case ch <- consoleChunk{data: b}:
				case <-stop:
					return
				}
			}
			if err != nil {
				select {
				case ch <- consoleChunk{err: err}:
				case <-stop:
				}
				return
			}
		}
	}()
	return ch
}

// copyConsole 本地输入 → 服务端透传，并在**任一方向断开时立即返回**（决策 #310）。
//
// 参数皆可注入（stream 为可写端、localIn 为本地输入、done 在服务端断开时关闭），
// 便于单测在没有真实 TTY/WebSocket 的情况下复现旧阻塞：断言「服务端断开后限时返回」。
func (r *REPL) copyConsole(stream io.Writer, localIn io.Reader, done <-chan struct{}) {
	stop := make(chan struct{})
	defer close(stop)
	in := pumpConsoleInput(stop, localIn)
	for {
		select {
		case <-done:
			fmt.Fprintf(r.out, "\r\n[串口已断开]\r\n")
			return
		case c, ok := <-in:
			if !ok { // 输入协程已结束（stop 生效）：静默返回
				return
			}
			if len(c.data) > 0 {
				if c.data[0] == consoleExitByte {
					fmt.Fprintf(r.out, "\r\n[已退出串口]\r\n")
					return
				}
				if _, werr := stream.Write(c.data); werr != nil {
					fmt.Fprintf(r.out, "\r\n[串口写入失败: %v]\r\n", werr)
					return
				}
			}
			if c.err != nil {
				if c.err != io.EOF {
					fmt.Fprintf(r.out, "\r\n[读取本地输入失败: %v]\r\n", c.err)
				}
				return
			}
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
