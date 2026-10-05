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
	"github.com/xzjt/nfvis/internal/cliparse"
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
		scriptFile   = flag.String("f", "", "执行脚本文件后退出（按行，语义同 -c；- 表示读 stdin；与 -c 互斥）")
		showVer      = flag.Bool("version", false, "输出版本后退出")
		caFile       = flag.String("ca", "", "服务端证书 PEM（HTTPS 校验；缺省尝试固定本机 nfvisd 证书）")
		insecure     = flag.Bool("insecure", false, "跳过 HTTPS 证书校验（仅限调试）")
	)
	flag.Parse()
	if *showVer {
		fmt.Println("nfvis-cli", cli.Version)
		return
	}

	// 脚本来源（-c 字符串 / -f 文件或 stdin）收敛到同一入口：执行路径只有一条（runScript），
	// 两种来源解析后的文本完全同义（决策 #309）。解析失败（互斥/缺文件/空文件）在此即退出，
	// 不必先握手/登录取口令。
	// 决策 #370（R142 E9）：区分「未给标志」与「给了空值」——`-c ''`/`-f ''` 必须报错，
	// 不能静默进交互模式（空脚本「假成功」）。
	cSet, fSet := false, false
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "c":
			cSet = true
		case "f":
			fSet = true
		}
	})
	script, err := resolveScript(*cmdline, cSet, *scriptFile, fSet, os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%% %v\n", err)
		os.Exit(1)
	}

	// 客户端先于口令提示装配：登录横幅（决策 #303）要在提示口令**之前**展示。
	client := mustClient(*server, *caFile, *insecure)
	fmt.Printf("连接 %s ...\n", *server)
	// 登录横幅在提示口令之前取一次；脚本模式不取也不打印（见 printLoginBanner）。
	printLoginBanner(os.Stdout, script, client)
	password := *passwordFlag
	if password == "" {
		// `-f -` 时 stdin 已被脚本吃掉，不能再拿它当口令来源：给明确指引而非静默挂起。
		if *scriptFile == "-" {
			fmt.Fprintln(os.Stderr, "%% 使用 -f - 时 stdin 已用于脚本，请用 -p 或 NFVIS_PASSWORD 提供口令")
			os.Exit(1)
		}
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
	// 决策 #324：把服务端权威的本会话 class 交给前端，`?`/Tab 候选按同一口径过滤
	// （无权执行的入口不列出；执行路径的既有拒绝语义不变，纵深防御保留）。
	session.SetClass(client.Class())

	if script != "" {
		runScript(session, script)
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

// printLoginBanner 交互模式的登录横幅（决策 #303）：在提示口令**之前**展示，未认证阶段即可见。
//
// 脚本模式（script 非空，`-c` 或 `-f`）**不取也不打印**：脚本的成败判定看输出行首的 %/%%（见 runScriptLines），
// 横幅文本会污染输出与判定。网络/服务端任何失败静默跳过（横幅是展示性功能，不得挡住登录流程），
// 这一层由 cli.PrintLoginBanner 保证。返回值仅供单测断言，调用方忽略。
func printLoginBanner(w io.Writer, script string, f cli.BannerFetcher) bool {
	if script != "" {
		return false
	}
	return cli.PrintLoginBanner(w, f)
}

// resolveScript 把脚本来源（`-c` 字符串 / `-f` 文件或 stdin）收敛到**同一入口**，
// 返回归一后的脚本文本；**两个标志都没给**表示交互模式（返回 ""）。
//
// 契约（决策 #309；#370 补空值）：
//   - `-c` 与 `-f` **互斥**，同时给出即报错（文案说清二者选一）；
//   - 标志**显式给出**即须有非空脚本：`-c ”` / `-f ”`（空值）与空文件一样报错——
//     此前空值会静默落进交互模式（空脚本「假成功」，R142 E9）；
//   - `-f <file>` 文件不存在/不可读 ⇒ 报错含路径；去空白后为空 ⇒ 报错；
//   - `-f -` 读 stdin（管道喂脚本的自动化形态）；
//   - 两种来源都经 normalizeScriptText 归一，语义完全一致。
//
// cmdlineSet/fileSet = 对应标志是否在命令行**显式出现**（flag.Visit 判定）。
func resolveScript(cmdline string, cmdlineSet bool, file string, fileSet bool, stdin io.Reader) (string, error) {
	switch {
	case cmdlineSet && fileSet:
		return "", errors.New("-c 与 -f 只能选其一：-c 直接给命令串，-f 从文件读；请去掉其中之一")
	case cmdlineSet:
		text := normalizeScriptText(cmdline)
		if strings.TrimSpace(text) == "" {
			return "", errors.New("-c 的脚本为空：没有任何命令可执行（要进交互模式请去掉 -c）")
		}
		return text, nil
	case fileSet:
		if file == "" {
			return "", errors.New("-f 的脚本为空：没有任何命令可执行（要进交互模式请去掉 -f）")
		}
	default:
		return "", nil // 两个标志都没给 ⇒ 交互模式
	}
	var data []byte
	var err error
	if file == "-" {
		data, err = io.ReadAll(stdin)
	} else {
		data, err = os.ReadFile(file)
	}
	if err != nil {
		return "", fmt.Errorf("读取脚本失败（%s）: %w", file, err)
	}
	text := normalizeScriptText(string(data))
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("脚本为空（%s）：没有任何命令可执行", file)
	}
	return text, nil
}

// normalizeScriptText 归一脚本换行与 BOM（决策 #309）。
//
// 脚本文件常由 Windows 编辑器写好：CRLF 行尾会让最后一条命令读成 `commit\r` 而报语法错，
// 文件头的 UTF-8 BOM 会让首条命令读成 `\ufeffconfigure`。这里统一剥 BOM、把 CRLF 与孤立 CR
// 归一为 LF；`-c` 与 `-f` 走同一归一点，故两种来源语义一致。
func normalizeScriptText(s string) string {
	s = strings.TrimPrefix(s, "\ufeff")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// runScript 多行脚本模式：任一行**真错误**即停止；结束时清理会话。
//
// 收尾必须清理（会话按 user@source 在服务端保留）：否则脚本会残留配置模式、
// 脏 candidate 与 candidate 会话锁，导致后续调用被按上一模式解释、
// 再次以 exit 收尾时报「存在未提交变更」并失败（见 docs/reviews/2026-09-13.md）。
func runScript(session *cli.Session, script string) {
	failed := runScriptLines(session, script)
	teardownScript(session)
	if failed {
		os.Exit(1)
	}
}

// runScriptLines 逐条语句执行脚本，返回是否失败。失败即**停止**（后续语句不执行），
// 并如实用行号说明停在哪一条、其余几条未执行（决策 #320）。
// 与 runScript 分开是为了可单测（后者收尾后直接 os.Exit）。
// 入参 script 已由 resolveScript 归一（CRLF/孤立 CR → LF、剥 BOM），`-c` 与 `-f` 共用本函数。
//
// 切句用 cliparse.SplitStatements（决策 #313）：**按未引用的换行**切，引号未闭合时把后续行
// 并入同一语句——这样多行引号值（如内联的 user-data）取整段、值内保留换行；此前按 `\n` 盲切
// 会把值只当首行、其余行当命令执行（静默截断）。
func runScriptLines(session *cli.Session, script string) bool {
	return runScriptLinesTo(session, script, os.Stdout)
}

// runScriptLinesTo 是 runScriptLines 的可测核心：输出写到 w（单测传 buffer 核对「停在哪一行」）。
//
// 失败判据与初始化向导**同源**（cli.OutputFailed，决策 #113）：**行首**单个或双个 %、
// 或行首「校验失败」即该行失败——不新造第二套判据。这一条曾只查 `%%`，于是服务端用
// fmt 格式串写出的 `%% 无效命令: show system …` 实际渲染成单 %（`fmt.Sprintf` 把 `%%`
// 折叠为一个 `%`）时不被判失败，脚本会继续执行后续语句，把前段错误掩盖掉（真机 round100
// 阶段 7 实测，R100-1）。「值未变化」这类空操作由服务端以结构化标记 Warning 明确为非失败，
// 脚本继续（决策 #190）——故还须 `!res.Warning`，而不是去猜输出文本里哪里有 `%`。
func runScriptLinesTo(session *cli.Session, script string, w io.Writer) (failed bool) {
	stmts := cliparse.SplitStatements(script)
	total := 0
	for _, s := range stmts {
		if strings.TrimSpace(s) != "" {
			total++
		}
	}
	line, done := 1, 0 // line = 当前语句的起始物理行号；done = 已执行（含失败那条）的非空语句数
	for _, raw := range stmts {
		stmt := strings.TrimSpace(raw)
		startLine := line
		line += strings.Count(raw, "\n") + 1 // 本语句占用 count+1 个物理行
		if stmt == "" {
			continue
		}
		done++
		if stmt == "wizard" { // 初始化向导（决策 #107）：交互式编排，非 TTY 时向导自行拒绝
			if err := cli.RunWizard(session, term.IsTerminal(int(os.Stdin.Fd())), os.Stdin, os.Stdout); err != nil {
				fmt.Fprintf(w, "%% %v\n", err)
				reportScriptStop(w, startLine, stmt, "初始化向导执行失败", total-done)
				return true
			}
			continue
		}
		res := session.Execute(stmt)
		out := res.Output
		fmt.Fprint(w, out)
		if !strings.HasSuffix(out, "\n") {
			fmt.Fprintln(w)
		}
		// 破坏性动作的问询文本（`… ? [yes,no]`）：脚本模式没有答复来源，
		// 不判定就会「只问不做」却以退出码 0 结束（假成功）。按失败处理并指引显式确认。
		if msg, need := confirmRefusal(out); need {
			fmt.Fprint(w, msg)
			reportScriptStop(w, startLine, stmt, "命令需交互确认（非交互模式不执行破坏性动作）", total-done)
			return true
		}
		// 交互式终端接管请求（串口 console / 容器 shell）：脚本模式**无法**接管终端。
		// 静默丢弃会让脚本看到「正在打开 …」却什么都没开——与「只问不做」同一类假成功，
		// 故按失败处理并指引交互式 CLI（决策 #358 顺带收口；此前对 console 也存在）。
		if res.Console != nil {
			fmt.Fprintf(w, "%% 脚本模式（非 TTY）不支持交互式终端接管（%s）：请在交互式 CLI 里执行本命令\n",
				res.Console.VM)
			reportScriptStop(w, startLine, stmt, "交互式命令不能在脚本模式执行", total-done)
			return true
		}
		// 行首 %/「校验失败」= 该行失败（判据与向导同源）；Warning 的空操作不算失败（决策 #190）。
		if cli.OutputFailed(out) && !res.Warning {
			reportScriptStop(w, startLine, stmt, "命令执行失败（输出含错误标记）", total-done)
			return true
		}
	}
	return false
}

// reportScriptStop 如实报告脚本为何停、停在哪一条、还有几条未执行（决策 #320）。
//
// 行号口径：**该语句在归一后脚本里的起始物理行号**（1 起，便于对照文件）；「行」的计数
// 按**逻辑行**（cliparse.SplitStatements 的执行单元：未引用换行分隔，跨行引号值算一条）——
// 单行语句时二者一致。语句原文折成单行并限长，避免把整段 user-data 倒出来。
func reportScriptStop(w io.Writer, lineNo int, stmt, reason string, remaining int) {
	fmt.Fprintf(w, "%% 脚本在第 %d 行停止执行：%s\n", lineNo, scriptStatementBrief(stmt))
	fmt.Fprintf(w, "%% 原因：%s\n", reason)
	fmt.Fprintf(w, "%% 其余 %d 行未执行\n", remaining)
}

// scriptStatementBrief 把一条语句渲染成**单行**摘要：多行引号值折叠为一行，过长截断。
// 报告要让人一眼看清「是哪一行」，而不是把值里的换行与整段内容原样倒出来。
func scriptStatementBrief(stmt string) string {
	s := strings.ReplaceAll(stmt, "\n", " ")
	const maxBrief = 200
	if r := []rune(s); len(r) > maxBrief {
		return string(r[:maxBrief]) + "…"
	}
	return s
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
