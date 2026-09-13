// Package cliclient 提供 nfvis-cli 访问 nfvisd 的客户端 SDK（骨架 §2 pkg/cliclient）。
//
// 依赖方向约束（骨架 §3.1）：CLI 前端（internal/cli、cmd/nfvis-cli）只能通过
// 本包访问守护进程，不得 import internal/config 等业务包——薄客户端原则在
// 编译期强制。Web 控制面未来也可复用本 SDK。
package cliclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Result 单条 CLI 命令的执行结果。
type Result struct {
	Output string
	Mode   string // oper | config
	Path   []string
	Prompt string
}

// Client nfvisd REST 客户端。
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// New 构造客户端。server 形如 https://host:443 或 http://127.0.0.1:8443。
func New(server string) *Client {
	return &Client{
		base: server,
		hc:   &http.Client{Timeout: 30 * time.Second},
	}
}

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
		Output string   `json:"output"`
		Mode   string   `json:"mode"`
		Path   []string `json:"path"`
		Prompt string   `json:"prompt"`
	}
	err := c.do(http.MethodPost, "/api/v1/cli/execute", map[string]string{
		"line": line, "source": source,
	}, &resp)
	if err != nil {
		return Result{}, err
	}
	return Result{Output: resp.Output, Mode: resp.Mode, Path: resp.Path, Prompt: resp.Prompt}, nil
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
