package container

// 容器内执行命令（决策 #357）：结果类型 + Docker exec 多路复用流的**纯解码**。
//
// 放在本文件（非 `_docker.go`）是刻意的：`*_docker.go` 按仓库约定进覆盖率排除，
// 而多路复用解码是这段逻辑里唯一有真实分支的部分，必须在 CI 里被单测覆盖。
//
// 帧格式（Docker Engine API，非 TTY exec）：8 字节头 `[stream, 0, 0, 0, len_be32]`
// 后跟 `len` 字节载荷；`stream` 1=stdout、2=stderr（3=system err，实测未出现）。
// 真机 spike（Docker 29.1.3）字节：`01 00 00 00 00 00 00 04 "out\n"` `02 … "err\n"`。

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/xzjt/nfvis/internal/orchestrator"
)

// ExecMaxStreamBytes / ExecResult 定义在 orchestrator（Provider 接口所在包，决策 #357）——
// 这里以**别名**复用同一类型：`container` 包反向依赖 orchestrator（ContainerProvider 在那边），
// 结果类型若定义在此会成环（同 network 包复用 ErrIfaceUnavailable 的先例）。
const ExecMaxStreamBytes = orchestrator.ExecMaxStreamBytes

type ExecResult = orchestrator.ExecResult

// demuxDockerStream 把 Docker exec 的多路复用流解成 (stdout, stderr, truncated)。
//
// 边界行为（均有单测）：
//   - 干净结束（EOF 在帧边界）⇒ err=nil；
//   - 帧头/载荷读不满（流中断）⇒ 返回已解出的部分 + 错误，由调用方判定「超时 vs 真错」；
//   - 未知 stream id（含 0）⇒ 归入 stderr（宁多不少，不丢数据）；
//   - 任一侧达到 capBytes ⇒ truncated=true，**继续读并丢弃**（保命令跑完）。
func demuxDockerStream(r io.Reader, capBytes int64) (stdout, stderr []byte, truncated bool, err error) {
	var outBuf, errBuf bytes.Buffer
	hdr := make([]byte, 8)
	chunk := make([]byte, 32<<10)

	for {
		if _, rerr := io.ReadFull(r, hdr); rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return outBuf.Bytes(), errBuf.Bytes(), truncated, nil
			}
			return outBuf.Bytes(), errBuf.Bytes(), truncated, fmt.Errorf("读取帧头: %w", rerr)
		}
		stream := hdr[0]
		remain := int64(binary.BigEndian.Uint32(hdr[4:8]))

		dst := &errBuf
		if stream == 1 {
			dst = &outBuf
		}
		for remain > 0 {
			n := int64(len(chunk))
			if n > remain {
				n = remain
			}
			if _, rerr := io.ReadFull(r, chunk[:n]); rerr != nil {
				return outBuf.Bytes(), errBuf.Bytes(), truncated, fmt.Errorf("读取帧载荷: %w", rerr)
			}
			remain -= n
			if room := capBytes - int64(dst.Len()); room > 0 {
				w := n
				if w > room {
					w = room
					truncated = true
				}
				dst.Write(chunk[:w])
			} else {
				truncated = true // 已满：读掉但丢弃
			}
		}
	}
}
