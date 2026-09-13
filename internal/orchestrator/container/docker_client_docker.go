package container

// Docker Engine API 薄适配层（unix socket + HTTP）。文件名 _docker.go → 覆盖率排除，
// 由 nfvis-vm 集成测试覆盖。字段以 Docker Engine API 29.x 为准（实测版本见 M4-P0 记录）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type dockerClient struct {
	http *http.Client
	base string
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
	return &dockerClient{http: &http.Client{Transport: tr}, base: "http://docker"}
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
