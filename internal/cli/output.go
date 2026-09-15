package cli

// raw 模式下的输出换行与候选列表渲染（决策 #81）。
//
// 背景：golang.org/x/term 的 MakeRaw 会清掉终端 OPOST，ONLCR 随之失效——
// 写出的裸 \n 只把光标下移、**不回车**。行编辑器自身历来用 "\r\n" 规避
// （见 rawmode.go 的 Enter/Ctrl-C/search），但命令输出与服务端文本是原样
// 写出的，全是 LF 结尾：在 raw 会话里就会逐行右移，候选列表呈阶梯状、
// 提示符也被顶到行中间。此处把同一个处理扩展到全部命令输出。

import (
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/xzjt/nfvis/internal/schema"
)

// crlfWriter 在 raw 生效期间把输出中的裸 \n 补成 \r\n。
// 已有 \r 的 \n 不重复转换（Editor 自身输出用 "\r\n"）；raw 为 nil 或返回
// false 时原样透传——非 raw（管道/脚本、monitor 挂起期）终端自身会做
// NL→CRNL，重复转换会凭空多出空行。
type crlfWriter struct {
	w    io.Writer
	raw  func() bool
	prev byte // 上一块的末字节：\r 与 \n 可能落在两次 Write 里
	has  bool
}

func (c *crlfWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if c.raw == nil || !c.raw() {
		n, err := c.w.Write(p)
		if n > 0 {
			c.prev, c.has = p[n-1], true
		}
		return n, err
	}
	buf := make([]byte, 0, len(p)+8)
	for i, b := range p {
		prevCR := (i > 0 && p[i-1] == '\r') || (i == 0 && c.has && c.prev == '\r')
		if b == '\n' && !prevCR {
			buf = append(buf, '\r')
		}
		buf = append(buf, b)
	}
	c.prev, c.has = p[len(p)-1], true
	if _, err := c.w.Write(buf); err != nil {
		return 0, err
	}
	return len(p), nil
}

// printCandidates 渲染候选列表（FR-CLI-001/002/003，命令树设计 §5.1/§5.6）。
// 调用方只需写 "\n"：raw 会话里的 CRLF 由 crlfWriter 负责。
// 列宽取「最长候选 + 2 空格」与 24 的较大者——固定 24 小于
// virtual-machine-functions（25 字符）会让 token 与描述粘连。
func printCandidates(w io.Writer, cs []schema.Candidate) {
	const minWidth = 24
	width := minWidth
	for _, c := range cs {
		if n := utf8.RuneCountInString(c.Token) + 2; n > width {
			width = n
		}
	}
	for _, c := range cs {
		fmt.Fprintf(w, "  %-*s%s\n", width, c.Token, c.Desc)
	}
}
