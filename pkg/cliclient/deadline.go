package cliclient

// 决策 #366（R142-10b）：客户端等待时长随容器 exec 超时延长。
//
// `RequestTimeout=90s` 原是 http.Client 全局 Timeout，会先于服务端截断
// `timeout 91..300` 的 exec——用户看到「请求超时」，命令还在容器里跑。
// 现有界性由每请求 ctx deadline 提供，等待时长按命令内容识别 exec 超时提示后放宽。
//
// 口径（如实）：
//   - 这是**等待时长提示**，不是语义权威——服务端超时语义（504/%%）不变；
//   - 误命中只延长等待（多等一会儿无害）、漏判回落 RequestTimeout（现状）；
//   - 识别器做**最小**引号感知 token 扫描，不实现转义——带转义的怪写法漏判即回落默认，安全。

import (
	"strconv"
	"strings"
	"time"
)

// execTimeoutGrace 对识别到的 exec 超时提示加的裕量：给服务端读流/收尾留一点时间。
const execTimeoutGrace = 15 * time.Second

// execTimeoutHint 从一行 CLI 命令里识别「容器 exec 的超时提示值」。
//
// 命中：存在 token `exec`，且在同语句**后续** token 中存在 token `timeout`、
// 其后一个 token 是 1..300 的整数 ⇒ 返回 `n 秒 + 裕量`。
// 未命中（无 exec / 无 timeout / 取值越界 / 取值在引号内）⇒ 返回 0（调用方取 RequestTimeout）。
//
// 引号感知：双引号与单引号内的空格不切分，且**引号内 token 不与关键字比较**——
// `exec "echo timeout 5"` 里引号内的 timeout 不触发延长。
func execTimeoutHint(line string) time.Duration {
	toks := splitHintTokens(line)
	for i, t := range toks {
		if t.quoted || t.s != "exec" {
			continue
		}
		for j := i + 1; j < len(toks); j++ {
			if toks[j].quoted || toks[j].s != "timeout" {
				continue
			}
			if j+1 >= len(toks) || toks[j+1].quoted {
				continue
			}
			if n, err := strconv.Atoi(toks[j+1].s); err == nil && n >= 1 && n <= 300 {
				return time.Duration(n)*time.Second + execTimeoutGrace
			}
		}
	}
	return 0
}

// requestDeadline 一条 CLI 命令的客户端等待上限：max(RequestTimeout, exec 超时提示)。
func requestDeadline(line string) time.Duration {
	if h := execTimeoutHint(line); h > RequestTimeout {
		return h
	}
	return RequestTimeout
}

// hintToken 一个扫描 token：s 为内容；quoted 表示整段来自引号内（不与关键字比较）。
type hintToken struct {
	s      string
	quoted bool
}

// splitHintTokens 最小引号感知切分：空格/Tab 切分；双/单引号内的空格并入同一 token
// 并标记 quoted。不实现转义（反斜杠等原样保留）——漏判只影响等待时长，安全。
// 未闭合引号：其余部分并入当前 token（按引号内计）。
func splitHintTokens(line string) []hintToken {
	var toks []hintToken
	var cur strings.Builder
	hasTok := false
	inQuote := false
	quoted := false
	var q byte
	flush := func() {
		if hasTok {
			toks = append(toks, hintToken{s: cur.String(), quoted: quoted})
		}
		cur.Reset()
		hasTok = false
		quoted = false
	}
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if inQuote {
			if ch == q {
				inQuote = false
			} else {
				cur.WriteByte(ch)
			}
			continue
		}
		switch ch {
		case '"', '\'':
			if !hasTok { // 引号出现在 token 中段（如 foo"bar"）不单独标 quoted，按普通字符并入
				hasTok = true
				quoted = true
			}
			q = ch
			inQuote = true
		case ' ', '\t':
			flush()
		default:
			hasTok = true
			cur.WriteByte(ch)
		}
	}
	flush()
	return toks
}
