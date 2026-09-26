package images

// HTTP(S) URL 拉取：断点续传（Range）+ sha256 校验 + 进度回调（FR-CMP-031）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

const (
	// defaultDownloadTimeout 未注入 Client 时的超时。注意它是**整体**上限（连接+读 body），
	// 不是空闲超时：服务端停发数据时只有到这个点才会失败——故中断原因与已下载字节必须落盘，
	// 否则这段时间里操作者只能看着 downloading（R84-6）。
	defaultDownloadTimeout = 10 * time.Minute
	// progressSaveStep 进度落盘的字节节流（每下载这么多更新一次元数据，避免每 256KB 写索引）。
	progressSaveStep = 1 << 20
)

// Download 拉取镜像到仓库并登记元数据。中途状态记 downloading，成功 ready、失败 failed。
// 支持断点续传：若存在 <dest>.part 且服务端支持 Range，则从断点续传。
func (s *Store) Download(ctx context.Context, opts DownloadOptions) (Meta, error) {
	if err := ValidateDownloadOptions(opts); err != nil {
		return Meta{}, err
	}
	pending := Meta{Name: opts.Name, Type: opts.Type, SHA256: opts.SHA256,
		Description: opts.Description, ImportState: StateDownloading}
	if err := s.setMeta(pending); err != nil {
		return Meta{}, err
	}
	s.emitState(opts.Name, opts.Type, StateDownloading)

	// 进度落盘（R84-6）：异步拉取期间操作者只能经 `show images <名> detail` 观察，
	// 故把已下载/总字节写进元数据——复用既有进度回调（opts.Progress / s.progress），不另造机制。
	var done, total int64
	prevProgress := opts.Progress
	lastSaved := int64(0)
	var progressErr error
	opts.Progress = func(d, t int64) {
		done, total = d, t
		if prevProgress != nil {
			prevProgress(d, t)
		}
		if d-lastSaved < progressSaveStep {
			return
		}
		lastSaved = d
		m := pending
		m.DownloadedBytes, m.TotalBytes = d, t
		if err := s.setMeta(m); err != nil && progressErr == nil {
			// 进度落盘失败不中断拉取：最终状态还会再写一次（成功时那次写失败会直接报错），
			// 失败时把这里的首个错误随结果一并上报——不静默丢弃。
			progressErr = err
		}
	}
	// failedMeta 失败时登记的元数据：带上已下载字节/总字节，让 failed 也能说明「断在哪」。
	failedMeta := func() Meta {
		m := pending
		m.DownloadedBytes, m.TotalBytes = done, total
		if m.DownloadedBytes == 0 {
			// 一个字节都没读到就失败（连接/HTTP 层）：断点仍可能留在 .part（续传场景）。
			if st, err := os.Stat(filepath.Join(s.cfg.Dir, opts.Name) + ".part"); err == nil {
				m.DownloadedBytes = st.Size()
			}
		}
		return m
	}

	meta, err := s.downloadFile(ctx, opts)
	if err != nil {
		if progressErr != nil {
			err = fmt.Errorf("%w（进度落盘失败：%v）", err, progressErr)
		}
		return Meta{}, s.fail(failedMeta(), err)
	}
	// 容器镜像：URL 拉取的是 docker save 归档 → `image load` 后删除临时文件，仅登记元数据。
	if opts.Type == TypeContainer {
		if s.dockerLoad == nil {
			_ = os.Remove(filepath.Join(s.cfg.Dir, opts.Name))
			return Meta{}, s.fail(failedMeta(), fmt.Errorf("拉取容器镜像 %s：未接入 Docker", opts.Name))
		}
		archive := filepath.Join(s.cfg.Dir, opts.Name)
		if err := s.dockerLoad(archive, opts.Name); err != nil {
			return Meta{}, s.fail(failedMeta(), fmt.Errorf("docker load %s: %w", opts.Name, err))
		}
		if err := os.Remove(archive); err != nil && !os.IsNotExist(err) {
			// 镜像已 load 进 Docker（可用），只是临时归档没清掉：不能因此把可用镜像判 failed
			// （failed 会被 Lookup 挡掉），也不能让状态停在 downloading——登记 ready 并把残留
			// 文件记进说明，操作者至少能看到该手工清哪个文件。
			meta.LastError = fmt.Sprintf("容器镜像已入库，但临时归档 %s 未能清理：%v", archive, err)
		}
		meta.Format = "docker-archive"
	}
	if err := s.setMeta(meta); err != nil {
		return Meta{}, err
	}
	s.emitState(opts.Name, opts.Type, StateReady)
	return meta, nil
}

// fail 统一收口失败：状态落 failed、失败原因写进元数据（`show images <名> detail` 可见）、
// 发布状态事件，并把「状态落盘失败」与原始错误一并返回。
// 此前失败分支用 `_ = s.setMeta(pending)` 丢弃了错误——落盘一旦失败，操作者只会看到
// 永远停在 downloading，连失败都看不到（R84-6）。
func (s *Store) fail(m Meta, cause error) error {
	m.ImportState = StateFailed
	m.LastError = cause.Error()
	if err := s.setMeta(m); err != nil {
		return fmt.Errorf("%w（失败状态落盘失败：%v）", cause, err)
	}
	s.emitState(m.Name, m.Type, StateFailed)
	return cause
}

func (s *Store) downloadFile(ctx context.Context, opts DownloadOptions) (Meta, error) {
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: defaultDownloadTimeout}
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
			// 中断原因要能照着做（R84-6）：说清断在哪、断点还在、怎么续——服务端限速/停发
			// 时最常见的失败是「等到客户端整体超时」，与网络断开区分开。
			if isTimeoutErr(rerr) {
				return Meta{}, fmt.Errorf("拉取中断（已下载 %d 字节，可重试续传）：等待服务端数据超时（HTTP 客户端整体超时触发）；"+
					"已下载部分保留在 .part，重跑同一命令即从断点续传: %w", written, rerr)
			}
			return Meta{}, fmt.Errorf("拉取中断（已下载 %d 字节，可重试续传）：连接中断；"+
				"已下载部分保留在 .part，重跑同一命令即从断点续传: %w", written, rerr)
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
	// 进度字段在完成时等于镜像大小（`show images <名> detail` 与 REST 同一视图）。
	return Meta{Name: opts.Name, Type: opts.Type, SizeBytes: written,
		DownloadedBytes: written, TotalBytes: written,
		SHA256: sum, Format: formatOf(opts.Name), Description: opts.Description,
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

// isTimeoutErr 判定「超时」类错误。注意 http.Client 的整体超时错误实现了 net.Error 的
// Timeout()（os.IsTimeout 据此判定），但**不** Unwrap 到 context.DeadlineExceeded，
// 故两条判据都要看。
func isTimeoutErr(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err)
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

// ValidateDownloadOptions 校验 URL 拉取参数（同步可调用）。
//
// 抽出为独立函数的原因：URL 拉取是**异步**的（先返回 202/受理，进度经 import_state 观察），
// 若只在 Download 内校验，缺 sha256 会被当成"受理成功"、随后静默转 failed——
// 调用方须在受理前同步校验并立刻报错（FR-SEC-004，决策 #71⑤）。
func ValidateDownloadOptions(opts DownloadOptions) error {
	if strings.TrimSpace(opts.Name) == "" || strings.TrimSpace(opts.URL) == "" {
		return fmt.Errorf("name 与 url 必填")
	}
	if opts.Type != TypeVM && opts.Type != TypeContainer {
		return fmt.Errorf("type 必须为 %s 或 %s", TypeVM, TypeContainer)
	}
	// FR-SEC-004：URL 拉取**默认强制** sha256，缺省即拒绝（不再静默跳过校验）。
	if strings.TrimSpace(opts.SHA256) == "" {
		return fmt.Errorf("URL 拉取必须提供 sha256（默认强制校验）")
	}
	if len(strings.TrimSpace(opts.SHA256)) != 64 || !isHex(opts.SHA256) {
		return fmt.Errorf("sha256 必须为 64 位十六进制字符串: %q", opts.SHA256)
	}
	return nil
}
