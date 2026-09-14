// Package cliclient 提供 nfvis-cli 访问 nfvisd 的客户端 SDK（骨架 §2 pkg/cliclient）。
//
// 依赖方向约束（骨架 §3.1）：CLI 前端（internal/cli、cmd/nfvis-cli）只能通过
// 本包访问守护进程，不得 import internal/config 等业务包——薄客户端原则在
// 编译期强制。Web 控制面未来也可复用本 SDK。
package cliclient

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Result 单条 CLI 命令的执行结果。
type Result struct {
	Output string
	Mode   string // oper | config
	Path   []string
	Prompt string
	// Console 非空表示该命令要求前端接管终端并桥接串口（M4-12，FR-CMP-014）：
	// 前端（internal/cli）经 DialConsole 连 WSURL，Ctrl-] 退出后恢复行编辑。
	Console *ConsoleRequest
}

// ConsoleRequest 串口终端接管请求（守护进程 CLIEResult.Console）。
type ConsoleRequest struct {
	VM    string `json:"vm"`
	WSURL string `json:"ws_url"`
}

// Client nfvisd REST 客户端。
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// RequestTimeout 单次请求上限。
//
// 必须**大于**服务端同步阻塞命令的上限：VM stop 走 ACPI 等待后强杀，上限为
// compute.StopTimeout（默认 30s）。若两者相等，任何走强杀路径的 stop 都会先触发客户端超时，
// 用户看到「连接 nfvisd 失败」而实际已停成功（真机实测，决策 #76）。取 2 倍 + 裕量。
const RequestTimeout = 90 * time.Second

// New 构造客户端。server 形如 https://host:443 或 http://127.0.0.1:8443。
func New(server string) *Client {
	return &Client{
		base: server,
		hc:   &http.Client{Timeout: RequestTimeout},
	}
}

// TLSOptions HTTPS 客户端选项（FR-SEC-004：nfvisd 默认以自签证书提供 HTTPS）。
type TLSOptions struct {
	// CAFile 服务端证书（PEM）。给出则按该系统信任锚校验——自签场景即**证书固定**。
	CAFile string
	// Insecure 跳过校验（仅限调试；显式选择，不默认开启）。
	Insecure bool
}

// NewWithTLS 构造带 TLS 选项的客户端。
//
// 背景：nfvisd 默认自签 HTTPS（决策 #72），而系统信任库不含该证书，
// 故客户端必须能校验它：nfvis-cli 通常就运行在一体机上（规格 §3.1：sshd 的 shell 即 nfvis-cli），
// 因此优先**固定守护进程自己的证书**（安全且零配置），而不是默认跳过校验。
func NewWithTLS(server string, opts TLSOptions) (*Client, error) {
	hc := &http.Client{Timeout: RequestTimeout}
	if strings.HasPrefix(server, "https://") {
		tc := &tls.Config{MinVersion: tls.VersionTLS12}
		switch {
		case opts.Insecure:
			tc.InsecureSkipVerify = true
		case opts.CAFile != "":
			pem, err := os.ReadFile(opts.CAFile)
			if err != nil {
				return nil, fmt.Errorf("读取证书 %s: %w", opts.CAFile, err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("证书 %s 不含可用 PEM", opts.CAFile)
			}
			tc.RootCAs = pool
		}
		hc.Transport = &http.Transport{TLSClientConfig: tc}
	}
	return &Client{base: server, hc: hc}, nil
}

// DefaultServerCertPath 守护进程自签证书的缺省路径（与 system.DefaultTLSDir 一致）。
const DefaultServerCertPath = "/var/lib/nfvis/tls/server.crt"

// DefaultServer nfvis-cli 的缺省服务端地址。
//
// **必须与守护进程的缺省监听保持一致**：`deploy/nfvis.service` 设 `NFVIS_LISTEN=:443`，
// 且 nfvisd 未提供证书时自动生成自签并启用 HTTPS（决策 #72）。
// 此前 CLI 缺省为 `http://127.0.0.1:8443`（明文、另一端口）→ **默认参数连不上**，
// 「装完即用」的第一步必然失败（决策 #78）。
//
// 用 HTTPS 缺省还顺带拿到**零配置的证书固定**：mustClient 在未给 -ca 时自动固定
// DefaultServerCertPath（见 cmd/nfvis-cli/main.go），故本机用户无需任何参数即可连上。
const DefaultServer = "https://127.0.0.1:443"

// SetToken 注入既有 token（跳过登录）。
func (c *Client) SetToken(tok string) { c.token = tok }

// Login 登录换取 token（FR-API-001）。
func (c *Client) Login(user, password string) error {
	var resp struct {
		Token string `json:"token"`
	}
	if err := c.do(http.MethodPost, "/api/v1/login", map[string]string{
		"username": user, "password": password,
	}, &resp); err != nil {
		return err
	}
	if resp.Token == "" {
		return fmt.Errorf("登录失败：响应缺少 token")
	}
	c.token = resp.Token
	return nil
}

// Logout 吊销当前 token。
func (c *Client) Logout() error {
	return c.do(http.MethodPost, "/api/v1/logout", nil, nil)
}

// Execute 执行一行 CLI 命令（经 nfvisd cli_bridge；source 影响
// FR-CFG-012 管理口自锁判定，ssh/console）。
func (c *Client) Execute(line, source string) (Result, error) {
	var resp struct {
		Output  string          `json:"output"`
		Mode    string          `json:"mode"`
		Path    []string        `json:"path"`
		Prompt  string          `json:"prompt"`
		Console *ConsoleRequest `json:"console"`
	}
	err := c.do(http.MethodPost, "/api/v1/cli/execute", map[string]string{
		"line": line, "source": source,
	}, &resp)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Output: resp.Output, Mode: resp.Mode, Path: resp.Path, Prompt: resp.Prompt,
		Console: resp.Console,
	}, nil
}

func (c *Client) do(method, path string, body any, out any) error {
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		// 超时与真正连不上要分开说：服务端有几条**同步阻塞**的命令（如 VM stop 等 ACPI 关机，
		// 上限即 compute.StopTimeout=30s），若客户端超时与服务端上限相等，用户会always看到
		// 「连接 nfvisd 失败」——而操作其实已在服务端成功（决策 #76）。
		var nerr net.Error
		if errors.As(err, &nerr) && nerr.Timeout() {
			return fmt.Errorf("请求超时（%s）：操作可能已在服务端完成，请用 show 确认（如 show virtual-machine-functions <name>）: %w",
				RequestTimeout, err)
		}
		return fmt.Errorf("连接 nfvisd 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Message != "" {
			return fmt.Errorf("%s", e.Message)
		}
		return fmt.Errorf("请求失败: HTTP %d", resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// IdleTimeoutMinutes 读取系统配置的 CLI 空闲超时（FR-SEC-005；0 表示未配置）。
func (c *Client) IdleTimeoutMinutes() (int, error) {
	var out struct {
		IdleTimeoutMinutes int `json:"idle_timeout_minutes"`
	}
	if err := c.do(http.MethodGet, "/api/v1/system", nil, &out); err != nil {
		return 0, err
	}
	return out.IdleTimeoutMinutes, nil
}

// DynamicCandidates 查询指定来源的动态候选（接口名/VNF 名/镜像名等，§5.3）。
func (c *Client) DynamicCandidates(kind string) ([]string, error) {
	var resp []string
	if err := c.do(http.MethodGet, "/api/v1/cli/candidates?kind="+kind, nil, &resp); err != nil {
		return nil, err
	}
	return resp, nil
}
