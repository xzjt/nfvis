package container

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

// frame 按 Docker exec 的帧格式拼一段（决策 #357）：8 字节头 + 载荷。
func frame(stream byte, payload string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(payload)))
	return append(h, payload...)
}

func TestDemuxDockerStream(t *testing.T) {
	cases := []struct {
		name        string
		in          []byte
		wantOut     string
		wantErr     string
		wantTrunc   bool
		wantErrFlag bool
	}{
		{
			name:    "单流 stdout",
			in:      frame(1, "hello\n"),
			wantOut: "hello\n",
		},
		{
			name:    "双流交错（真机 spike 的字节序）",
			in:      append(frame(1, "out\n"), frame(2, "err\n")...),
			wantOut: "out\n",
			wantErr: "err\n",
		},
		{
			name:    "同流多帧",
			in:      append(frame(1, "a"), frame(1, "b")...),
			wantOut: "ab",
		},
		{
			name:    "未知 stream id（含 3=system err）归入 stderr，不丢数据",
			in:      frame(3, "sys\n"),
			wantErr: "sys\n",
		},
		{
			name:    "空载荷帧",
			in:      frame(1, ""),
			wantOut: "",
		},
		{
			name:    "零字节流（命令无输出）",
			in:      nil,
			wantOut: "",
		},
		{
			name:        "帧头读不满 ⇒ 流中断报错（不 panic）",
			in:          []byte{1, 0, 0},
			wantErrFlag: true,
		},
		{
			name:        "载荷短于声明长度 ⇒ 流中断报错，已解出的部分保留",
			in:          append(frame(1, "ok"), []byte{2, 0, 0, 0, 0, 0, 0, 8, 'x', 'y'}...),
			wantOut:     "ok",
			wantErrFlag: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, errb, truncated, err := demuxDockerStream(bytes.NewReader(tc.in), ExecMaxStreamBytes)
			if (err != nil) != tc.wantErrFlag {
				t.Fatalf("错误判定错：err=%v wantErr=%v", err, tc.wantErrFlag)
			}
			if got := string(out); got != tc.wantOut {
				t.Fatalf("stdout=%q want %q", got, tc.wantOut)
			}
			if got := string(errb); got != tc.wantErr {
				t.Fatalf("stderr=%q want %q", got, tc.wantErr)
			}
			if truncated != tc.wantTrunc {
				t.Fatalf("truncated=%v want %v", truncated, tc.wantTrunc)
			}
		})
	}
}

// 上限：单侧超限 ⇒ 只留前 cap 字节并置 truncated，且**把剩余读掉**（否则对端阻塞、命令跑不完）。
func TestDemuxDockerStreamCapKeepsDraining(t *testing.T) {
	const cap = 8
	// 三段：前两段吃满 cap、第三段超出 ⇒ 必须仍把第三段读走。
	in := append(frame(1, "12345678"), frame(1, "9ABCDEFG")...)
	in = append(in, frame(2, "tail")...) // 后续帧仍能被解出 ⇒ 证明流被读到底
	out, errb, truncated, err := demuxDockerStream(bytes.NewReader(in), cap)
	if err != nil {
		t.Fatalf("demux: %v", err)
	}
	if !truncated {
		t.Fatal("超过上限应置 truncated")
	}
	if string(out) != "12345678" {
		t.Fatalf("超限后应只留前 %d 字节，得 %q", cap, out)
	}
	if string(errb) != "tail" {
		t.Fatalf("超限后仍应把后续帧读完（stderr=%q）", errb)
	}
}

// 跨帧边界的读（io.ReadFull 语义）：载荷被切成多个小块也必须完整解出。
func TestDemuxDockerStreamSplitReads(t *testing.T) {
	in := append(frame(1, "abcdef"), frame(2, "ghij")...)
	// 逐字节喂：模拟网络分片
	out, errb, _, err := demuxDockerStream(&iotest{data: in}, ExecMaxStreamBytes)
	if err != nil {
		t.Fatalf("demux: %v", err)
	}
	if string(out) != "abcdef" || string(errb) != "ghij" {
		t.Fatalf("分片读错：stdout=%q stderr=%q", out, errb)
	}
}

// iotest 每次最多返回 1 字节的 Reader（制造网络分片），读完回 io.EOF。
type iotest struct {
	data []byte
	off  int
}

func (r *iotest) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.data[r.off]
	r.off++
	return 1, nil
}
