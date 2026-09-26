// nfvis-cli NFViS JunOS 风格 CLI 入口（薄客户端，骨架 §3.1）。
// 行编辑/补全/提示符/历史/空闲超时在 internal/cli；本文件只做参数解析与装配。
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/xzjt/nfvis/internal/cli"
	"github.com/xzjt/nfvis/pkg/cliclient"
)

func main() {
	var (
		// 缺省须与守护进程缺省一致：deploy/nfvis.service 设 NFVIS_LISTEN=:443，且 nfvisd
		// 未提供证书时自动自签并启用 HTTPS（决策 #72）。此前缺省是明文 http://…:8443，
		// 与守护进程默认不匹配 → 默认参数连不上（决策 #78）。
		// 可用 NFVIS_SERVER 覆盖（与 NFVIS_PASSWORD 同风格），便于脚本/自动化。
		server       = flag.String("server", envOr("NFVIS_SERVER", cliclient.DefaultServer), "nfvisd 地址（缺省读 NFVIS_SERVER）")
		user         = flag.String("u", "admin", "用户名")
		passwordFlag = flag.String("p", os.Getenv("NFVIS_PASSWORD"), "口令（缺省读 NFVIS_PASSWORD；均未给则在终端下交互索取）")
		source       = flag.String("source", "ssh", "接入源（ssh|console）")
		cmdline      = flag.String("c", "", "执行多行命令后退出（换行分隔）")
		showVer      = flag.Bool("version", false, "输出版本后退出")
		caFile       = flag.String("ca", "", "服务端证书 PEM（HTTPS 校验；缺省尝试固定本机 nfvisd 证书）")
		insecure     = flag.Bool("insecure", false, "跳过 HTTPS 证书校验（仅限调试）")
	)
	flag.Parse()
	if *showVer {
		fmt.Println("nfvis-cli", cli.Version)
		return
	}

	password := *passwordFlag
	if password == "" {
		// 未给 -p / NFVIS_PASSWORD 时**交互式索取**：口令不进命令行（`ps` 与 shell 历史都看不到），
		// 也避免口令中的 shell 特殊字符（如 `!`）被 shell 先行展开。
		// 非 TTY（脚本/管道）不提示，直接给明确指引，以免挂起。
		pw, err := readPassword(os.Stdin, os.Stderr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%% %v\n", err)
			os.Exit(1)
		}
		password = pw
	}

	client := mustClient(*server, *caFile, *insecure)
	fmt.Printf("连接 %s ...\n", *server)
	if err := client.Login(*user, password); err != nil {
		fmt.Fprintf(os.Stderr, "%% 登录失败: %v\n", err)
		if isConnErr(err) {
			// 最常见的两个原因：守护进程未起 / 监听地址与端口不是缺省。
			fmt.Fprintf(os.Stderr,
				"%% 提示: 确认 nfvisd 已运行（systemctl status nfvis）；"+
					"若其监听地址/端口非缺省（当前尝试 %s），用 -server 或 NFVIS_SERVER 指定，"+
					"并注意开发用 -allow-plaintext 时须把地址写成 http://…\n", *server)
		}
		os.Exit(1)
	}
	session := cli.New(client, *source)

	if *cmdline != "" {
		runScript(session, *cmdline)
		return
	}
	// 空闲超时取 system idle-timeout-minutes，缺省 10 分钟（FR-SEC-005/FR-CLI-006）。
	timeout := cli.DefaultIdleTimeout
	if m, err := client.IdleTimeoutMinutes(); err == nil && m > 0 {
		timeout = time.Duration(m) * time.Minute
	}
	repl := cli.NewREPL(session, cli.NewHistory(), cli.NewIdleGuard(timeout, nil))
	if err := repl.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "%% %v\n", err)
		os.Exit(1)
	}
}

// runScript 多行脚本模式：任一行失败即停止；结束时清理会话。
//
// 收尾必须清理（会话按 user@source 在服务端保留）：否则脚本会残留配置模式、
// 脏 candidate 与 candidate 会话锁，导致后续调用被按上一模式解释、
// 再次以 exit 收尾时报「存在未提交变更」并失败（见 docs/reviews/2026-09-13.md）。
func runScript(session *cli.Session, cmdline string) {
	failed := false
	for _, line := range strings.Split(cmdline, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "wizard" { // 初始化向导（决策 #107）：交互式编排，非 TTY 时向导自行拒绝
			if err := cli.RunWizard(session, term.IsTerminal(int(os.Stdin.Fd())), os.Stdin, os.Stdout); err != nil {
				fmt.Printf("%% %v\n", err)
				failed = true
				break
			}
			continue
		}
		out, _ := session.ExecuteLine(line)
		fmt.Print(out)
		if !strings.HasSuffix(out, "\n") {
			fmt.Println()
		}
		// 破坏性动作的问询文本（`… ? [yes,no]`）：脚本模式没有答复来源，
		// 不判定就会「只问不做」却以退出码 0 结束（假成功）。按失败处理并指引显式确认。
		if msg, need := confirmRefusal(out); need {
			fmt.Print(msg)
			failed = true
			break
		}
		if strings.Contains(out, "%%") {
			failed = true
			break
		}
	}
	teardownScript(session)
	if failed {
		os.Exit(1)
	}
}

// teardownScript 退出配置模式（有 candidate 先丢弃）、释放会话并吊销 token。
// 收尾逻辑在 cli.Session.Teardown（-c 与交互 REPL 共用）；此处只回显失败步骤，
// 保持脚本模式的输出口径不变。
func teardownScript(session *cli.Session) {
	for _, out := range session.Teardown() {
		if strings.Contains(out, "%%") {
			fmt.Print(out)
		}
	}
}

// confirmSuffix 服务端问询确认文本的固定结尾（删 VNF/容器/镜像、软件 add|rollback、
// reboot|shutdown、zeroize、接口 bind-dpdk|unbind-dpdk 都以它结尾）。
// 交互 REPL 用同一判据（internal/cli/repl.go）：TrimSpace 后做后缀匹配，
// 故**回显了历史问询文本**（如 `show log audit`）不会误判。
const confirmSuffix = "[yes,no]"

// confirmRefusal 判定服务端输出是否为「破坏性动作的交互确认问询」。
//
// 非交互模式（`-c`）没有答复来源：交互 REPL 会读一行答复并追加 `--yes` 重发
// （见 internal/cli/repl.go），而脚本模式若把问询当普通输出，就会「命令问了没做、
// 却以退出码 0 结束」——对自动化是**假成功**。故命中即返回一条以 `%%` 开头的
// 错误（置 failed、按「任一行失败即停止」语义中止），并给出可操作的出路。
//
// 判据与 REPL 同源：仅看输出**结尾**，不猜语义。
func confirmRefusal(out string) (msg string, need bool) {
	if !strings.HasSuffix(strings.TrimSpace(out), confirmSuffix) {
		return "", false
	}
	return "%% 该命令需交互确认（破坏性动作），非交互模式不会执行；" +
		"确认要执行时请在命令尾追加 --yes 后重试（如再次问询，再追加一个 --yes）\n", true
}

// mustClient 构造 REST 客户端（FR-SEC-004：默认自签 HTTPS）。
//
// 校验策略：显式 -ca > 固定本机 nfvisd 证书（缺省，CLI 通常运行在一体机上）> 显式 -insecure。
// 均不满足时仍按系统信任库校验（自签会失败并给出明确提示）。
func mustClient(server, caFile string, insecure bool) *cliclient.Client {
	opts := cliclient.TLSOptions{CAFile: caFile, Insecure: insecure}
	if opts.CAFile == "" && !insecure && strings.HasPrefix(server, "https://") {
		if _, err := os.Stat(cliclient.DefaultServerCertPath); err == nil {
			opts.CAFile = cliclient.DefaultServerCertPath
		}
	}
	c, err := cliclient.NewWithTLS(server, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "初始化客户端失败:", err)
		os.Exit(1)
	}
	return c
}

// envOr 取环境变量，为空时用默认值。
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// isConnErr 判断是否为「连不上」类错误（网络/超时），用于给出排障提示。
// 证书校验失败等**不**算在内——那属于配置问题，报错本身已足够明确。
func isConnErr(err error) bool {
	var nerr net.Error
	if errors.As(err, &nerr) {
		return true
	}
	var oerr *net.OpError
	return errors.As(err, &oerr)
}

// readPassword 在终端下无回显地索取口令；非 TTY 时返回可操作的错误（不阻塞脚本）。
func readPassword(in *os.File, out io.Writer) (string, error) {
	fd := int(in.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("未提供口令：请在终端下运行以交互输入，或用 -p / NFVIS_PASSWORD 指定")
	}
	fmt.Fprint(out, "Password: ")
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(out)
	if err != nil {
		return "", fmt.Errorf("读取口令失败: %w", err)
	}
	return string(b), nil
}
