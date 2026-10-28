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
	// L4：串口/容器终端由**本阶段的读取者**消费按键（ReadLine 不在跑），结束时必须重新
	// 武装空闲守卫——否则回到 ReadLine 后 poll 立刻判「会话空闲超时」，把一直在打字的操作者
	// 登出并丢弃候选。所有返回路径（非 TTY / 拨号失败 / Ctrl-] / 断开）都由这条 defer 覆盖。
	defer r.touchIdle()
	if !r.editor.IsRaw() {
		// 非 TTY：不做终端接管（透传会阻塞脚本）；给出可复制的手工方式。
		fmt.Fprintf(r.out, "%% 当前环境不支持交互式%s接管（非 TTY）。\n", strings.TrimSpace(consoleKindLabel(req)))
		fmt.Fprintf(r.out, "%% 如需脚本接入：WebSocket %s（需一次性 ticket，请经 API 客户端连接）。\n", req.WSURL)
		return
	}
	stream, err := r.session.DialConsole(req.WSURL, consoleKindName(req))
	if err != nil {
		fmt.Fprintf(r.out, "%% %v\n", err)
		return
	}
	defer stream.Close()

	fmt.Fprintf(r.out, "\r\n[已进入 %s%s，Ctrl-] 退出]\r\n", req.VM, consoleKindLabel(req))

	// 服务端 → 本地：原样回显（串口字节流）。EOF/错误即结束，close(done) 通知主循环。
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(r.out, stream)
		close(done)
	}()

	r.copyConsole(stream, os.Stdin, done, strings.TrimSpace(consoleKindLabel(req)))
	// L12：Ctrl-]/本地 EOF 返回时，上面那个 copy 协程可能仍卡在一次 Write 里，而 r.out 与
	// REPL 的输出是**同一个** writer（crlfWriter 的 prev/has 是跨 Write 的状态）。关掉流让它
	// 的 Read 立即返回，再 join 它——保证此后提示符/命令输出绝不与该协程并发写同一 writer。
	// （并发本身也已由 crlfWriter 的互斥保护，这两条是同一缺陷的两面：一面防竞争、一面防交错。）
	joinConsoleCopy(stream, done)
}

// joinConsoleCopy 结束「服务端 → 本地」的 copy 协程：关闭流使其 Read 立即返回，再等它退出
// （返回后该协程绝不再写 writer）。独立成函数以便单测用可阻塞的底层 writer 复现「返回时
// 它还在写」的现场（见 fix_r51_l12_test.go）。
func joinConsoleCopy(stream io.Closer, done <-chan struct{}) {
	_ = stream.Close()
	<-done
}

// consoleKindName 会话种类**显示名**（无前后缀）：VM 串口 / 容器终端（决策 #358/#375）。
// 单一来源：既供 SDK 拨号失败文案（决策 #375/R142 B10——同一函数被两类会话共用，写死
// 「串口 console」会在容器终端上张冠李戴），也供下方 label 复用。
func consoleKindName(req *cliclient.ConsoleRequest) string {
	if req != nil && req.Kind == "container" {
		return "容器终端"
	}
	return "串口"
}

// consoleKindLabel 会话种类后缀（带前导空格，用于提示语拼接）：VM 串口 / 容器终端。
// 复用 consoleKindName 的同一映射，不新造第二套 kind→文案。
func consoleKindLabel(req *cliclient.ConsoleRequest) string {
	return " " + consoleKindName(req)
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
func (r *REPL) copyConsole(stream io.Writer, localIn io.Reader, done <-chan struct{}, label string) {
	stop := make(chan struct{})
	defer close(stop)
	in := pumpConsoleInput(stop, localIn)
	for {
		select {
		case <-done:
			fmt.Fprintf(r.out, "\r\n[%s已断开]\r\n", label)
			return
		case c, ok := <-in:
			if !ok { // 输入协程已结束（stop 生效）：静默返回
				return
			}
			if len(c.data) > 0 {
				r.touchIdle() // L4：按键由本阶段消费，据此刷新空闲守卫（ReadLine 不在跑）
				if c.data[0] == consoleExitByte {
					fmt.Fprintf(r.out, "\r\n[已退出%s]\r\n", label)
					return
				}
				if _, werr := stream.Write(c.data); werr != nil {
					fmt.Fprintf(r.out, "\r\n[%s写入失败: %v]\r\n", label, werr)
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
