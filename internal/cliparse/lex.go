// Package cliparse 提供 CLI 命令行/脚本的**纯词法**函数（无状态、可单测）。
//
// 单一事实源（决策 #313）：守护进程的命令分词（internal/api）与 CLI 客户端的脚本切句
// （cmd/nfvis-cli）、交互续行（internal/cli）共用同一套引号语义。此前 internal/api 有两份
// 近似实现——`splitFieldsQuoted` 认反斜杠转义、`splitUnquoted` 不认——而客户端又按 `\n` 盲切
// 脚本，于是「引号内的 `|` 被当管道切分」「多行引号值只取首行」长期共存（round34/35 登记）。
//
// 统一语义（对操作者可见、可解释）：
//   - 双引号内的空白、换行、`|`、`#` 都不参与切分；引号本身在分词时剥除；
//   - 引号内（及引号外）的 `\"` 与 `\\` 是字面量 `"` 与 `\`；
//   - 未闭合的引号按「到文本末尾」宽容处理（不静默出错，交由上层给出明确报错）；
//   - CLI **没有注释语法**：`#` 在任何位置都是普通字符——user-data 的 `#!/bin/sh`、
//     `#cloud-config` 才能内联（有意不引入行首注释，见决策 #313④）。
package cliparse

import "strings"

// SplitFields 按空白切分命令，但**尊重双引号**：引号内的空白与换行不切分、引号剥除，
// `\"` 与 `\\` 表示字面量。未闭合引号时按到文本末尾为一个 token。
//
// 与旧 internal/api.splitFieldsQuoted 逐字同义（决策 #313 收敛到本包）。
func SplitFields(s string) []string {
	var out []string
	var b strings.Builder
	inQuote, started := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\'):
			b.WriteByte(s[i+1])
			i++
			started = true
		case c == '"':
			inQuote = !inQuote
			started = true
		case (c == ' ' || c == '\t') && !inQuote:
			if started {
				out = append(out, b.String())
				b.Reset()
				started = false
			}
		default:
			b.WriteByte(c)
			started = true
		}
	}
	if started {
		out = append(out, b.String())
	}
	return out
}

// SplitUnquoted 按 sep 拆分，**忽略双引号内的 sep**，并尊重 `\"`/`\\` 转义
// （转义字符原样保留在结果里，以便下游分词器再解析）。
// 与旧 internal/api.splitUnquoted 同义，但补上转义感知（决策 #313）。
func SplitUnquoted(s string, sep rune) []string {
	var segs []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\'):
			cur.WriteByte(c)
			cur.WriteByte(s[i+1])
			i++
		case c == '"':
			inQuote = !inQuote
			cur.WriteByte(c)
		case rune(c) == sep && !inQuote:
			segs = append(segs, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(segs, cur.String())
}

// OpenQuote 报告文本末尾是否停在**未闭合的双引号**内（`\"`/`\\` 转义已计入）。
func OpenQuote(s string) bool {
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\'):
			i++
		case c == '"':
			inQuote = !inQuote
		}
	}
	return inQuote
}

// SplitStatements 把脚本文本切成**逻辑语句**：按未引用的换行切分；引号未闭合时把后续行
// 并入同一语句（值内保留换行）。返回值保留原始空白与换行，由调用方按需 TrimSpace/跳过空行。
//
// 这是「多行引号值」的落点：`-c`/`-f` 与交互续行都不再按物理行盲切。
func SplitStatements(text string) []string {
	var out []string
	var cur strings.Builder
	for _, ln := range strings.Split(text, "\n") {
		if cur.Len() > 0 {
			cur.WriteByte('\n')
		}
		cur.WriteString(ln)
		if OpenQuote(cur.String()) {
			continue // 引号未闭合：后续行并入同一语句（值内换行保留）
		}
		out = append(out, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		out = append(out, cur.String()) // 末尾仍未闭合：按到文本末尾为一语句（上层报错）
	}
	return out
}
