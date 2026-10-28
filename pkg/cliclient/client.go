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
	"path/filepath"
	"regexp"
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
	// Kind 会话种类："vm"（缺省，VM 串口 console——serial 与 telnet 两种控制台形态都走它）、
	// "container"（容器交互式终端，决策 #358）——前端据此选择提示文案，桥接方式两者相同。
	// "vnc"（VM 图形控制台，无内置查看器）由 CLI `console` 直接拒绝，CLI 侧不会出现。
	Kind string `json:"kind,omitempty"`
}

// Client nfvisd REST 客户端。
type Client struct {
	base  string
	token string
	// user/pass 登录凭据（修 BUG-16）：升级会重启 nfvisd，旧 token 随之失效；复核前必须能
	// **重新登录**才问得到结论。只在本进程内存里留，不落盘、不进日志。
	user, pass string
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
	c.user, c.pass = user, password
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
	// 本次命令的发起时刻（L5）：服务端结论记录里的「时间」是该次判定的落笔时刻，
	// 用它把重连后读到的记录**归因到本次尝试**（留旧记录不得冒充本次结果）。
	issued := time.Now()
	err := c.do(http.MethodPost, "/api/v1/cli/execute", map[string]string{
		"line": line, "source": source,
	}, &resp)
	if err != nil {
		// 修 BUG-16：`request system software add` 会让 nfvisd 重启（postinst 的
		// try-restart），承载这次请求的连接随之 EOF——升级本身在下游继续并成功，但操作者
		// 只看到「连接 nfvisd 失败」+ 非零退出码，会误判失败而重试或走回退。
		// 这里等 nfvisd 回来，再用**独立读命令**把真实结果问一遍并作为本命令的结论返回。
		if isUpgradeLine(line) && isConnDropped(err) {
			return c.recheckAfterRestart(line, err, issued)
		}
		return Result{}, err
	}
	return Result{
		Output: resp.Output, Mode: resp.Mode, Path: resp.Path, Prompt: resp.Prompt,
		Console: resp.Console, Warning: resp.Warning,
	}, nil
}

// upgradeRestartWait 升级后等「结论落定」的上限。
//
// 为什么是 240s：这段等待覆盖的不只是「nfvisd 回来」，而是**服务端判定的全过程**——
//
//	· nfvisd 重启（postinst 触发的 try-restart）：十几秒；
//	· 服务端等「同一状态行 + audit 干净」连续 6 次一致（约 10s+，spec 见 internal/system）；
//	· 若 dpkg 被打断留下 half-configured，脚本还要跑一次 `dpkg --configure nfvis` 自救，
//	  那又是一轮 postinst（重启 + 基线检查），实测数十秒。
//
// 90s 在真机上不够（CLI 会读到 pending 就报「未定」，而包其实稍后就装好了）。
// 仍**有界**：等不到就如实说「结果未知」，不无限挂住终端。
//
// var（非 const）：单测把窗口与轮询间隔一起缩短（见 fix_r51_l5_test.go），与 internal/system 的
// dpkgRCWait 同法——产品路径只读、不写。
var upgradeRestartWait = 240 * time.Second

// recheckInterval 复核轮询间隔（两次读结论之间）。真机上 2s 足够密（服务端终判要 10s+，
// 重启本身十几秒），又不至于把刚起来的 nfvisd 打满；单测缩短它。
var recheckInterval = 2 * time.Second

// isUpgradeLine 该命令行是不是会重启 nfvisd 的那类动作（升级/回退）。
//
// 只认这两条：它们必然触发 postinst 的 try-restart；其它命令即使让服务抖动，也不该被
// 静默改成「重连后报另一条命令的结论」。
func isUpgradeLine(line string) bool {
	l := strings.Join(strings.Fields(line), " ")
	return strings.HasPrefix(l, "request system software add") ||
		strings.HasPrefix(l, "request system software rollback")
}

// isConnDropped 这次错误是不是「服务端在请求处理中断开了连接」（EOF / 连接被重置 / 对端关闭），
// 而不是超时或本地网络问题——只有前者才可能对应「服务端已受理并重启」。
func isConnDropped(err error) bool {
	if err == nil {
		return false
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return false // 超时另有专门文案（决策 #76），不在此改判
	}
	s := err.Error()
	return strings.Contains(s, "EOF") || strings.Contains(s, "connection reset") ||
		strings.Contains(s, "broken pipe") || strings.Contains(s, "server closed")
}

// recheckAfterRestart 等 nfvisd 回来后复核升级结果，并把结论作为本命令的输出返回。
//
// 复核**不猜**：服务端在安装结束时把结论（状态行 + 版本 + 成败 + 时间）落到
// /var/lib/nfvis/software/last-result.json（见 system.LastResultPath），这里重连后读它。
// 为什么不读 dpkg：CLI 在宿主之外拿不到 dpkg；而 `show version` 给的是**运行中进程**的编译
// 版本，证明不了包状态（半装时它照样报新版本）。
//
// L5（round51 评审）：读到的记录必须能**归因到本次尝试**（issued = 本次命令的发起时刻），
// 否则一条历史 SUCCESS 记录会被当成本次结果报「结论: 成功」+ 退出码 0——对一次**根本没跑成**
// 的动作谎报成功。归因规则（见 unattributable）：
//   - 记录带「时间」（≥3.0.26 的服务端都写）：须不早于 issued；
//   - 命令行能推出目标版本（`rollback to <v>`、或 `add` 的 `nfvis_<v>_<arch>.deb` 本地路径，
//     与 internal/system 同一正则）：记录版本须等于它。
//
// 两条都可用时**都须成立**；任一条不成立就继续在既有有界窗口内等（新记录可能还没落笔），
// 等到头仍归因不上就如实报「未定」，绝不把旧记录当成功。
func (c *Client) recheckAfterRestart(line string, cause error, issued time.Time) (Result, error) {
	// 服务端按 RFC3339（秒级）渲染时间，亚秒被截掉；issued 也截到秒，避免「同一秒内」被误判成早于。
	issued = issued.Truncate(time.Second)
	target := targetVersionOf(line)
	deadline := time.Now().Add(upgradeRestartWait)
	var lastErr error
	for time.Now().Before(deadline) {
		time.Sleep(recheckInterval)
		// 旧 token 随 nfvisd 重启失效：先重登（凭据只在内存里），再问结论。
		if c.user != "" {
			if err := c.Login(c.user, c.pass); err != nil {
				lastErr = err
				continue // nfvisd 还没回来 / 尚未就绪
			}
		}
		var r Result
		if err := c.executeOnce("show system software result", &r); err != nil {
			lastErr = err
			continue // nfvisd 还没回来
		}
		ok, ver, status, recTime := parseSoftwareResult(r.Output)
		// 结论还在 pending（升级进行中/判定未完成）⇒ **继续等**，不要就此下「失败」的结论。
		// 真机实证：服务端的终判要等「同一状态行连续 6 次一致」（约 10s+），而 CLI 重连通常
		// 在 16s 内就发生——读到 pending 是中间态，不是结果；把它当失败会让用户白白回退。
		// 结论还没定：pending（升级进行中）或空（脚本刚起步、还没有观测）。
		// 两者都**不是结果**，必须接着等——把它当失败会让用户对一次正在成功的升级执行回退。
		if status == statusPending || status == "" {
			lastErr = fmt.Errorf("服务端结论尚未落定（当前 %q）", status)
			continue
		}
		// L5：归因不上（旧记录仍在、新记录还没落笔）⇒ 接着等，不当成本次结论。
		if why := unattributable(recTime, ver, issued, target); why != "" {
			lastErr = fmt.Errorf("服务端记录不属于本次尝试（%s）", why)
			continue
		}
		var b strings.Builder
		if strings.HasPrefix(line, "request system software rollback") {
			b.WriteString("回退动作已受理：nfvisd 因此重启，本次连接已断开（预期行为，不是失败）。\n")
		} else {
			b.WriteString("升级动作已受理：nfvisd 因此重启，本次连接已断开（预期行为，不是失败）。\n")
		}
		b.WriteString("已自动重连并读取服务端记录的实际结果：\n")
		if ver != "" {
			fmt.Fprintf(&b, "  版本: %s\n", ver)
		}
		if status != "" {
			fmt.Fprintf(&b, "  状态: %s\n", status)
		}
		if ok {
			b.WriteString("结论: 成功。\n")
			return Result{Output: b.String(), Mode: "oper"}, nil
		}
		b.WriteString("结论: 未落定为成功——可执行 request system software rollback 回退，或看 journalctl -u nfvis 定位。\n")
		return Result{}, fmt.Errorf("%s", b.String())
	}
	// 有界窗口内既没等到可归因的结论（可能是历史记录在冒充，也可能是判定一直没落定）：
	// 如实报**未定**并带上最后一次观测到的错误（登录/HTTP 文案），让操作者知道卡在哪一环。
	return Result{}, fmt.Errorf("升级请求的连接中断（%v），且在 %s 内未能重连 nfvisd 复核**本次**升级的结果"+
		"（结论未定，不得据此判成败）：最后一次复核错误：%v；请手动核对 `dpkg -l nfvis`（升级可能已成功）",
		cause, upgradeRestartWait, lastErr)
}

// unattributable 判定服务端记录能否归因到本次尝试；归因不上时返回原因（空串＝可归因）。
//
// 规则见 recheckAfterRestart 的注释；核心是「旧记录不得冒充本次结果」：
//   - 记录有版本、命令行也推出了目标版本 ⇒ 两者必须一致；
//   - 记录有时间 ⇒ 不得早于本次发起时刻；
//   - 记录无时间（3.0.26 之前写入）⇒ 只能靠版本归因，版本不可比对时如实说无法归因。
func unattributable(recTime time.Time, ver string, issued time.Time, target string) string {
	if ver != "" && target != "" && ver != target {
		return fmt.Sprintf("记录版本 %s 与本次目标版本 %s 不一致", ver, target)
	}
	if recTime.IsZero() {
		if target == "" || ver != target {
			return "记录缺少时间字段，且记录版本与本次目标版本不可比对"
		}
		return ""
	}
	if recTime.Before(issued) {
		return fmt.Sprintf("记录时间 %s 早于本次发起时刻 %s", recTime.Format(time.RFC3339), issued.Format(time.RFC3339))
	}
	return ""
}

// debVersionRe 与服务端同一判据（internal/system 的 debVersionRe）：从包文件名取版本段。
var debVersionRe = regexp.MustCompile(`^nfvis_(.+)_(amd64|arm64|all)\.deb$`)

// targetVersionOf 从升级/回退命令行推出**本次尝试的目标版本**（推不出返回空串，此时只能靠时间归因）。
//
// 只认命令行里能确定的东西：`rollback to <v>` 的目标就是 <v>；`add` 的**本地 .deb 路径**若形如
// nfvis_<v>_<arch>.deb 则取版本段（服务端另以 `dpkg-deb -f Version` 为准——名字被改过时宁可
// 不判版本，交给时间归因，而不是拿一个错的版本把人吓住）。自动选版的 rollback 与 URL 安装
// 都推不出目标版本，返回空串。
func targetVersionOf(line string) string {
	f := strings.Fields(line)
	if len(f) < 4 || f[0] != "request" || f[1] != "system" || f[2] != "software" {
		return ""
	}
	switch f[3] {
	case "rollback":
		for i := 4; i+1 < len(f); i++ {
			if f[i] == "to" {
				return f[i+1]
			}
		}
	case "add":
		for _, tok := range f[4:] {
			if strings.Contains(tok, "://") {
				continue // URL 安装：目标版本取决于下载件内容，命令行里拿不到
			}
			if m := debVersionRe.FindStringSubmatch(filepath.Base(tok)); m != nil {
				return m[1]
			}
		}
	}
	return ""
}

// executeOnce 发一次执行请求（不触发升级重连复核，供内部复核路径用）。
func (c *Client) executeOnce(line string, out *Result) error {
	var resp struct {
		Output string   `json:"output"`
		Mode   string   `json:"mode"`
		Path   []string `json:"path"`
		Prompt string   `json:"prompt"`
	}
	if err := c.do(http.MethodPost, "/api/v1/cli/execute",
		map[string]string{"line": line, "source": "ssh"}, &resp); err != nil {
		return err
	}
	*out = Result{Output: resp.Output, Mode: resp.Mode, Path: resp.Path, Prompt: resp.Prompt}
	return nil
}

// statusPending 服务端结论文件里「升级进行中、结论未定」的标记（与 internal/system 同值）。
// 读到它表示**还没定**，既不是成功也不是失败——复核循环要接着等。
const statusPending = "pending"

// parseSoftwareResult 解析服务端 `show system software result` 的输出。
//
// 输出形状由服务端渲染（`结果: 成功|失败`、`版本: x`、`状态: install ok installed`、`时间: <RFC3339>`）；
// 解析不出「结果」行时返回 ok=false——**判不出就不当作成功**。recTime 为记录落笔时刻（L5：
// 归因本次尝试用；服务端不写该行或格式不认识时为零值，调用方据此改走版本归因）。
func parseSoftwareResult(out string) (ok bool, version, status string, recTime time.Time) {
	for _, ln := range strings.Split(out, "\n") {
		s := strings.TrimSpace(ln)
		if v, found := strings.CutPrefix(s, "版本:"); found {
			version = strings.TrimSpace(v)
		}
		if v, found := strings.CutPrefix(s, "状态:"); found {
			status = strings.TrimSpace(v)
		}
		if v, found := strings.CutPrefix(s, "时间:"); found {
			if t, err := time.Parse(time.RFC3339, strings.TrimSpace(v)); err == nil {
				recTime = t
			}
		}
		if v, found := strings.CutPrefix(s, "结果:"); found {
			ok = strings.TrimSpace(v) == "成功"
		}
	}
	return ok, version, status, recTime
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
