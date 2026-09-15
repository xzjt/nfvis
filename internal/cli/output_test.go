package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/schema"
)

// ---------- 决策 #81：raw 模式输出换行与候选列表渲染 ----------

// crlfWriter 存在的唯一理由：MakeRaw 清掉终端 OPOST 后，
// 裸 \n 只下移光标不回车 → 多行输出逐行右移（候选列表呈阶梯、提示符错位）。
func TestCRLFWriterAddsCRForBareLFWhenRaw(t *testing.T) {
	var buf bytes.Buffer
	w := &crlfWriter{w: &buf, raw: func() bool { return true }}
	if _, err := w.Write([]byte("a\nb\n")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "a\r\nb\r\n" {
		t.Fatalf("raw 模式下裸 LF 应补 CR（否则阶梯错位），实际 %q", got)
	}
}

// 行编辑器自身输出历来用 "\r\n"：重复转换会凭空多出空行。
func TestCRLFWriterLeavesExistingCRLFAlone(t *testing.T) {
	var buf bytes.Buffer
	w := &crlfWriter{w: &buf, raw: func() bool { return true }}
	if _, err := w.Write([]byte("x\r\ny\r\n")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "x\r\ny\r\n" {
		t.Fatalf("已是 CRLF 不应再插 CR，实际 %q", got)
	}
}

// 非 raw（管道/脚本、monitor 挂起期）终端自身会做 NL→CRNL，不得重复转换。
func TestCRLFWriterPassthroughWhenNotRaw(t *testing.T) {
	var buf bytes.Buffer
	w := &crlfWriter{w: &buf, raw: func() bool { return false }}
	if _, err := w.Write([]byte("a\nb\n")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "a\nb\n" {
		t.Fatalf("非 raw 应原样透传，实际 %q", got)
	}
}

// 服务端文本按块写出，"\r" 与 "\n" 可能落在两次 Write 里。
func TestCRLFWriterHandlesCRAtChunkBoundary(t *testing.T) {
	var buf bytes.Buffer
	w := &crlfWriter{w: &buf, raw: func() bool { return true }}
	if _, err := w.Write([]byte("a\r")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("\nb")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "a\r\nb" {
		t.Fatalf("跨块的 \\r 与 \\n 不应被重复补 CR，实际 %q", got)
	}
}

// 固定 %-24s 小于最长候选（virtual-machine-functions 长 25）→ token 与描述粘连。
func TestPrintCandidatesPadsToLongestToken(t *testing.T) {
	cs := []schema.Candidate{
		{Token: "virtual-machine-functions", Desc: "VM VNF"},
		{Token: "vrfs", Desc: "L3 虚拟交换机"},
	}
	var buf bytes.Buffer
	printCandidates(&buf, cs)
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("候选应每条一行，实际 %d 行: %q", len(lines), buf.String())
	}
	if !strings.HasPrefix(lines[0], "  ") {
		t.Fatalf("候选行应以两空格缩进: %q", lines[0])
	}
	if !strings.HasPrefix(lines[0], "  virtual-machine-functions ") {
		t.Fatalf("长 token 与描述之间应有空格（不得粘连）: %q", lines[0])
	}
	if i, j := strings.Index(lines[0], "VM VNF"), strings.Index(lines[1], "L3 虚拟交换机"); i != j {
		t.Fatalf("描述列未对齐（%d vs %d）:\n%q", i, j, buf.String())
	}
}
