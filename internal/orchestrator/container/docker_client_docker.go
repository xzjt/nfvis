package container

// Docker Engine API 薄适配层（unix socket + HTTP）。文件名 _docker.go → 覆盖率排除，
// 由 nfvis-vm 集成测试覆盖。字段以 Docker Engine API 29.x 为准（实测版本见 M4-P0 记录）。

import (
	"bufio"
	"context"
	"encoding/json"
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
		return fmt.Errorf("docker %s %s: %w", method, path, err)
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
func (c *dockerClient) RemoveImage(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodDelete, "/images/"+url.PathEscape(ref), nil, nil)
}

// LoadImage 载入容器镜像归档（POST /images/load，body 为 docker save 的 tar；
// 不复用 do()：其 body 走 JSON 序列化，无法流式传 tar）。载入后按 name 重打标签
// `<name>:latest`（决策 #160）：tar 内嵌 tag 必含冒号（如 alpine:3.20）而容器引用的
// 是目录项名（白名单禁冒号），不重打标签则 docker create 解析不到镜像。
func (c *dockerClient) LoadImage(ctx context.Context, path, name string) error {
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
		return fmt.Errorf("docker image load: %w", err)
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
	return c.tag(ctx, loaded, name, "latest")
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
	var out struct {
		State struct {
			Status string `json:"Status"`
		} `json:"State"`
	}
	err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil, &out)
	if err == errDockerNotFound {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return dockerStateToContract(out.State.Status), true, nil
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
	path := "/containers/" + url.PathEscape(name) + "/logs?stdout=1&stderr=1&tail=" + strconv.Itoa(tail)
	var sb strings.Builder
	// 日志为流式（带 8 字节头或 TTY 原始）；此处直接取文本并清理帧头。
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
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
	if err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/exec", body, &created); err != nil {
		return ExecResult{}, err
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
		if wctx.Err() != nil {
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
		// 流中断：超时窗口到点 ⇒ 如实报「超时」（已读到的部分保留）；否则是真错。
		if wctx.Err() != nil {
			res.TimedOut = true
			res.Duration = time.Since(started)
			return res, nil
		}
		return res, fmt.Errorf("docker exec 读取输出: %w", derr)
	}

	var inspect struct {
		ExitCode int `json:"ExitCode"`
	}
	if err := c.do(ctx, http.MethodGet, "/exec/"+url.PathEscape(created.ID)+"/json", nil, &inspect); err != nil {
		return res, fmt.Errorf("docker exec 读取退出码: %w", err)
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
	if err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/exec", body, &created); err != nil {
		return nil, err
	}
	if created.ID == "" {
		return nil, fmt.Errorf("docker exec shell: 未返回 exec 实例 id")
	}

	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return nil, fmt.Errorf("连接 Docker socket: %w", err)
	}
	startBody := `{"Detach":false,"Tty":true}`
	req := fmt.Sprintf("POST /exec/%s/start HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\n"+
		"Connection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: %d\r\n\r\n%s",
		url.PathEscape(created.ID), len(startBody), startBody)
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
		rest := make([]byte, 0, 512)
		for len(rest) < 4096 {
			b, rerr := br.ReadByte()
			if rerr != nil {
				break
			}
			rest = append(rest, b)
		}
		_ = conn.Close()
		return nil, fmt.Errorf("docker exec shell: 未升级为裸流（%s：%s）",
			strings.TrimSpace(statusLine), strings.TrimSpace(string(rest)))
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
	return &hijackedStream{conn: conn, r: br}, nil
}
