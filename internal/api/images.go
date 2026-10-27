package api

// M4-8：镜像仓库 API（FR-CMP-030~033）。
//
// POST /images 二选一：JSON {url,sha256?} 异步拉取；JSON {incoming_file} 从 /data/incoming 导入。
// 删除前校验引用（VNF/容器），被引用返回 409（FR-CMP-033）。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"

	"github.com/xzjt/nfvis/internal/images"
	"github.com/xzjt/nfvis/internal/model"
)

// ImagesRuntime 镜像仓库能力（*images.Store 注入；nil = 503）。
type ImagesRuntime interface {
	List() []images.Meta
	Get(name string) (images.Meta, bool)
	Download(ctx context.Context, opts images.DownloadOptions) (images.Meta, error)
	ImportIncoming(name, typ, incomingFile, description string) (images.Meta, error)
	Delete(name string, refCount int) error
}

// imageUploadStaging 可选能力：能把上传字节流暂存到仓库 incoming 目录内的安全临时文件
// （*images.Store 实现，见 internal/images/upload.go）。
//
// 用**可选接口**而不是给 ImagesRuntime 加方法：ImagesRuntime 是 api 包对镜像仓库的能力抽象，
// 测试替身只实现被测路径用到的那几个方法，加方法会强制所有替身跟着改；生产注入的恒是
// *images.Store（cmd/nfvisd/main.go），故这条路在生产恒可用。同 #15 vppRestartWarnings 的既有先例。
type imageUploadStaging interface {
	StageUpload(src io.Reader, maxBytes int64) (string, int64, error)
}

// imageConfigSource 可选能力：注入 committed 配置读口，供**发布前**引用复核（决策 #514）。
// 参数用匿名接口，与 *images.Store.SetConfigSource 的签名逐字一致（接口类型是结构化的，
// 跨包也相等）。与 imageUploadStaging 同为可选断言：测试替身不要求实现（生产注入恒是
// *images.Store，见 cmd/nfvisd/main.go）。
type imageConfigSource interface {
	SetConfigSource(src interface{ Committed() (model.Config, error) })
}

const (
	// multipartMemoryLimit 解析 multipart 时**驻留内存**的上限：超过阈值的部分由 Go 溢写到
	// 系统临时目录（不是 incoming 目录），再由 handlePostImageMultipart 复制进 incoming 内的
	// 安全临时文件；溢写部分在请求收尾时清掉（handler 显式 RemoveAll）。
	// 与 POST /images 的 JSON 路径（decodeBody 限 4 MiB）不同：上传镜像动辄上百 MB，内存阈值只控
	// 「多少驻留内存」，真正的总上限是 maxImageUploadBytes。
	multipartMemoryLimit = 64 << 20
)

// maxImageUploadBytes POST /images 的 multipart 上传**请求体总上限**。
//
// 4 GiB 的由来（给得出依据，不用拍脑袋的数）：
//   - 本产品会遇到的镜像都远小于它——仓库里最大的真实产物是 178 MB 的 alpine.qcow2（alpine 云镜像）、
//     ~600 MB 的 Debian/Ubuntu 云镜像、186 MB 的离线安装包 .run；4 GiB 给 >6 倍余量；
//   - 同时它给「一次上传最多能吃掉多少磁盘」一个明确上界：超限在读请求体时就中断
//     （http.MaxBytesReader），既不会把超量数据写进 incoming 目录、也不会堆进内存。
//
// 变量而非常量：测试要能在小体量上验证超限路径（见 images_multipart_round49_test.go）。
// 生产侧不做调整（没有对外配置项，避免把「上限」变成需要运维理解的新旋钮）。
var maxImageUploadBytes int64 = 4 << 30

// imageResponse 镜像详情（元数据 + 引用计数，契约 Image）。
type imageResponse struct {
	images.Meta
	RefCount int `json:"ref_count"`
}

func (s *Server) requireImages(w http.ResponseWriter) bool {
	if s.images == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "镜像仓库未接入", nil)
		return false
	}
	return true
}

// handleListImages GET /api/v1/images
func (s *Server) handleListImages(w http.ResponseWriter, r *http.Request) {
	if !s.requireImages(w) {
		return
	}
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	metas := s.images.List()
	out := make([]imageResponse, 0, len(metas))
	for _, m := range metas {
		out = append(out, imageResponse{Meta: m, RefCount: images.RefCount(cfg, m.Name)})
	}
	writeJSON(w, http.StatusOK, paginate(r, out))
}

// handleGetImage GET /api/v1/images/{name}
func (s *Server) handleGetImage(w http.ResponseWriter, r *http.Request) {
	if !s.requireImages(w) {
		return
	}
	name := r.PathValue("name")
	m, ok := s.images.Get(name)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("镜像 %s 不存在", name), nil)
		return
	}
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, imageResponse{Meta: m, RefCount: images.RefCount(cfg, name)})
}

// handleGetVMImageProfiles GET /api/v1/vm-image-profiles（决策 #485）：镜像级启动画像列表。
// 读 committed 配置——与 CLI `show vm-image-profiles` 同源（同一份配置事实，不另存一份）。
// 空集合发 `[]`（不是 null）；分页参数与其余列表端点同口径（缺省不截断）。
func (s *Server) handleGetVMImageProfiles(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	out := make([]model.VMImageProfile, 0, len(cfg.VMImageProfiles))
	out = append(out, cfg.VMImageProfiles...)
	writeJSON(w, http.StatusOK, paginate(r, out))
}

// imagePublishPrecheck 受理前的同步发布前复核（决策 #514）：同名镜像被引用时返回 ErrReferenced。
// 仓库未实现该可选能力（测试替身）时返回 nil（不阻断）。
func imagePublishPrecheck(rt ImagesRuntime, name string) error {
	if c, ok := rt.(interface{ CheckPublishAllowed(string) error }); ok {
		return c.CheckPublishAllowed(name)
	}
	return nil
}

// handlePostImage POST /api/v1/images（multipart 上传 或 URL 拉取 或 incoming 导入）
func (s *Server) handlePostImage(w http.ResponseWriter, r *http.Request) {
	if !s.requireImages(w) {
		return
	}
	// 契约三选一里的 multipart/form-data 上传（Web 控制台「上传文件并导入」）。
	if isMultipartForm(r) {
		s.handlePostImageMultipart(w, r)
		return
	}
	var in struct {
		Name         string `json:"name"`
		Type         string `json:"type"`
		URL          string `json:"url"`
		SHA256       string `json:"sha256"`
		Description  string `json:"description"`
		IncomingFile string `json:"incoming_file"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if in.Name == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "name 必填", nil)
		return
	}
	if (in.URL == "") == (in.IncomingFile == "") {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "url 与 incoming_file 必须且只能指定其一", nil)
		return
	}
	if in.IncomingFile != "" {
		m, err := s.images.ImportIncoming(in.Name, in.Type, in.IncomingFile, in.Description)
		if err != nil {
			writeImageImportError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, m)
		return
	}
	// URL 拉取为异步（进度经 GET /images/{name} 的 import_state 观察；事件流随 M5）。
	// 但参数校验必须**同步**先做：否则缺 sha256 会被当成"受理成功"再静默转 failed
	// （FR-SEC-004 默认强制校验，决策 #71⑤）。
	dlOpts := images.DownloadOptions{Name: in.Name, Type: in.Type, URL: in.URL, SHA256: in.SHA256, Description: in.Description}
	if err := images.ValidateDownloadOptions(dlOpts); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	// 受理前同步复核（决策 #514）：同名镜像已被引用时立刻 409，别先回 202「已受理」再静默失败。
	if err := imagePublishPrecheck(s.images, in.Name); err != nil {
		writeImageImportError(w, err)
		return
	}
	go func(opts images.DownloadOptions) {
		_, _ = s.images.Download(context.Background(), opts)
	}(dlOpts)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "downloading", "name": in.Name})
}

// isMultipartForm 判定请求是不是 multipart/form-data（契约里 POST /images 的上传形态）。
// Content-Type 缺失或解析不出来一律按 JSON 处理——既有 JSON 路径（URL 拉取 / incoming 导入）的
// 行为逐字不变（回归）。
func isMultipartForm(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return mt == "multipart/form-data"
}

// handlePostImageMultipart POST /images 的 multipart/form-data 上传形态（Web 控制台「上传文件并导入」）。
//
// 校验顺序与 JSON 路径**同源**：name 必填 → type 合法 → 名字经 images.ValidateImageName
// （A 组的**同一函数**，不另写一套）→ file 必填；全部通过之后才落临时文件，
// 故越界名不会在 incoming 里留下任何东西。
//
// 文件用**本包生成的随机临时名**落在 incoming 目录内（images.StageUpload），用户提供的 name 只当
// 镜像名、绝不当路径用；失败路径一律清理临时文件。
func (s *Server) handlePostImageMultipart(w http.ResponseWriter, r *http.Request) {
	// 请求体层面的总上限：超限在读的时候就中断——既不会把超量数据落进 incoming 目录，
	// 也不会让它先在系统临时目录里堆成整份（多 MB/GB 的镜像上传至少两处都要有界）。
	// 镜像上传动辄上百 MB，故这一层只判「总大小」，不做任何内存驻留假设。
	r.Body = http.MaxBytesReader(w, r.Body, maxImageUploadBytes)
	if err := r.ParseMultipartForm(multipartMemoryLimit); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "VALIDATION_FAILED",
				fmt.Sprintf("上传内容超过上限 %d 字节（POST /images 单次上传总量上限）", maxImageUploadBytes), nil)
			return
		}
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "解析 multipart 失败: "+err.Error(), nil)
		return
	}
	// 溢写到系统临时目录的部分在这里清掉（不依赖 net/http 的收尾清理）。
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	// 只读**表单体**（r.PostFormValue 不含 URL query 参数）——与 JSON 路径只读请求体同口径，
	// 免得 `POST /images?name=…` 这种把字段藏在 query 里的写法被采纳。
	name := r.PostFormValue("name")
	typ := r.PostFormValue("type")
	description := r.PostFormValue("description")
	if name == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "name 必填", nil)
		return
	}
	if typ != images.TypeVM && typ != images.TypeContainer {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED",
			fmt.Sprintf("type 必须为 %s 或 %s", images.TypeVM, images.TypeContainer), nil)
		return
	}
	if err := images.ValidateImageName(name, typ); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	src, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "缺少 file 字段（multipart/form-data）", nil)
		return
	}
	defer src.Close()
	up, ok := s.images.(imageUploadStaging)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "镜像仓库未接入上传暂存（无法确定 incoming 目录）", nil)
		return
	}
	tmp, _, err := up.StageUpload(src, maxImageUploadBytes)
	if err != nil {
		// 落盘侧超限与请求体侧同一常量，故正常情况下请求体上限先生效（这里基本不可达）；
		// 仍映射成同一个码，避免两处口径不一致。
		if errors.Is(err, images.ErrUploadTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "VALIDATION_FAILED",
				fmt.Sprintf("上传内容超过上限 %d 字节（POST /images 单次上传总量上限）", maxImageUploadBytes), nil)
			return
		}
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	// 失败路径清掉暂存文件；成功路径已由 ImportIncoming 收走（改名/删除），删不到即无害。
	defer func() { _ = os.Remove(tmp) }()
	m, err := s.images.ImportIncoming(name, typ, tmp, description)
	if err != nil {
		writeImageImportError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

// writeImageImportError 把镜像发布（导入/上传）错误映射到 HTTP（决策 #514）：
// 被引用（ErrReferenced）→ 409 CONFLICT（不是 400/500，与 DELETE 被引用同码）；
// 其余（名字非法、未接入 Docker、incoming 越界…）保持既有 400 形状，文案透传。
func writeImageImportError(w http.ResponseWriter, err error) {
	if errors.Is(err, images.ErrReferenced) {
		writeError(w, http.StatusConflict, "CONFLICT", err.Error(), nil)
		return
	}
	writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
}

// handleDeleteImage DELETE /api/v1/images/{name}（被引用 409，FR-CMP-033）
func (s *Server) handleDeleteImage(w http.ResponseWriter, r *http.Request) {
	if !s.requireImages(w) {
		return
	}
	name := r.PathValue("name")
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	if err := s.images.Delete(name, images.RefCount(cfg, name)); err != nil {
		switch {
		case errors.Is(err, images.ErrInvalidName):
			// round49（镜像名越界收口）：索引里若有历史遗留的越界条目，删除会被仓库层拒绝——
			// 那是**输入**问题（旧数据），回 400 让操作者看得出，而不是 500。
			writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		case errors.Is(err, images.ErrReferenced):
			writeError(w, http.StatusConflict, "CONFLICT", err.Error(), nil)
		case errors.Is(err, images.ErrNotFound):
			writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error(), nil)
		default:
			writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "name": name})
}
