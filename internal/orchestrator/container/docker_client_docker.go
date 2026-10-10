package container

// Docker Engine API 薄适配层（unix socket + HTTP）。文件名 _docker.go → 覆盖率排除，
// 由 nfvis-vm 集成测试覆盖。字段以 Docker Engine API 29.x 为准（实测版本见 M4-P0 记录）。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type dockerClient struct {
	http   *http.Client
	base   string
	socket string // unix socket 路径（shell 的裸流 hijack 需要自己 dial，决策 #358）
}

// NewDockerClient 连接 Docker Engine API（缺省 /var/run/docker.sock）。
func NewDockerClient(socket string) dockerAPI {
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &dockerClient{http: &http.Client{Transport: tr}, base: "http://docker", socket: socket}
}

// NewConnectedProvider 装配生产 Provider（真实 Docker 客户端）。
func NewConnectedProvider(cfg Config) *Provider {
	return NewProvider(cfg, NewDockerClient(cfg.Socket))
}

type dockerDevice struct {
	PathOnHost        string `json:"PathOnHost"`
	PathInContainer   string `json:"PathInContainer"`
	CgroupPermissions string `json:"CgroupPermissions"`
}

type dockerHostConfig struct {
	Binds         []string       `json:"Binds,omitempty"`
	Memory        int64          `json:"Memory,omitempty"`
	NanoCpus      int64          `json:"NanoCpus,omitempty"`
	RestartPolicy dockerRestart  `json:"RestartPolicy"`
	Privileged    bool           `json:"Privileged,omitempty"`
	CapAdd        []string       `json:"CapAdd,omitempty"`
	Devices       []dockerDevice `json:"Devices,omitempty"`
	NetworkMode   string         `json:"NetworkMode,omitempty"`
	ExtraHosts    []string       `json:"ExtraHosts,omitempty"`
}

type dockerRestart struct {
	Name string `json:"Name"`
}

type dockerCreateBody struct {
	Image      string           `json:"Image"`
	Entrypoint []string         `json:"Entrypoint,omitempty"`
	Cmd        []string         `json:"Cmd,omitempty"`
	Env        []string         `json:"Env,omitempty"`
	HostConfig dockerHostConfig `json:"HostConfig"`
}

func (c *dockerClient) do(ctx context.Context, method, path string, body any, out any) error {
	// 决策 #396：底座层给**每次调用**硬上界（不再按调用点选择性包裹）。调用方 ctx 无
	// deadline（CLI 路径的 context.Background()）时套 dockerCallTimeout；调用方给了
	// deadline（长操作，如镜像 load）则以其为准放宽。这样 State/start/stop/restart/
	// remove/镜像 remove 等所有经 do 的调用都不再因 dockerd 假死而无界挂起。
	ctx, cancel := c.boundedCtx(ctx)
	defer cancel()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// 决策 #375（R142 B8）：底座不可达/无响应（dial 失败、连接被拒、deadline 等）要能被上层
		// 识别为「Docker 不可用」并映射 503——带上包级 sentinel errDockerUnavailable（同时用
		// 第二个 %w 保留原始错误因果与文本，便于排障）。**调用方取消**（ctx canceled）不算底座
		// 故障，原样透传（不是 503）。
		if errors.Is(err, context.Canceled) {
			return fmt.Errorf("docker %s %s: %w", method, path, err)
		}
		return fmt.Errorf("docker %s %s: %w", method, path, dockerUnavailableError{err})
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		if resp.StatusCode == http.StatusNotFound {
			return errDockerNotFound
		}
		return fmt.Errorf("docker %s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

var errDockerNotFound = fmt.Errorf("docker: not found")

// errDockerUnavailable Docker 底座**不可达/无响应**（决策 #375，R142 B8）：连接被拒、dial
// 失败、请求超时等。包级 sentinel，由 Provider 归一为 orchestrator.ErrContainerUnavailable
// （API 层映射 503 UNAVAILABLE）。与 errDockerNotFound（404 竞态 ⇒ ErrVMNotFound）区分开。
//
// 用类型 dockerUnavailableError 承载（而非直接 `%w` 包 sentinel）：sentinel 文案不进**用户
// 可见消息**（Error 只回原始错误文本），而 `errors.Is(err, errDockerUnavailable)` 仍成立。
var errDockerUnavailable = errors.New("docker 底座不可达")

// dockerUnavailableError 标记「底座不可达」并保留原始错误因果；Error 只回原始文本。
type dockerUnavailableError struct{ err error }

func (e dockerUnavailableError) Error() string        { return e.err.Error() }
func (e dockerUnavailableError) Unwrap() error        { return e.err }
func (e dockerUnavailableError) Is(target error) bool { return target == errDockerUnavailable }

func (c *dockerClient) Create(ctx context.Context, name string, spec CreateSpec) error {
	body := dockerCreateBody{
		Image:      spec.Image,
		Entrypoint: spec.Entrypoint,
		Cmd:        spec.Cmd,
		Env:        spec.Env,
		HostConfig: dockerHostConfig{
			Binds:         spec.Binds,
			Memory:        spec.MemoryBytes,
			NanoCpus:      spec.NanoCPUs,
			RestartPolicy: dockerRestart{Name: spec.RestartPolicy},
			Privileged:    spec.Privileged,
			CapAdd:        spec.CapAdd,
			NetworkMode:   "none", // 网络经 memif 接入 VPP，不用 Docker 默认网桥
		},
	}
	for _, d := range spec.Devices {
		body.HostConfig.Devices = append(body.HostConfig.Devices,
			dockerDevice{PathOnHost: d.PathOnHost, PathInContainer: d.PathInContainer, CgroupPermissions: d.CgroupPermissions})
	}
	return c.do(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(name), body, nil)
}

func (c *dockerClient) Start(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/start", nil, nil)
}

func (c *dockerClient) Stop(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/stop?t=10", nil, nil)
}

func (c *dockerClient) Restart(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/restart?t=10", nil, nil)
}

func (c *dockerClient) Remove(ctx context.Context, name string, force bool) error {
	return c.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(name)+"?force="+strconv.FormatBool(force)+"&v=1", nil, nil)
}

// RemoveImage 删除容器镜像（DELETE /images/<ref>）。
//
// ref 为镜像仓库目录项名，先经 DockerRefFor 归一为 Docker 侧引用（决策 #447），与 LoadImage
// 落成的引用**严格对称**：含冒号原样删、不含冒号删 `<名>:latest`。此前按目录项名原样删，
// 与载入侧重打出来的引用不对称（删不到产品打的那个 tag）。
func (c *dockerClient) RemoveImage(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodDelete, "/images/"+url.PathEscape(DockerRefFor(ref)), nil, nil)
}

// LoadImage 载入容器镜像归档（POST /images/load，body 为 docker save 的 tar；
// 不复用 do()：其 body 走 JSON 序列化，无法流式传 tar）。载入后把 Docker 侧引用对齐到
// 目录项名的推导引用（DockerRefFor，决策 #160/#447）：目录项名**允许含冒号**，`alpine:3.20`
// 本身就是合法 Docker 引用（原样、优先于 #160 的补全口径）；归档载入的引用与推导引用一致时
// **不做任何 tag 调用**（不抢占用户 Docker 里同名的既有 tag），不一致才按 repo/tag 重打，
// 使 `docker create` 按目录项名解析得到镜像。
func (c *dockerClient) LoadImage(ctx context.Context, path, name string) error {
	// 决策 #396：镜像载入是**长操作**（大 tar），不经 do（流式 body）。上界按调用方 ctx
	// 放宽——调用方（镜像导入路径）给宽松 deadline；若调用方未给（Background），仍套缺省
	// 上界兜底（不再无界挂起）。真正的宽松上界由调用方给出（见 cmd/nfvisd/main.go）。
	ctx, cancel := c.boundedCtx(ctx)
	defer cancel()
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开镜像归档 %s: %w", path, err)
	}
	defer f.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/images/load?quiet=1", f)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := c.http.Do(req)
	if err != nil {
		// 与 do 同口径（决策 #375/#396）：底座不可达/无响应（含内部硬上界到期）标记为
		// errDockerUnavailable 供上层映射 503；调用方取消原样透传。
		if errors.Is(err, context.Canceled) {
			return fmt.Errorf("docker image load: %w", err)
		}
		return fmt.Errorf("docker image load: %w", dockerUnavailableError{err})
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("docker image load: %d %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	loaded, err := loadedTagOf(resp.Body)
	if err != nil {
		return err
	}
	if loaded == "" {
		return fmt.Errorf("docker image load: 归档中未解析到镜像 tag，无法按 %q 重打标签", name)
	}
	// 引用对齐（决策 #447）：目录项名含冒号时它就是 Docker 引用本身，归档载入的引用与之一致
	// 即无须任何 tag 调用（不抢占用户既有 tag）；不一致才按 repo/tag 重打（tag 缺省 latest）。
	ref := DockerRefFor(name)
	if loaded == ref {
		return nil
	}
	repo, tag := splitDockerRef(ref)
	if tag == "" {
		tag = "latest"
	}
	return c.tag(ctx, loaded, repo, tag)
}

// loadedTagOf 从 docker load 的响应流（NDJSON）解析载入的镜像引用：
// 行形如 {"stream":"Loaded image: alpine:3.20\n"}；未打 tag 的归档为
// "Loaded image ID: sha256:…"（ID 同样可作 tag 的引用源）。
func loadedTagOf(r io.Reader) (string, error) {
	dec := json.NewDecoder(r)
	for dec.More() {
		var line struct {
			Stream string `json:"stream"`
		}
		if err := dec.Decode(&line); err != nil {
			return "", fmt.Errorf("docker image load 响应解析: %w", err)
		}
		for _, p := range []string{"Loaded image: ", "Loaded image ID: "} {
			if s := strings.TrimSpace(line.Stream); strings.HasPrefix(s, p) {
				return strings.TrimSpace(strings.TrimPrefix(s, p)), nil
			}
		}
	}
	return "", nil
}

// tag 给既有镜像引用打标签（POST /images/<ref>/tag）。
func (c *dockerClient) tag(ctx context.Context, ref, repo, tag string) error {
	q := url.Values{"repo": {repo}, "tag": {tag}}
	return c.do(ctx, http.MethodPost, "/images/"+url.PathEscape(ref)+"/tag?"+q.Encode(), nil, nil)
}

// State 返回契约枚举；不存在 exists=false。
func (c *dockerClient) State(ctx context.Context, name string) (string, bool, error) {
	facts, exists, err := c.Inspect(ctx, name)
	if err != nil || !exists {
		return "", exists, err
	}
	return facts.State, true, nil
}

// Inspect 单次 GET /containers/{name}/json 取回契约状态 + 原始状态 + 重启次数（决策 #432）。
//
// 重启次数与状态**同一次应答**取回；用指针接收，应答里没有该字段时 RestartsKnown=false
// （「取不到」与「0 次」必须分得开——规格 #432：取不到就不给该字段）。
//
// 字段位置（真机实测，Docker 29.1.3）：`RestartCount` 在 inspect 应答的**顶层**，
// **不在** `State` 里（`State` 只有 Dead/Error/ExitCode/FinishedAt/OOMKilled/Paused/Pid/
// Restarting/Running/StartedAt/Status）。首版按 `State.RestartCount` 读，于是永远「取不到」——
// 读视图省略该字段、重启循环告警也不触发（真机 `docker inspect <名> --format '{{.RestartCount}}'`
// = 10，而 `.State.RestartCount` 报 `map has no entry for key`）。故以顶层为准，`State` 内那份
// 仅作兼容回退（同样是指针，缺省不影响判定）。
func (c *dockerClient) Inspect(ctx context.Context, name string) (ContainerFacts, bool, error) {
	var out struct {
		RestartCount *int `json:"RestartCount"`
		State        struct {
			Status       string `json:"Status"`
			RestartCount *int   `json:"RestartCount"`
		} `json:"State"`
	}
	err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil, &out)
	if err == errDockerNotFound {
		return ContainerFacts{}, false, nil
	}
	if err != nil {
		return ContainerFacts{}, false, err
	}
	facts := ContainerFacts{State: dockerStateToContract(out.State.Status), RawState: out.State.Status}
	switch {
	case out.RestartCount != nil:
		facts.RestartCount, facts.RestartsKnown = *out.RestartCount, true
	case out.State.RestartCount != nil:
		facts.RestartCount, facts.RestartsKnown = *out.State.RestartCount, true
	}
	return facts, true, nil
}

// ExitCode 返回容器退出码（State.ExitCode）。
func (c *dockerClient) ExitCode(ctx context.Context, name string) (int, bool, error) {
	var out struct {
		State struct {
			ExitCode int `json:"ExitCode"`
		} `json:"State"`
	}
	err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil, &out)
	if err == errDockerNotFound {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return out.State.ExitCode, true, nil
}

// ContainerPID 容器主进程 PID（容器 vNIC 接入用，决策 #441）。
//
// 字段位置按 #432 的真机教训核对：`Pid` 在 inspect 应答的 **State** 里（与 RestartCount 不同
// ——那个在顶层）；未运行的容器该值为 0，调用方据此如实报错、不猜。
func (c *dockerClient) ContainerPID(ctx context.Context, name string) (int, error) {
	var out struct {
		State struct {
			Pid int `json:"Pid"`
		} `json:"State"`
	}
	err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil, &out)
	if err != nil {
		return 0, err
	}
	return out.State.Pid, nil
}

// OOMKilled 返回容器是否因内存超限被终止（State.OOMKilled）。
func (c *dockerClient) OOMKilled(ctx context.Context, name string) (bool, bool, error) {
	var out struct {
		State struct {
			OOMKilled bool `json:"OOMKilled"`
		} `json:"State"`
	}
	err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil, &out)
	if err == errDockerNotFound {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return out.State.OOMKilled, true, nil
}

func (c *dockerClient) Logs(ctx context.Context, name string, tail int) (string, error) {
	// 决策 #396：日志读取不经 do（流式响应），内部同样加硬上界——dockerd 假死时
	// `request container-functions <n> log` 与 `show container-functions` 不再无界挂起。
	ctx, cancel := c.boundedCtx(ctx)
	defer cancel()
	path := "/containers/" + url.PathEscape(name) + "/logs?stdout=1&stderr=1&tail=" + strconv.Itoa(tail)
	var sb strings.Builder
	// 日志为流式（带 8 字节头或 TTY 原始）；此处直接取文本并清理帧头。
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// 与 do 同口径（决策 #375）：底座不可达/无响应（含内部硬上界到期）标记为
		// errDockerUnavailable 供上层映射 503；调用方取消原样透传。
		if errors.Is(err, context.Canceled) {
			return "", fmt.Errorf("docker logs: %w", err)
		}
		return "", fmt.Errorf("docker logs: %w", dockerUnavailableError{err})
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("docker logs: %d %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	sb.Write(stripDockerLogFrames(data))
	return sb.String(), nil
}

// stripDockerLogFrames 去掉 Docker 流式日志的 8 字节帧头（非 TTY 模式）。
func stripDockerLogFrames(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		if i+8 <= len(b) && (b[i] == 1 || b[i] == 2) && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 0 {
			n := int(b[i+4])<<24 | int(b[i+5])<<16 | int(b[i+6])<<8 | int(b[i+7])
			i += 8
			if i+n > len(b) {
				n = len(b) - i
			}
			out = append(out, b[i:i+n]...)
			i += n
			continue
		}
		out = append(out, b[i])
		i++
	}
	return out
}

// Exec 在容器内执行命令（决策 #357，非 TTY）。
//
// 三步（Docker Engine API）：① `POST /containers/{name}/exec` 建 exec 实例；
// ② `POST /exec/{id}/start` 拿**多路复用流**（8 字节头，见 exec.go 的 demuxDockerStream）；
// ③ `GET /exec/{id}/json` 读退出码。
//
// 超时语义（如实）：timeout 只界定**客户端等待**——超时返回 TimedOut=true、不带退出码
// （容器内进程可能仍在运行；Docker 不提供 exec 进程的中止接口），**不**当成错误。
func (c *dockerClient) Exec(ctx context.Context, name, command string, timeout time.Duration) (ExecResult, error) {
	started := time.Now()
	var created struct {
		ID string `json:"Id"`
	}
	body := map[string]any{
		"AttachStdout": true,
		"AttachStderr": true,
		"Tty":          false,
		"Cmd":          []string{"/bin/sh", "-c", command},
	}
	callCtx, cancel := c.boundedCtx(ctx)
	defer cancel()
	if err := c.do(callCtx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/exec", body, &created); err != nil {
		return ExecResult{}, wrapDockerTimeout(err)
	}
	if created.ID == "" {
		return ExecResult{}, fmt.Errorf("docker exec: 未返回 exec 实例 id")
	}

	// 等待窗口与「调用方 ctx」分离：超时是我们**预期**的一种结果（如实上报），不是异常。
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(wctx, http.MethodPost,
		c.base+"/exec/"+url.PathEscape(created.ID)+"/start",
		strings.NewReader(`{"Detach":false,"Tty":false}`))
	if err != nil {
		return ExecResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// 决策 #375（R142 B7）：**只有等待窗口到期**才算超时；wctx 派生自调用方 ctx，故调用方
		// 取消（客户端断开/上层取消）此前被一律谎报成 TimedOut。收紧为 DeadlineExceeded——
		// 取消/其它失败按错误透传（不设 TimedOut）。
		if errors.Is(wctx.Err(), context.DeadlineExceeded) {
			return ExecResult{TimedOut: true, Duration: time.Since(started)}, nil
		}
		return ExecResult{}, fmt.Errorf("docker exec start: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return ExecResult{}, fmt.Errorf("docker exec start: %d %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	stdout, stderr, truncated, derr := demuxDockerStream(resp.Body, ExecMaxStreamBytes)
	res := ExecResult{
		Stdout:    string(stdout),
		Stderr:    string(stderr),
		Truncated: truncated,
	}
	if derr != nil {
		// 流中断：只有**等待窗口到期**才是超时（已读到的部分保留）；调用方取消/其它失败按错误
		// 透传（决策 #375，R142 B7）——已捕获的 stdout/stderr 仍保留在返回的 ExecResult 里。
		if errors.Is(wctx.Err(), context.DeadlineExceeded) {
			res.TimedOut = true
			res.Duration = time.Since(started)
			return res, nil
		}
		if errors.Is(wctx.Err(), context.Canceled) {
			res.Duration = time.Since(started)
			return res, fmt.Errorf("docker exec 读取输出: %w", wctx.Err())
		}
		return res, fmt.Errorf("docker exec 读取输出: %w", derr)
	}

	var inspect struct {
		ExitCode int `json:"ExitCode"`
	}
	if err := c.do(ctx, http.MethodGet, "/exec/"+url.PathEscape(created.ID)+"/json", nil, &inspect); err != nil {
		// 决策 #370（R142 B9）：输出已完整捕获，只有退出码读不到——不再整体按失败丢弃
		// （部分成功被当整体失败）。如实：HasExitCode=false + note，stdout/stderr 保留。
		res.ExitCodeNote = fmt.Sprintf("退出码未能读取（输出已保留）: %v", err)
		res.Duration = time.Since(started)
		return res, nil
	}
	res.ExitCode = inspect.ExitCode
	res.HasExitCode = true
	res.Duration = time.Since(started)
	return res, nil
}

// hijackedStream Docker exec TTY 的**全双工**裸流（决策 #358）。
//
// 读走 bufio.Reader（**不是** http.Response.Body）：升级响应的头之后可能紧跟同包到达的
// 首批数据（首屏提示符），自己拿 reader 才不会丢。写走同一连接的裸 conn。
//
// 为什么不用 http.ReadResponse 拿 body：真机实测（round139 pty）Go 对 101 的 body 语义
// 与本用途不合——`resp.Body` 读起来立刻 EOF ⇒ 桥接两侧马上都结束，表现为「会话刚建立就
// 断开」。手工解析状态行 + 头（读掉空行即止）后两端都用同一连接，行为与 Docker 语义一致。
type hijackedStream struct {
	conn net.Conn
	r    *bufio.Reader
}

func (h *hijackedStream) Read(p []byte) (int, error)  { return h.r.Read(p) }
func (h *hijackedStream) Write(p []byte) (int, error) { return h.conn.Write(p) }
func (h *hijackedStream) Close() error                { return h.conn.Close() }

// ExecShell 打开容器内的**交互式 TTY**（决策 #358）：建 TTY exec → 裸 unix conn 手写带
// `Connection: Upgrade` / `Upgrade: tcp` 的 start 请求 → 期望 `101 UPGRADED` → 返回全双工流。
//
// 真机 spike 实证（round139）：升级后写入 `echo SHELL-OK; id -u` 能读到 shell 回显；
// 这正是「不用 WebSocket 也能双向」的那条路——与 CLI/Web 的 WS 桥接在 API 层相接。
//
// 决策 #366（R142-12）：拨号后的**整段握手**（写请求 → 状态行 → 头 → 分流）有硬上界
// shellHandshakeTimeout——此前状态行 `ReadString('\n')`、非 101 错误体的 `ReadByte` 循环
// （无锚点）、响应头循环三处都可能因 dockerd 假死永久挂起。握手成功即清 deadline
// （交互流空闲是常态，不得带握手限）。
func (c *dockerClient) ExecShell(ctx context.Context, name string) (io.ReadWriteCloser, error) {
	var created struct {
		ID string `json:"Id"`
	}
	body := map[string]any{
		"AttachStdin":  true,
		"AttachStdout": true,
		"AttachStderr": true,
		"Tty":          true,
		"Cmd":          []string{"/bin/sh"},
	}
	callCtx, cancel := c.boundedCtx(ctx)
	defer cancel()
	if err := c.do(callCtx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/exec", body, &created); err != nil {
		return nil, wrapDockerTimeout(err)
	}
	if created.ID == "" {
		return nil, fmt.Errorf("docker exec shell: 未返回 exec 实例 id")
	}

	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "unix", c.socket)
	if err != nil {
		// 决策 #375（R142 B8）：拨号失败是「底座不可达」，带上标记供上层映射 503
		// （保留原始错误因果/文本）。Provider 归一里对调用方取消优先透传，故此处无需区分。
		return nil, fmt.Errorf("连接 Docker socket: %w", dockerUnavailableError{err})
	}
	return execShellHandshake(conn, created.ID)
}

// shellHandshakeTimeout exec shell 握手段（写请求 → 读状态行/头 → 分流）的硬上界
// （决策 #366，R142-12）。包级 var 仅为测试可注入更小值；生产代码不得改写。
var shellHandshakeTimeout = 10 * time.Second

// dockerCallTimeout Docker API **单次调用**的缺省硬上界（决策 #366 起用于 State 检查 /
// exec·shell 的 create；决策 #396 起为**客户端内部**所有调用（State/Logs/start/stop/
// restart/remove/镜像 remove/load…，经 boundedCtx）的缺省上界）。真机 SIGSTOP 实测：
// dockerd 冻结时这些调用无超时、挂满整个观察窗口，是「dockerd 挂死即挂住」的真实挂点。
// 到期取消请求：未被读走的请求随连接关闭被 dockerd 丢弃，不留「底座恢复后幽灵执行」。
// 调用方自带更长 deadline 时按调用方放宽（长操作，如镜像 load——见 boundedCtx）。
// 包级 var 仅为测试可注入。
var dockerCallTimeout = 10 * time.Second

// wrapDockerTimeout 底座无响应的报错要**可照做**：裸 `context deadline exceeded`
// 不说人话，包装成「Docker 未响应」并保留原错误。
func wrapDockerTimeout(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("Docker 未在 %s 内响应（已中止等待；请确认 docker 服务状态）", dockerCallTimeout)
	}
	return err
}

// boundedCtx 给单次 Docker API 调用加**客户端内部硬上界**（决策 #396）。
//
// 调用方 ctx 已带 deadline 时以其为准（长操作——如镜像 load——由调用方给宽松上界，
// 不被 10s 缺省截断）；无 deadline 时套 dockerCallTimeout。这是「不再按调用点选择性
// 包裹」的落点：State/Logs/start/stop/restart/remove/镜像 remove/load 等一律经此有界。
// 返回的 cancel 必须调用。
func (c *dockerClient) boundedCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, dockerCallTimeout)
}

// execShellHandshake 拨号后的握手段（可测，决策 #366）：
//   - 开头给 conn 设 shellHandshakeTimeout deadline——三处曾经无界的读（状态行、
//     非 101 错误体、响应头）都由它兜底；
//   - 非 101：按行读头解析 Content-Length（缺省/非法按 0），按锚点读 min(CL, 8 KiB)
//     正文作错误摘录（不再是无锚点的 ReadByte 循环）；
//   - 101：读完头**清 deadline** 再返回流（TTY 交互空闲是常态）。
//
// 任何失败路径都关 conn。
func execShellHandshake(conn net.Conn, execID string) (io.ReadWriteCloser, error) {
	_ = conn.SetDeadline(time.Now().Add(shellHandshakeTimeout))
	startBody := `{"Detach":false,"Tty":true}`
	req := fmt.Sprintf("POST /exec/%s/start HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\n"+
		"Connection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: %d\r\n\r\n%s",
		url.PathEscape(execID), len(startBody), startBody)
	if _, err := conn.Write([]byte(req)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("docker exec shell 请求: %w", err)
	}
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("docker exec shell 响应: %w", err)
	}
	if !strings.Contains(statusLine, " 101 ") {
		// 非升级响应：把可读到的正文带上（Docker 的报错在正文里），便于定位。
		body := readUpgradeRejectBody(br)
		_ = conn.Close()
		return nil, fmt.Errorf("docker exec shell: 未升级为裸流（%s：%s）",
			strings.TrimSpace(statusLine), strings.TrimSpace(body))
	}
	for { // 读掉响应头（空行即止；其后即 TTY 字节流）
		line, lerr := br.ReadString('\n')
		if lerr != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("docker exec shell 读取响应头: %w", lerr)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	// 决策 #366：握手完成即清 deadline——返回的流是交互 TTY，空闲等待是常态，
	// 绝不能带着握手限去读（否则用户停在提示符 shellHandshakeTimeout 后必断）。
	_ = conn.SetDeadline(time.Time{})
	return &hijackedStream{conn: conn, r: br}, nil
}

// readUpgradeRejectBody 非 101 响应的错误正文摘录：按行读头解析 Content-Length
// （缺省/非法按 0 ⇒ 不读正文），按锚点读 min(ContentLength, 8 KiB)。读取都受握手
// deadline 兜底；连接提前关闭时读到多少算多少。
func readUpgradeRejectBody(br *bufio.Reader) string {
	cl := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(strings.ToLower(line), "content-length:"); ok {
			if n, perr := strconv.Atoi(strings.TrimSpace(v)); perr == nil && n > 0 {
				cl = n
			}
		}
	}
	if cl > 8<<10 {
		cl = 8 << 10
	}
	rest := make([]byte, 0, min(cl, 512))
	for len(rest) < cl {
		b, rerr := br.ReadByte()
		if rerr != nil {
			break
		}
		rest = append(rest, b)
	}
	return string(rest)
}
