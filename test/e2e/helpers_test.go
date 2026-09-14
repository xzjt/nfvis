//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"io"
	"mime/multipart"
)

// newScanner SSE 读取器（逐行）。
func newScanner(r io.Reader) *bufio.Scanner { return bufio.NewScanner(r) }

// newMultipart 构造单文件 multipart body，返回 Content-Type。
func newMultipart(buf *bytes.Buffer, field, filename string, data []byte) string {
	w := multipart.NewWriter(buf)
	part, _ := w.CreateFormFile(field, filename)
	_, _ = part.Write(data)
	_ = w.Close()
	return w.FormDataContentType()
}
