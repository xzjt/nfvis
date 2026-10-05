// Package cliclient 提供 nfvis-cli 访问 nfvisd 的客户端 SDK（骨架 §2 pkg/cliclient）。
//
// 依赖方向约束（骨架 §3.1）：CLI 前端（internal/cli、cmd/nfvis-cli）只能通过
// 本包访问守护进程，不得 import internal/config 等业务包——薄客户端原则在
// 编译期强制。Web 控制面未来也可复用本 SDK。
package cliclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// Warning 为真表示 Output 是**提示**而非失败（当前唯一来源：语句未产生配置变更——
	// 值未变化/未映射到模型）。脚本模式（`-c`）据此继续执行、不影响退出码；
	// 判据是服务端的结构化标记，不是输出文本（round86 R86-8）。
	Warning bool
}

// ConsoleRequest 串口终端接管请求（守护进程 CLIEResult.Console）。
type ConsoleRequest struct {
	VM    string `json:"vm"`
	WSURL string `json:"ws_url"`
	// Kind 会话种类："vm"（缺省，VM 串口 console）或 "container"（容器交互式终端，
	// 决策 #358）——前端据此选择提示文案，桥接方式两者相同。
	Kind string `json:"kind,omitempty"`
}

// Client nfvisd REST 客户端。
type Client struct {
	base  string
	token string
	// class 登录响应 `user.class`（服务端权威的本会话 login class，决策 #145/#324）。
	// nfvis-cli 据此按同一口径过滤 `?`/Tab 候选（见 internal/cli.Session）。
	class string
	hc    *http.Client
	// tc HTTPS 的 TLS 口径（决策 #156）：REST 与 console 的 wss 拨号必须同源——
	// 此前只装在 http.Transport 里，console 用默认 TLS 校验自签证书必挂。
	// 明文（http://）为 nil。
	tc *tls.Config
}

// RequestTimeout 单次请求的缺省等待上限（每请求 ctx deadline；不是全局 http.Client.Timeout，
// 决策 #366）。
//
// 必须**大于**服务端同步阻塞命令的上限：VM stop 走 ACPI 等待后强杀，上限为
// compute.StopTimeout（默认 30s）。若两者相等，任何走强杀路径的 stop 都会先触发客户端超时，
// 用户看到「连接 nfvisd 失败」而实际已停成功（真机实测，决策 #76）。取 2 倍 + 裕量。
//
// 例外（决策 #366）：`cli/execute` 里的容器 exec 超时可配到 300s——全局上限会先于服务端
// 截断 `timeout 91..300` 的 exec（用户看到「请求超时」，命令还在容器里跑），
// 故等待时长按命令内容延长（requestDeadline）。
const RequestTimeout = 90 * time.Second

// New 构造客户端。server 形如 https://host:443 或 http://127.0.0.1:8443。
//
// hc **不带**全局 Timeout（决策 #366）：有界性改由每请求 ctx deadline 提供
// （do/doWithDeadline），才能对长 exec 单独放宽等待。
func New(server string) *Client {
	return &Client{
		base: server,
		hc:   &http.Client{},
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
	hc := &http.Client{}
	var tc *tls.Config
	if strings.HasPrefix(server, "https://") {
		tc = &tls.Config{MinVersion: tls.VersionTLS12}
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
	return &Client{base: server, hc: hc, tc: tc}, nil
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

// Class 返回登录响应里服务端权威的本会话 login class（未登录/缺字段为空串）。
// 决策 #324：CLI 前端按它与命令树节点等级同源地过滤 `?`/Tab 候选。
func (c *Client) Class() string { return c.class }

// Login 登录换取 token（FR-API-001），并记下服务端权威的 login class（决策 #324）。
func (c *Client) Login(user, password string) error {
	var resp struct {
		Token string `json:"token"`
		User  struct {
			Class string `json:"class"`
		} `json:"user"`
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
	c.class = resp.User.Class
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
		Warning bool            `json:"warning"`
	}
	err := c.do(http.MethodPost, "/api/v1/cli/execute", map[string]string{
		"line": line, "source": source,
	}, &resp)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Output: resp.Output, Mode: resp.Mode, Path: resp.Path, Prompt: resp.Prompt,
		Console: resp.Console, Warning: resp.Warning,
	}, nil
}

func (c *Client) do(method, path string, body any, out any) error {
	// 等待时长（决策 #366）：缺省 RequestTimeout；仅对 `cli/execute` 按命令内容识别
	// 容器 exec 的超时提示并延长（超时 300 的 exec 若仍按 90s 等待，会先于服务端截断）。
	d := RequestTimeout
	if path == "/api/v1/cli/execute" {
		if m, ok := body.(map[string]string); ok {
			d = requestDeadline(m["line"])
		}
	}
	return c.doWithDeadline(d, method, path, body, out)
}

// doWithDeadline 在给定等待上限内执行一次请求（决策 #366：有界性由每请求 ctx deadline
// 提供，不再依赖 http.Client 全局 Timeout）。测试经它注入更小的 deadline 验证超时真生效。
func (c *Client) doWithDeadline(d time.Duration, method, path string, body any, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
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
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
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
			// 提示要**与命令无关**（发现 #13）：原文写死「如 show virtual-machine-functions <name>」，
			// 而超时也可能发生在接口/配置/运维动作上——对不上号的例子比不给还糟。
			return fmt.Errorf("请求超时（%s）：操作可能已在服务端完成或仍在进行；"+
				"请先用相关 show 命令核对实际状态，必要时看 nfvisd 日志（journalctl -u nfvis）: %w",
				d, err)
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

// MetricsText 拉取 /api/v1/metrics 的原始文本（无鉴权端点，M5-2）。
// setup 向导读取主机事实（在线核数/内存总量）用：metrics 是机器可读格式，
// 解析它不属于「解析 show 表格文本」的脆弱类（决策 #85 的教训）。
//
// 决策 #366：hc 不再有全局 Timeout，此处自带 ctx deadline 保住有界性（不走 do：
// 响应是 Prometheus 文本不是 JSON）。
func (c *Client) MetricsText() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/metrics", nil)
	if err != nil {
		return "", err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /api/v1/metrics: %s", resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
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

// LoginBanner 拉取登录横幅（未认证端点，决策 #303）。
// nfvis-cli 交互模式在口令提示**之前**展示：本方法在未认证阶段调用（无 token，
// 端点也不要求）。未设置横幅返回空串；网络/服务端失败原样返回错误，由调用方
// 静默跳过（横幅是展示性功能，不得挡住登录流程）。
func (c *Client) LoginBanner() (string, error) {
	var out struct {
		Banner string `json:"banner"`
	}
	if err := c.do(http.MethodGet, "/api/v1/login-banner", nil, &out); err != nil {
		return "", err
	}
	return out.Banner, nil
}

// DynamicCandidates 查询指定来源的动态候选（接口名/VNF 名/镜像名等，§5.3）。
func (c *Client) DynamicCandidates(kind string) ([]string, error) {
	var resp []string
	if err := c.do(http.MethodGet, "/api/v1/cli/candidates?kind="+kind, nil, &resp); err != nil {
		return nil, err
	}
	return resp, nil
}
