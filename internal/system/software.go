// M5-7：软件升级/回退与电源、NTP（FR-OPS-001~003）。
//
// 语义：
//   - add：本地 .deb 或 http(s) URL（可选 sha256 强校验）→ 校验包名/依赖（`dpkg-deb -f`）
//     → 归档到软件目录（供 rollback）→ `dpkg -i` 安装（postinst 负责重启 nfvisd）→ 读回版本报告。
//     安装**脱离 nfvisd 的 cgroup** 执行、并以 dpkg 数据库状态复核成败（见 installOnce / installAndVerify）：
//     postinst 重启 nfvisd 时不能连带杀死 dpkg，命令的退出路径也不能当作安装结果。
//   - rollback：从归档目录选**版本最高且非当前**的 .deb → `dpkg -i` 回退（FR-OPS-002）。
//   - reboot/shutdown：`systemctl reboot|poweroff`（调用方须已完成确认）。
//   - ntp sync：优先 chronyc，其次 ntpdate（取配置服务器），最后 systemd-timesyncd 重启。
//
// 底座命令经 Runner 注入，单测在任意平台用假实现覆盖；govpp/系统命令适配见 main.go。
package system

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// DefaultSoftwareDir 软件包归档目录（保留历史版本供回退）。
const DefaultSoftwareDir = "/var/lib/nfvis/software"

// 安装日志：安装输出经**文件**取回（不带管道），见 installOnce。
const installLogName = "install.log"

// dpkg 数据库里「已安装且已配置」的唯一状态行。
const pkgStatusInstalled = "install ok installed"

// Runner 执行宿主命令（dpkg/systemctl/chronyc…），返回合并输出。
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// SoftwareResult 升级/回退结果。
type SoftwareResult struct {
	Action   string `json:"action"` // add|rollback
	Version  string `json:"version"`
	Previous string `json:"previous,omitempty"`
	Package  string `json:"package,omitempty"`
	Output   string `json:"output,omitempty"`
	// OK 本次动作是否落定为成功（修 BUG-16：结论要能在连接断开后仍被读到）。
	OK bool `json:"ok,omitempty"`
	// Status 复核时的 dpkg 状态行（如 `install ok installed`；取不到则为空）。
	Status string `json:"status,omitempty"`
}

// LastResultPath 最近一次升级/回退的结论落点（修 BUG-16）。
//
// 为什么必须落盘：`software add` 会重启 nfvisd，承载该命令的 HTTP 连接随之 EOF——操作者
// 只看到「连接 nfvisd 失败」+ 非零退出码，会误判失败而重试或走回退，而升级其实已成功。
// 结论写在**不随进程消失**的文件里，CLI 重连后就能读到「到底成没成」，不靠猜、也不必
// 为此新增一条读命令（命令树是契约，为一次复核扩命令面不划算）。
const LastResultPath = DefaultSoftwareDir + "/last-result.json"

// RecordResult 把结论落到 LastResultPath（尽力而为：写不成不影响升级本身）。
func (m *SoftwareManager) RecordResult(res SoftwareResult) {
	if m == nil || m.dir == "" {
		return
	}
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(m.dir, "last-result.json"), append(b, '\n'), 0o644)
}

// ReadLastResult 读最近一次结论（供升级后复核；文件不存在或损坏返回 ok=false）。
func ReadLastResult() (SoftwareResult, bool) { return ReadLastResultAt(LastResultPath) }

// ReadLastResultAt 按给定路径读结论（读口路径可注入以便单测覆盖三种形态）。
func ReadLastResultAt(path string) (SoftwareResult, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return SoftwareResult{}, false
	}
	var res SoftwareResult
	if json.Unmarshal(raw, &res) != nil {
		return SoftwareResult{}, false
	}
	return res, res.Version != "" || res.Status != ""
}

// SoftwareManager 软件升级管理。
type SoftwareManager struct {
	dir     string
	run     Runner
	version func() string
	client  *http.Client
	now     func() time.Time
	// lookPath 判定脱离手段是否可用（nil = exec.LookPath）。可注入以便单测在任意平台覆盖
	// 「有 systemd-run」与「只有 setsid」两条路径。
	lookPath func(string) (string, error)
	// detach 本次选定的脱离手段（首次探测后缓存；见 detacher）。
	detach string
}

// NewSoftwareManager 构造（run/version 必填；dir 空取缺省）。
func NewSoftwareManager(dir string, run Runner, version func() string) *SoftwareManager {
	if dir == "" {
		dir = DefaultSoftwareDir
	}
	return &SoftwareManager{dir: dir, run: run, version: version,
		client: &http.Client{Timeout: 10 * time.Minute}, now: time.Now, lookPath: exec.LookPath}
}

var debVersionRe = regexp.MustCompile(`^nfvis_(.+)_(amd64|arm64|all)\.deb$`)

// currentVersion 读取**已安装**的 nfvis 版本（dpkg-query 为准；查不到时回落到编译期版本）。
// 回退判定必须用系统实际安装版本，而非运行中进程的编译版本（后者在升级后仍为旧值）。
func (m *SoftwareManager) currentVersion(ctx context.Context) string {
	if out, err := m.run(ctx, "dpkg-query", "-W", "-f=${Version}", "nfvis"); err == nil {
		if v := strings.TrimSpace(out); v != "" {
			return v
		}
	}
	return m.version()
}

// installAttempt 一次安装尝试：cmdOut 是脱离手段（systemd-run/setsid）自身的诊断输出，
// log 是 dpkg/postinst 的实际输出（经文件取回，见 installOnce）。两者都是操作者可见的事实——
// 判前端锁与报错都要看全部文本。
type installAttempt struct {
	cmdOut string
	log    string
	err    error
}

// text 合并本次尝试的全部输出。
func (a installAttempt) text() string { return joinLines(a.cmdOut, a.log) }

// 复核节奏（测试可缩短）。都是**判定**用的上界，不参与安装本身的超时。
var (
	installConvergeTimeout  = 60 * time.Second // 命令被打断后等 dpkg 自己跑完的上界
	installConvergeInterval = 2 * time.Second
	installVerifyTimeout    = 2 * time.Minute // 一次状态复核/补救的上界
)

// Add 安装软件包（本地路径或 URL）。
func (m *SoftwareManager) Add(ctx context.Context, pkg, expectSHA string) (SoftwareResult, error) {
	res := SoftwareResult{Action: "add", Previous: m.currentVersion(ctx)}
	path := pkg
	if isURL(pkg) {
		p, err := m.download(ctx, pkg, expectSHA)
		if err != nil {
			return res, err
		}
		defer os.Remove(p) // 下载件移到归档后即删（归档名按版本规范）
		path = p
	} else {
		if _, err := os.Stat(path); err != nil {
			return res, fmt.Errorf("软件包不存在: %s", pkg)
		}
		if expectSHA != "" {
			if err := verifySHA(path, expectSHA); err != nil {
				return res, err
			}
		}
	}

	// 包名/类型校验（防误装）：dpkg-deb -f <pkg> Package
	nameOut, err := m.run(ctx, "dpkg-deb", "-f", path, "Package")
	if err != nil {
		return res, fmt.Errorf("校验软件包（dpkg-deb）: %w: %s", err, strings.TrimSpace(nameOut))
	}
	if pkgName := strings.TrimSpace(nameOut); pkgName != "nfvis" {
		return res, fmt.Errorf("软件包 %q 不是 nfvis（Package=%s）", filepath.Base(path), pkgName)
	}
	verOut, err := m.run(ctx, "dpkg-deb", "-f", path, "Version")
	if err != nil {
		return res, fmt.Errorf("读取软件包版本: %w", err)
	}
	newVer := strings.TrimSpace(verOut)
	if newVer == "" {
		return res, errors.New("软件包版本为空")
	}

	// 归档（供回退）后安装
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return res, fmt.Errorf("创建软件目录: %w", err)
	}
	archived := filepath.Join(m.dir, fmt.Sprintf("nfvis_%s_amd64.deb", newVer))
	if err := copyFileTo(path, archived); err != nil {
		return res, fmt.Errorf("归档软件包: %w", err)
	}
	out, err := m.installAndVerify(ctx, archived, newVer, "安装")
	if err != nil {
		// 修 BUG-16：失败也要留结论——连接可能已随 nfvisd 重启断掉，CLI 重连后靠这份
		// 落盘结论才知道「到底成没成」，而不是只看到 EOF。
		m.RecordResult(SoftwareResult{Action: "add", Version: newVer, Package: filepath.Base(archived),
			Previous: res.Previous, Status: "安装失败: " + err.Error(), OK: false})
		return res, err
	}
	res.Package, res.Output = filepath.Base(archived), strings.TrimSpace(out)
	// 以包内版本为准报告（安装后版本读回由 dpkg-query 另行校验，避免时钟/缓存歧义）
	res.Version = newVer
	res.OK = true
	res.Status = pkgStatusInstalled
	m.RecordResult(res)
	return res, nil
}

// Rollback 回退到归档中的上一个版本（FR-OPS-002）。
func (m *SoftwareManager) Rollback(ctx context.Context) (SoftwareResult, error) {
	res := SoftwareResult{Action: "rollback", Previous: m.currentVersion(ctx)}
	cands, err := m.archived()
	if err != nil {
		return res, err
	}
	if len(cands) == 0 {
		return res, fmt.Errorf("无可回退的版本：归档目录 %s 为空（仅能回退到经 request system software add 安装过的版本）", m.dir)
	}
	// 排除当前版本，取版本最高者
	cur := m.currentVersion(ctx)
	var pick string
	for _, c := range cands { // archived 已按版本升序
		if c.version != cur {
			pick = c.path
			res.Version = c.version
		}
	}
	if pick == "" {
		return res, fmt.Errorf("无可回退的版本：归档中仅有当前版本 %s（%d 个包）", cur, len(cands))
	}
	out, err := m.installAndVerify(ctx, pick, res.Version, "回退")
	if err != nil {
		m.RecordResult(SoftwareResult{Action: "rollback", Version: res.Version,
			Package: filepath.Base(pick), Status: "回退失败: " + err.Error(), OK: false})
		return res, err
	}
	res.Package, res.Output = filepath.Base(pick), strings.TrimSpace(out)
	// 版本读回不继承调用方 ctx：回退同样会重启 nfvisd，那个 ctx 可能已随重启失效。
	res.Version = m.currentVersion(context.Background())
	res.OK = true
	res.Status = pkgStatusInstalled
	m.RecordResult(res)
	return res, nil
}

// archived 列出归档包（按版本升序；版本比较用 dpkg --compare-versions 等价的简化排序）。
func (m *SoftwareManager) archived() ([]struct{ version, path string }, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]struct{ version, path string }, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		mx := debVersionRe.FindStringSubmatch(e.Name())
		if mx == nil {
			continue
		}
		out = append(out, struct{ version, path string }{mx[1], filepath.Join(m.dir, e.Name())})
	}
	sort.Slice(out, func(i, j int) bool { return compareVersion(out[i].version, out[j].version) < 0 })
	return out, nil
}

// compareVersion 版本比较（数字段逐段比较，非数字段字典序；够用即可）。
func compareVersion(a, b string) int {
	as, bs := strings.FieldsFunc(a, func(r rune) bool { return r == '.' || r == '-' || r == '+' || r == '~' }), strings.FieldsFunc(b, func(r rune) bool { return r == '.' || r == '-' || r == '+' || r == '~' })
	for i := 0; i < len(as) && i < len(bs); i++ {
		ai, aerr := parseInt(as[i])
		bi, berr := parseInt(bs[i])
		switch {
		case aerr == nil && berr == nil:
			if ai != bi {
				if ai < bi {
					return -1
				}
				return 1
			}
		default:
			if as[i] != bs[i] {
				if as[i] < bs[i] {
					return -1
				}
				return 1
			}
		}
	}
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	}
	return 0
}

func parseInt(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, errors.New("empty")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

// installAndVerify 安装/回退的统一收口：下发 → 以 dpkg 数据库里的**真实状态与版本**判定结果。
//
// 为什么不能看命令退出码：升级时 postinst 会 `systemctl try-restart nfvis.service`，而本函数
// 的调用方（CLI/REST）就活在那个被重启的进程里——命令侧看到的「被终止」与「包是否装好」
// 是两件事（真机 v3.0.24：报 signal: terminated，包随后是 iF）。故：
//  1. 命令**被打断**（信号/上下文取消）时，先给 dpkg 一段收敛时间——它多半还在跑；
//  2. 判定只认 `dpkg-query` 的 `install ok installed`，且**版本必须是本次的目标版本**——
//     升级前旧包本来也是 installed，只看状态会把「dpkg 根本没跑」误判成升级成功；
//  3. 停在 half-configured 等「已解包未配置」的中间态时，补救执行一次 `dpkg --configure nfvis`；
//  4. 仍不到位就如实报出**真实状态**与**恢复命令**，不谎称原因、也不承诺并不存在的自动回退。
func (m *SoftwareManager) installAndVerify(ctx context.Context, path, targetVer, action string) (string, error) {
	at := m.installDeb(ctx, path, action)
	if at.err != nil && looksInterrupted(at.err) {
		at.log = joinOutput(at.log, m.awaitTerminalStatus(targetVer))
	}
	status, serr := m.packageStatus()
	installed, verr := m.installedVersion()
	if installSettled(status, serr, installed, verr, targetVer) {
		return m.installedReport(action, at, status), nil
	}
	// 复核拿不到可判读的状态行时**不能**据此判失败——那只是「这个执行通道答不了这个问题」，
	// 旧口径（以命令结果为准）必须保留，否则任何不支持 dpkg-query 的适配都会让升级恒报错。
	if !statusKnown(status, serr) {
		if at.err == nil {
			return m.installedReport(action, at, ""), nil
		}
		return m.installFailure(action, path, status, serr, installed, verr, at, "")
	}
	// 只对「已解包、未配置」的中间态做补救：一次 configure 就能收口；而「包根本没落地」
	//（not-installed）补救没有意义（那要先重装），直接如实报错。
	// 补救本身会再跑一遍 postinst（同样重启 nfvisd），故与安装走同一条脱离路径。
	recovery := ""
	if isConfigurableStatus(status) {
		rec := m.runDetached(ctx, "dpkg --configure nfvis")
		recovery = "dpkg --configure nfvis: " + strings.TrimSpace(joinLines(rec.text(), errText(rec.err)))
		status, serr = m.packageStatus()
		installed, verr = m.installedVersion()
		if installSettled(status, serr, installed, verr, targetVer) {
			return m.installedReport(action, at, recovery), nil
		}
	}
	return m.installFailure(action, path, status, serr, installed, verr, at, recovery)
}

// installSettled 判定「这次安装真的落定了」：状态必须是 install ok installed，**且**版本是
// 目标版本。两个条件缺一不可——半配置（half-configured/unpacked）时 dpkg 已经把 Version 改成
// 新版本、旧包没被触碰时状态又本来就是 installed：只看其中一个都会得出错误结论。
func installSettled(status string, serr error, installed string, verr error, target string) bool {
	if serr != nil || status != pkgStatusInstalled {
		return false
	}
	if target == "" || verr != nil || installed == "" {
		// 版本这一路取不到证据（适配层不答/查不到）时不据此判失败。
		return true
	}
	return installed == target
}

// installedVersion 读 dpkg 数据库里的 nfvis 版本（与状态分开查：状态不区分版本）。
func (m *SoftwareManager) installedVersion() (string, error) {
	out, err := m.runBounded(installVerifyTimeout, "dpkg-query", "-W", "-f=${Version}", "nfvis")
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
	}
	return strings.TrimSpace(out), nil
}

// statusKnown 判断 dpkg-query 的应答是否是**可判读**的状态行。
// dpkg 的状态行以 want-flag 开头，如 `install ok installed`、`install ok half-configured`、
// `deinstall ok config-files`。不是这个形状的应答说明这一路根本没问到 dpkg，此时「不是
// installed」不代表「装失败」——判据不能建立在一个没被回答的问题上。
func statusKnown(status string, err error) bool {
	if err != nil {
		return false
	}
	s := strings.TrimSpace(status)
	return strings.HasPrefix(s, "install ") || strings.HasPrefix(s, "deinstall ")
}

// installedReport 命令退出路径无论成功还是被打断，包状态已是 installed ⇒ 报「已生效」。
// 操作者最需要知道的是：升级真的发生了、nfvisd 会（或已经）重启——而不是命令的结束码。
func (m *SoftwareManager) installedReport(action string, at installAttempt, extra ...string) string {
	var head string
	if at.err != nil && looksInterrupted(at.err) {
		head = fmt.Sprintf("%s命令的结束状态不可信（%v）——已按 dpkg 数据库复核：包已是 %s，本次%s已实际生效。"+
			"nfvisd 由安装脚本重启以加载新版本；若本会话中途断开，属预期（重启的正是执行本命令的进程）。",
			action, at.err, pkgStatusInstalled, action)
	}
	parts := append([]string{head, at.log}, extra...)
	return strings.TrimSpace(joinLines(parts...))
}

// installFailure 真失败时的报错：点名包的**真实状态**与**照做的恢复命令**。
func (m *SoftwareManager) installFailure(action, path, status string, statusErr error, installed string, verr error, at installAttempt, recovery string) (string, error) {
	state := status
	switch {
	case statusErr != nil:
		state = fmt.Sprintf("读不到（dpkg-query 失败: %v）", statusErr)
	case state == "":
		state = "空（dpkg 数据库里查不到 nfvis）"
	}
	out := at.text()
	if recovery != "" {
		out = joinLines(out, recovery)
	}
	// 恢复命令按状态给出：已解包/半配置 → configure 即可；其余 → 重新下发同一个包。
	var fix string
	if statusErr == nil && isConfigurableStatus(status) {
		fix = fmt.Sprintf("恢复：执行 dpkg --configure nfvis（当前状态 %s）；仍失败时执行 dpkg -i %s 重试。", status, path)
	} else {
		fix = fmt.Sprintf("恢复：执行 dpkg -i %s 重试，随后 dpkg --configure nfvis；仍失败请查看上方 dpkg 输出。", path)
	}
	msg := fmt.Sprintf("%s未完成：包 nfvis 当前状态为 %q（未达到 %q）。%s", action, state, pkgStatusInstalled, fix)
	if verr == nil && installed != "" {
		msg += fmt.Sprintf(" 已安装版本为 %s。", installed)
	}
	if at.err != nil {
		msg += fmt.Sprintf(" 底层错误: %v。", at.err)
	}
	return strings.TrimSpace(out), errors.New(msg)
}

// looksInterrupted 判断命令是「被打断」而非「跑完并报错」：前者 dpkg 的真实结果未知
// （缺陷本身的形态：nfvisd 被安装脚本重启，连带把同一 cgroup 里的 dpkg 杀了），
// 后者 dpkg 已经把结论写在输出里，不必再等它的中间态。
func looksInterrupted(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, sig := range []string{"signal: terminated", "signal: killed", "signal: interrupt", "broken pipe", "eof"} {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

// awaitTerminalStatus 命令被打断后给 dpkg 一段收敛时间：只要包还停在「已解包/半配置」这类
// **仍在推进**的中间态就继续等——过早判定会在包已装好时误报失败，或重复下发同一次安装。
func (m *SoftwareManager) awaitTerminalStatus(targetVer string) string {
	deadline := time.Now().Add(installConvergeTimeout)
	for {
		status, serr := m.packageStatus()
		installed, verr := m.installedVersion()
		// 已装成目标版本 = 收敛完毕；停在不可配置的终态（如完全没落地）也无需再等。
		if installSettled(status, serr, installed, verr, targetVer) {
			return ""
		}
		if serr == nil && !isConfigurableStatus(status) {
			return ""
		}
		if time.Now().After(deadline) {
			return fmt.Sprintf("（等待 %s 后包仍停在 %q，dpkg 可能尚未结束）", installConvergeTimeout, status)
		}
		time.Sleep(installConvergeInterval)
	}
}

// packageStatus 读 dpkg 数据库里 nfvis 的状态行（如 `install ok installed`）。
func (m *SoftwareManager) packageStatus() (string, error) {
	out, err := m.runBounded(installVerifyTimeout, "dpkg-query", "-W", "-f=${Status}", "nfvis")
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
	}
	return strings.TrimSpace(out), nil
}

// isConfigurableStatus 判断状态是否属于「包已解包、只差配置」——一次 dpkg --configure 能收口的形态。
func isConfigurableStatus(status string) bool {
	return strings.Contains(status, "half-configured") || strings.HasSuffix(status, " unpacked")
}

// runBounded 以固定上界执行一条**判定/补救**命令，且不继承调用方 ctx：安装会把执行本命令的
// 进程重启，请求上下文随之取消，但「包到底是什么状态」必须查完。
func (m *SoftwareManager) runBounded(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return m.run(ctx, name, args...)
}

// installDeb 安装 .deb；dpkg 前端锁被 apt/unattended-upgrades 占用时短等重试
// （Ubuntu 定时升级是常态，不应让一次升级因锁竞争失败）。
func (m *SoftwareManager) installDeb(ctx context.Context, path, action string) installAttempt {
	const attempts = 6
	var at installAttempt
	for i := 0; i < attempts; i++ {
		at = m.installOnce(ctx, path, action)
		if at.err == nil {
			return at
		}
		if !strings.Contains(at.text(), "dpkg frontend lock") && !strings.Contains(at.text(), "frontend lock was locked") {
			return at
		}
		select {
		case <-ctx.Done():
			at.err = ctx.Err()
			return at
		case <-time.After(5 * time.Second):
		}
	}
	return at
}

// installLogPath 安装输出的落盘位置：放在软件目录（root 0700，不进 /tmp——那里任何本地用户
// 都能预置同名符号链接），nfvisd 被重启后操作者仍能直接查看。
func (m *SoftwareManager) installLogPath() string {
	return filepath.Join(m.dir, installLogName)
}

// installOnce 一次安装尝试：让 dpkg 在**与 nfvisd 不同的 cgroup** 里执行。
//
// 为什么必须换 cgroup（真机 v3.0.24 实证）：命令经 nfvisd 执行，dpkg 是它的子进程；postinst
// 在升级时会 `systemctl try-restart nfvis.service`，而 systemd 停单元默认按 **control-group**
// 杀——同 cgroup 里的 dpkg 一并收到 SIGTERM，dpkg 于是判定 postinst 失败、把包留在 iF（文件
// 早已解包）。setsid 只换 session 不换 cgroup，挡不住这种杀法；`systemd-run --scope` 把 dpkg
// 放进自持的 scope cgroup，nfvisd 的生死与它无关。
//
// 输出也不走管道：父进程（nfvisd）一死，读端关闭，写管道的那端会拿到 SIGPIPE/EPIPE，
// 等于换了个方式杀死 dpkg。改为重定向到文件、事后读回，操作者照样拿到完整输出。
func (m *SoftwareManager) installOnce(ctx context.Context, path, action string) installAttempt {
	logPath := m.installLogPath()
	_ = os.Remove(logPath) // 上一次的残渣会误导本次排障
	// 修 BUG-16（关键一处）：结论必须由**脱离执行的那个 shell 自己**在 dpkg 结束后写出。
	// 为什么不能等回到本进程再写：postinst 会重启 nfvisd，本进程在那时已被杀——`Add` 里
	// 后面的 RecordResult 根本执行不到（真机实证：升级成功但结论文件不存在）。
	cmdOut, err := m.launchShell(ctx, logPath, m.installScriptWithResult(path, action), "nfvis-install")
	return installAttempt{cmdOut: cmdOut, log: m.readInstallLog(logPath), err: err}
}

// installScriptWithResult dpkg 安装 + **在同一个脱离 shell 里**落结论。
//
// 结论文件格式与 ReadLastResultAt 读取的一致（action/version/status/ok）。版本取
// `dpkg-query` 的实装版本（不是包内声明值——半装时两者会不一致），成败以状态行为准
// （`install ok installed` 是唯一成功态，与 installSettled 同口径）。
func (m *SoftwareManager) installScriptWithResult(path, action string) string {
	act := "add"
	if action == "回退" {
		act = "rollback"
	}
	// 结论由**脱离执行的那个 shell 自己**在 dpkg 结束后写出（nfvisd 那时已被 postinst 杀掉）。
	//
	// 两处关键取舍：
	//   · 状态与版本直接读 **/var/lib/dpkg/status**，不调 `dpkg-query`——后者要拿 dpkg 的锁，
	//     而 postinst 重启期间事务未收尾，真机实证返回空串；
	//   · 结论**写进独立文件**，且整段用 `{ … }` 分组把 awk 的输出重定向到该文件：
	//     launchShell 会在整条命令后追加 `>>install.log 2>&1`，若不加分组，脚本内最后一个
	//     重定向会被外层覆盖，结论落进日志而不是结果文件（真机实证：log 里有正确 JSON、
	//     last-result.json 恒为空）。
	res := filepath.Join(m.dir, "last-result.json")
	script := "dpkg -i " + shellQuote(path) + "; mkdir -p " + shellQuote(m.dir) + "; " +
		"{ awk -v act=" + shellQuote(act) + " '" +
		"BEGIN{st=\"\";ver=\"\"} " +
		"/^Package: nfvis$/{inpkg=1;next} inpkg&&/^Status:/{sub(/^Status: /,\"\");st=$0} " +
		"inpkg&&/^Version:/{sub(/^Version: /,\"\");ver=$0} " +
		"inpkg&&/^$/{inpkg=0} " +
		"END{printf \"{\\\"action\\\":\\\"%s\\\",\\\"version\\\":\\\"%s\\\",\\\"status\\\":\\\"%s\\\",\\\"ok\\\":%s}\\n\", " +
		"act, ver, st, (st==\"install ok installed\"?\"true\":\"false\")}' " +
		"/var/lib/dpkg/status > " + shellQuote(res) + "; }"
	return script
}

// runDetached 执行一条**会重启 nfvisd** 的补救命令（dpkg --configure），同样脱离本进程的
// cgroup 并把输出落到安装日志。它没有可重试的锁语义（configure 撞锁就是真失败），故只跑一次。
func (m *SoftwareManager) runDetached(ctx context.Context, command string) installAttempt {
	logPath := m.installLogPath() + ".configure"
	_ = os.Remove(logPath)
	cmdOut, err := m.launchShell(ctx, logPath, command, "nfvis-configure")
	return installAttempt{cmdOut: cmdOut, log: m.readInstallLog(logPath), err: err}
}

// launchShell 以「与调用方不同 cgroup/session」的形态执行命令，返回脱离手段自身的诊断输出。
//
// 只跑**一次**：手段由 detacher 事先探定，不在失败后改换手段重跑——dpkg 不幂等，换手段再跑
// 一遍就是重复解包同一颗包。手段本身不可用时由 detacher 提前降级，代价是一次 `/bin/true` 探针。
func (m *SoftwareManager) launchShell(ctx context.Context, logPath, command, unitPrefix string) (string, error) {
	script := fmt.Sprintf("%s >>%s 2>&1", command, shellQuote(logPath))
	// 修 BUG-16（第三处，也是最隐蔽的一处）：**脱离执行的命令不能挂调用方的 ctx**。
	//
	// 注入的 Runner 是 `exec.CommandContext`：调用方 ctx 一取消，它立刻 SIGKILL 掉
	// `systemd-run` 客户端进程——而那个客户端正是 scope 的发起者，父进程一死，scope 里的
	// dpkg 与后续脚本一起被收走。真机实证：升级装上了（包状态 installed），但结论文件
	// 恒为空、install.log 只有上一轮残渣——因为写结论的那半截命令从没跑完。
	//
	// 故这里给脱离执行换成**独立 ctx**（context.WithoutCancel 保留值、去掉取消传播），
	// 有界性由 systemd-run 的 scope 自身与 dpkg 事务保证，不再依赖调用方存活。
	detached := context.WithoutCancel(ctx)
	switch m.detacher(ctx) {
	case detachSystemdRun:
		// 系统级 scope：自持 cgroup，重启 nfvisd 波及不到；单元名带固定前缀便于排障
		//（只放 nfvis-install / nfvis-configure 两族，异常残留一眼能认出来）。
		return m.run(detached, "systemd-run", "--scope", "--collect", "--quiet",
			fmt.Sprintf("--unit=%s-%d", unitPrefix, m.now().UnixNano()),
			"/bin/sh", "-c", script)
	case detachSetsid:
		// setsid 把命令放进新 session：无 systemd 的底座上 postinst 的 try-restart 本就不执行
		//（它先判 command -v systemctl），故不存在 cgroup 连坐，脱离 session 已经足够。
		return m.run(detached, "setsid", "/bin/sh", "-c", script)
	}
	// 一个脱离手段都没有：功能仍可用，但回到「被重启波及」的旧风险，必须在输出里写明。
	out, err := m.run(ctx, "/bin/sh", "-c", script)
	return joinLines("未找到可用的 systemd-run/setsid，已直跑（升级可能被 nfvisd 重启波及）", out), err
}

// 脱离手段。
const (
	detachDirect     = "direct"
	detachSetsid     = "setsid"
	detachSystemdRun = "systemd-run"
)

// detacher 探定本次使用的脱离手段（结果缓存：同一进程内底座不会变，反复探测只是多花时间）。
//
// systemd-run 的**存在不等于可用**（容器里没有 /run/systemd/system、D-Bus 不可达时它照样
// 在 PATH 里）。故用 `/bin/true` 真跑一次 scope 探针，只看退出状态——不去猜错误文本。
// 探针失败就退到 setsid；两者都没有才直跑。
func (m *SoftwareManager) detacher(ctx context.Context) string {
	if m.detach != "" {
		return m.detach
	}
	pick := detachDirect
	if _, err := m.lookPathFn()("systemd-run"); err == nil {
		if _, err := m.run(ctx, "systemd-run", "--scope", "--quiet", "/bin/true"); err == nil {
			pick = detachSystemdRun
		}
	}
	if pick == detachDirect {
		if _, err := m.lookPathFn()("setsid"); err == nil {
			pick = detachSetsid
		}
	}
	m.detach = pick
	return pick
}

// lookPathFn 让单测覆盖「有 systemd-run / 只有 setsid / 两者都没有」三种环境。
func (m *SoftwareManager) lookPathFn() func(string) (string, error) {
	if m.lookPath != nil {
		return m.lookPath
	}
	return exec.LookPath
}

// readInstallLog 取回安装输出；文件不存在（dpkg 没跑到）视为空，不编造内容。
func (m *SoftwareManager) readInstallLog(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// shellQuote 用单引号包裹 shell 参数（POSIX）：文件名里的空格/分号不会被当成语法。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// joinOutput 拼接两段输出，忽略空段（不留孤立换行）。
func joinOutput(a, b string) string { return joinLines(a, b) }

// joinLines 以换行拼接非空段；用于把命令输出与判定说明拼成操作者可读的整段文本。
func joinLines(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimRight(p, "\n"); strings.TrimSpace(t) != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, "\n")
}

// errText 把错误渲染成一行（补救命令的诊断行用）。
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Reboot 重启整机（调用方须已完成确认，FR-OPS-003）。
func (m *SoftwareManager) Reboot(ctx context.Context) error {
	_, err := m.run(ctx, "systemctl", "reboot")
	return err
}

// Shutdown 关机（调用方须已完成确认）。
func (m *SoftwareManager) Shutdown(ctx context.Context) error {
	_, err := m.run(ctx, "systemctl", "poweroff")
	return err
}

// NTPSync 立即同步一次时间（chronyc → ntpdate → systemd-timesyncd）。
func (m *SoftwareManager) NTPSync(ctx context.Context, servers []string) (string, error) {
	if out, err := m.run(ctx, "chronyc", "makestep"); err == nil {
		return "chronyc makestep: " + strings.TrimSpace(out), nil
	}
	if len(servers) > 0 {
		args := append([]string{"-u"}, servers...)
		if out, err := m.run(ctx, "ntpdate", args...); err == nil {
			return "ntpdate: " + strings.TrimSpace(out), nil
		}
	}
	if _, err := m.run(ctx, "systemctl", "restart", "systemd-timesyncd"); err == nil {
		return "systemd-timesyncd restarted", nil
	}
	return "", errors.New("无可用 NTP 工具（chronyc/ntpdate/systemd-timesyncd 均不可用）")
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func (m *SoftwareManager) download(ctx context.Context, url, expectSHA string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载软件包 %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("下载软件包 %s: HTTP %d", url, resp.StatusCode)
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(m.dir, "download-*.deb")
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if expectSHA != "" && !strings.EqualFold(sum, expectSHA) {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("sha256 校验失败：期望 %s，实际 %s", expectSHA, sum)
	}
	return f.Name(), nil
}

func verifySHA(path, expect string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, expect) {
		return fmt.Errorf("sha256 校验失败：期望 %s，实际 %s", expect, got)
	}
	return nil
}

// copyFileTo 复制文件（保留源文件；跨设备亦可）。
func copyFileTo(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
