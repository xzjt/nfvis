package images

// HTTP(S) URL 拉取：断点续传（Range）+ sha256 校验 + 进度回调（FR-CMP-031）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DownloadOptions URL 拉取参数。
type DownloadOptions struct {
	Name        string
	Type        string
	URL         string
	SHA256      string // 期望校验和（可空）
	Description string
	Client      *http.Client // 可空（缺省 10 分钟超时）
	Progress    func(done, total int64)
}

// Download 拉取镜像到仓库并登记元数据。中途状态记 downloading，成功 ready、失败 failed。
// 支持断点续传：若存在 <dest>.part 且服务端支持 Range，则从断点续传。
func (s *Store) Download(ctx context.Context, opts DownloadOptions) (Meta, error) {
	if opts.Name == "" || opts.URL == "" {
		return Meta{}, fmt.Errorf("name 与 url 必填")
	}
	if opts.Type != TypeVM && opts.Type != TypeContainer {
		return Meta{}, fmt.Errorf("type 必须为 %s 或 %s", TypeVM, TypeContainer)
	}
	// FR-SEC-004（决策 #71）：URL 拉取默认强制 sha256，缺省即拒绝（不再静默跳过校验）。
	// 提前失败，避免先登记 downloading 再报错。
	if strings.TrimSpace(opts.SHA256) == "" {
		return Meta{}, fmt.Errorf("URL 拉取必须提供 sha256（FR-SEC-004：默认强制校验）")
	}
	if len(strings.TrimSpace(opts.SHA256)) != 64 || !isHex(opts.SHA256) {
		return Meta{}, fmt.Errorf("sha256 必须为 64 位十六进制字符串: %q", opts.SHA256)
	}
	pending := Meta{Name: opts.Name, Type: opts.Type, SHA256: opts.SHA256,
		Description: opts.Description, ImportState: StateDownloading}
	if err := s.setMeta(pending); err != nil {
		return Meta{}, err
	}
	s.emitState(opts.Name, opts.Type, StateDownloading)
	meta, err := s.downloadFile(ctx, opts)
	if err != nil {
		pending.ImportState = StateFailed
		_ = s.setMeta(pending)
		s.emitState(opts.Name, opts.Type, StateFailed)
		return Meta{}, err
	}
	// 容器镜像：URL 拉取的是 docker save 归档 → `image load` 后删除临时文件，仅登记元数据。
	if opts.Type == TypeContainer {
		if s.dockerLoad == nil {
			_ = os.Remove(filepath.Join(s.cfg.Dir, opts.Name))
			pending.ImportState = StateFailed
			_ = s.setMeta(pending)
			return Meta{}, fmt.Errorf("拉取容器镜像 %s：未接入 Docker", opts.Name)
		}
		archive := filepath.Join(s.cfg.Dir, opts.Name)
		if err := s.dockerLoad(archive); err != nil {
			pending.ImportState = StateFailed
			_ = s.setMeta(pending)
			return Meta{}, fmt.Errorf("docker load %s: %w", opts.Name, err)
		}
		if err := os.Remove(archive); err != nil && !os.IsNotExist(err) {
			return Meta{}, err
		}
		meta.Format = "docker-archive"
	}
	if err := s.setMeta(meta); err != nil {
		return Meta{}, err
	}
	s.emitState(opts.Name, opts.Type, StateReady)
	return meta, nil
}

func (s *Store) downloadFile(ctx context.Context, opts DownloadOptions) (Meta, error) {
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	dest := filepath.Join(s.cfg.Dir, opts.Name)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return Meta{}, err
	}
	part := dest + ".part"

	var offset int64
	if st, err := os.Stat(part); err == nil {
		offset = st.Size()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.URL, nil)
	if err != nil {
		return Meta{}, err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := client.Do(req)
	if err != nil {
		return Meta{}, fmt.Errorf("拉取 %s: %w", opts.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return Meta{}, fmt.Errorf("拉取 %s: HTTP %d", opts.URL, resp.StatusCode)
	}

	flags := os.O_CREATE | os.O_WRONLY
	resuming := false
	switch resp.StatusCode {
	case http.StatusPartialContent:
		if offset > 0 {
			if start, ok := contentRangeStart(resp.Header.Get("Content-Range")); ok && start != offset {
				return Meta{}, fmt.Errorf("断点位置不一致：服务端 %d，本地 %d", start, offset)
			}
			flags |= os.O_APPEND
			resuming = true
		} else {
			flags |= os.O_TRUNC
		}
	default: // 200：服务端不支持 Range，从头写
		flags |= os.O_TRUNC
		offset = 0
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return Meta{}, err
	}

	// sha256：续传时先对已下载部分求摘要，保证整体校验正确。
	h := sha256.New()
	if resuming {
		if err := hashExisting(part, offset, h); err != nil {
			_ = f.Close()
			return Meta{}, err
		}
	}
	total := resp.ContentLength
	if total > 0 {
		total += offset
	}
	written := offset
	buf := make([]byte, 256*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				_ = f.Close()
				return Meta{}, werr
			}
			h.Write(buf[:n])
			written += int64(n)
			if opts.Progress != nil {
				opts.Progress(written, total)
			}
			if s.progress != nil {
				s.progress(opts.Name, written, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_ = f.Close()
			return Meta{}, fmt.Errorf("拉取中断（已下载 %d 字节，可重试续传）: %w", written, rerr)
		}
	}
	if err := f.Close(); err != nil {
		return Meta{}, err
	}

	sum := hex.EncodeToString(h.Sum(nil))
	// FR-SEC-004：URL 拉取**默认强制** sha256 校验——不提供即拒绝，避免拿到未校验的镜像。
	// 早期实现把 sha256 当可选项（缺省跳过校验），与规格"默认要求 sha256 校验"不符。
	if !equalFoldHex(sum, opts.SHA256) {
		_ = os.Remove(part)
		return Meta{}, fmt.Errorf("sha256 校验失败：期望 %s，实际 %s", opts.SHA256, sum)
	}
	if err := os.Rename(part, dest); err != nil {
		return Meta{}, err
	}
	return Meta{Name: opts.Name, Type: opts.Type, SizeBytes: written, SHA256: sum,
		Format: formatOf(opts.Name), Description: opts.Description,
		ImportedAt: s.now().UTC(), ImportState: StateReady}, nil
}

func hashReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashExisting 对本地已下载部分重新求摘要（断点续传时调用）。
func hashExisting(path string, n int64, h hash.Hash) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.CopyN(h, f, n); err != nil {
		return fmt.Errorf("读取已下载部分: %w", err)
	}
	return nil
}

// contentRangeStart 解析 Content-Range 起始字节（形如 "bytes 100-999/1000"）。
func contentRangeStart(v string) (int64, bool) {
	v = strings.TrimPrefix(v, "bytes ")
	i := strings.IndexByte(v, '-')
	if i <= 0 {
		return 0, false
	}
	var start int64
	if _, err := fmt.Sscanf(v[:i], "%d", &start); err != nil {
		return 0, false
	}
	return start, true
}

func equalFoldHex(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// isHex 判定字符串是否全为十六进制字符。
func isHex(s string) bool {
	for _, c := range strings.ToLower(strings.TrimSpace(s)) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
