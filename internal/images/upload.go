package images

// round49（契约与实现漂移收口）：把 multipart 上传的镜像字节流暂存到 incoming 目录内的
// **安全临时文件**。
//
// 为什么必须有这一层：契约定的是 POST /images 三选一（multipart/form-data 上传 / JSON incoming
// 导入 / JSON URL 拉取），而 ImportIncoming 只接受**已经在 incoming 目录里的文件路径**——
// 上传必须先落盘、再把落点交给它。落盘这一步有两处必须做对：
//
//  1. **临时名由本包生成**（`upload-<随机十六进制>.tmp`），**绝不采用用户提供的名字**。
//     镜像名是用户输入（与镜像名越界同一类问题，见 name.go），拿它当落盘路径就是可被利用的
//     写路径：`../../etc/xxx` 这类名字会把文件写到仓库外面去。随机名 + O_EXCL 另带上
//     「不会被预置路径顶掉/跟随」的性质。
//  2. **落点复核**必须仍在 incoming 目录内——与 images.go 的 containedPath 同一口径
//     （先 filepath.Abs，Join/Clean 之后仍须带 `<dirAbs><分隔符>` 前缀），越界输入在这里失配被拒。
//
// 临时文件的生命周期（调用方 api 层配合）：
//   - 成功：ImportIncoming 把文件收走（VM 镜像 os.Rename 进仓库、容器镜像 docker load 后 os.Remove）；
//   - 失败：由调用方清理（api 层 defer os.Remove；文件已被收走时删除不存在的路径按无害处理）。

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrUploadTooLarge 上传内容超过大小上限（api 层据此回 413，而不是笼统的 400）。
var ErrUploadTooLarge = errors.New("上传内容超过大小上限")

// StageUpload 把 src 落到 incoming 目录内的安全临时文件，返回临时文件绝对路径与写入字节数。
//
// maxBytes 为本次暂存的上限：超过即报 ErrUploadTooLarge 并**删除已写部分**（不留半截文件）。
// 调用方仍应在请求体层面另设上限（http.MaxBytesReader）——那一层能阻止超量数据被读进来，
// 这一层是落盘侧的纵深防御（两边都用同一常量，不产生第二套口径）。
func (s *Store) StageUpload(src io.Reader, maxBytes int64) (string, int64, error) {
	incAbs, err := filepath.Abs(s.cfg.IncomingDir)
	if err != nil {
		return "", 0, fmt.Errorf("解析 incoming 目录 %s: %w", s.cfg.IncomingDir, err)
	}
	if st, err := os.Stat(incAbs); err != nil || !st.IsDir() {
		return "", 0, fmt.Errorf("incoming 目录 %s 不可用（先经 scp/sftp 或确认该目录存在）", s.cfg.IncomingDir)
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", 0, fmt.Errorf("生成临时文件名: %w", err)
	}
	tmp := filepath.Join(incAbs, "upload-"+hex.EncodeToString(rnd[:])+".tmp")
	// 与 containedPath 同口径的复核：Join 已 Clean，逃逸出 incoming 后前缀必然失配。
	// 名字全部由本包生成，故这一判恒真——保留它是纵深防御（挡住将来有人把名字参数化）。
	if !strings.HasPrefix(tmp, incAbs+string(filepath.Separator)) {
		return "", 0, fmt.Errorf("临时文件 %s 超出 incoming 目录 %s", tmp, s.cfg.IncomingDir)
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, fmt.Errorf("创建临时文件: %w", err)
	}
	// 上限 +1 字节：读到 maxBytes+1 即判超限（不必等到整条流读完）。
	n, werr := io.Copy(f, io.LimitReader(src, maxBytes+1))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return "", 0, fmt.Errorf("写入临时文件: %w", werr)
	}
	if n > maxBytes {
		_ = os.Remove(tmp)
		return "", 0, fmt.Errorf("%w：已写入 %d 字节 > 上限 %d 字节", ErrUploadTooLarge, n, maxBytes)
	}
	return tmp, n, nil
}
