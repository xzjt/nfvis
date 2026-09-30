package api

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/cliparse"
)

// 决策 #313：多行取值经 display set 反推必须加引号，且加引号后可回放还原。
//
// 若不引号，值会直接铺成多行——经脚本切句后不再是同一条语句，回放即不等；
// 加引号后 cliparse.SplitStatements 把跨行引号值并回整段（值内换行保留）。
func TestQuoteStatementTokenQuotesMultilineAndReplays(t *testing.T) {
	val := "#!/bin/sh\necho hi"
	tok := quoteStatementToken(val)
	if !strings.HasPrefix(tok, `"`) || !strings.HasSuffix(tok, `"`) {
		t.Fatalf("含换行的取值必须加引号，实得 %q", tok)
	}
	// joinStatementTokens 对非首 token 统一加引号，故这里传**原值**（与 display set 生成同路）。
	line := joinStatementTokens([]string{"set", "virtual-machine-functions", "fw-vm", "user-data", val})
	stmts := cliparse.SplitStatements(line)
	if len(stmts) != 1 {
		t.Fatalf("跨行引号语句应并为一条，实得 %d 条：%#v", len(stmts), stmts)
	}
	toks := splitFieldsQuoted(stmts[0])
	if len(toks) != 5 || toks[4] != val {
		t.Fatalf("回放应还原多行取值 %q，实得 %#v", val, toks)
	}
}

// TestSplitFieldsKeepsQuotedPipe 引号内的 `|` 不参与管道切分、落在同一 token 里（决策 #155/#313）。
func TestSplitFieldsKeepsQuotedPipe(t *testing.T) {
	cmd, pipes, err := splitPipes(`set virtual-switches vs description "a|b" | count`)
	if err != nil {
		t.Fatalf("splitPipes 不应报错: %v", err)
	}
	if len(pipes) != 1 || pipes[0].kind != "count" {
		t.Fatalf("应切出 1 段 count 管道，实得 %#v", pipes)
	}
	toks := splitFieldsQuoted(cmd)
	if len(toks) != 5 || toks[4] != "a|b" {
		t.Fatalf("引号内的 `|` 应留在同一 token，实得 %#v", toks)
	}
}
