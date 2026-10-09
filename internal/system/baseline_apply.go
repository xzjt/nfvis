package system

// 系统基线（主机名 / 时区 / NTP / 宿主解析器）的**宿主落地**（FR-SYS-001；决策 #424）。
//
// 由来（v3 走查真机实测）：`set system hostname`、`set system timezone`、
// `set system ntp server` 三条此前只写入配置库——宿主 `/etc/hostname`、`/etc/localtime`、
// chrony 的源文件全无变化，而控制台与 `/system/status` 的主机名显示的是**配置值**，
// 于是「用户可见面」与「宿主实况」长期不一致；NTP 声明对实际同步毫无作用
// （`request system ntp sync` 只跑 `chronyc makestep`，用的是 chrony 自己的源）。
// 同一形态的还有 `set system dns server`（宿主解析器上游）——只存不落。
//
// 本组件把四项落到宿主：
//
//   - 主机名：`hostnamectl set-hostname <name>`，并把 `/etc/hostname` 核对到一致
//     （两条独立事实源：`hostnamectl hostname` 与文件内容）；
//   - 时区：`timedatectl set-timezone <tz>`（读回判据 `timedatectl show -p Timezone`）；
//   - NTP：写 chrony 源 drop-in `chronySourcesPath`（`server`/`pool` 行，`prefer` 映射为
//     `prefer` 选项），随后热重载：`chronyc reload sources` → `systemctl reload chrony` →
//     `systemctl restart chrony` 逐级退让；**本机没有 chrony 时如实跳过**（不写文件、
//     不假装成功，说明进日志）；
//   - 宿主解析器：写 systemd-resolved drop-in `resolvedDropInPath` 并重启
//     `systemd-resolved`（resolved 的配置变更只有重启是确定性生效路径，故不用 SIGHUP；
//     未运行时如实跳过）。
//
// 应用时机：nfvisd 启动一次 + 每次提交成功后（与证书 / 日志保留 / 主机防火墙同一条
// 「提交后落实」通道，见 cmd/nfvisd 的 OnCommitted 回调）。**失败不静默、也不阻塞**：
// 逐项结果经返回值上报，调用方逐条 Warn（失败含原因与自查路径；跳过含说明），
// 提交已成功的事实不受影响——与证书、日志保留、主机防火墙的既有语义一致。
// 失败项还经 Failures 逐项进告警表（`SYSTEM_BASELINE_APPLY_FAILED`，决策 #428）：
// 运维在告警面板看得到，下一次应用该项成功或该项声明已空即自动消解。
//
// 幂等：先比对现状（值未变不写文件、不起写子进程；NTP/解析器的 drop-in 内容一致即不重载），
// 配置为空即跳过；NTP / 解析器配置清空时回收本产品写的 drop-in，回到宿主原有来源。

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/xzjt/nfvis/internal/model"
)

const (
	// chronySourcesPath chrony 源 drop-in（chrony.conf 的 sourcedir 会纳入本目录的 *.sources）。
	chronySourcesPath = "/etc/chrony/sources.d/nfvis.sources"
	// resolvedDropInPath systemd-resolved 的 drop-in（宿主解析器的全局上游）。
	resolvedDropInPath = "/etc/systemd/resolved.conf.d/nfvis-dns.conf"
	// hostnameFilePath 静态主机名文件（hostnamectl 会写它；本组件核对它与声明值一致）。
	hostnameFilePath = "/etc/hostname"
)

// BaselineAction 一项系统基线的处置结果（读视图/测试与日志共用同一套取值）。
type BaselineAction string

const (
	// BaselineApplied 本次已落到宿主（Actions 里给出实际执行的动作）。
	BaselineApplied BaselineAction = "applied"
	// BaselineUnchanged 宿主已是声明值：本次未执行任何写动作。
	BaselineUnchanged BaselineAction = "unchanged"
	// BaselineSkipped 未配置，或本机不具备条件（如未安装 chrony / 未运行 systemd-resolved），
	// 说明见 Result.Notes——**如实跳过，不假装成功**。
	BaselineSkipped BaselineAction = "skipped"
	// BaselineFailed 执行失败（原因经 Apply 的 error 返回，含自查路径）。
	BaselineFailed BaselineAction = "failed"
)

// BaselineApplyFailedAlarmCode 系统基线应用失败告警码（决策 #428）。
//
// severity warning；告警键（scope/source）由调用方的落点侧给出（source=system/baseline）。
// 与 SystemBaselineResult.Failures（逐项失败清单）同源：调用方逐项报出，下一次应用该项成功
// 或该项声明已空即按对账口径消解。
const BaselineApplyFailedAlarmCode = "SYSTEM_BASELINE_APPLY_FAILED"

// BaselineItemFailure 一项系统基线应用失败的事实（告警文案与日志共用同一来源，调用方无需
// 解析 Apply 的汇总错误串即可逐项落告警）。
type BaselineItemFailure struct {
	// Item 项名（主机名 / 时区 / NTP / 宿主解析器）。
	Item string
	// Err 失败原因（含自查命令；与 Apply 的 error 返回值逐字同源）。
	Err string
}

// SystemBaselineResult 一次系统基线应用的结果。
type SystemBaselineResult struct {
	Hostname BaselineAction
	Timezone BaselineAction
	NTP      BaselineAction
	DNS      BaselineAction
	// Actions 本次实际执行的动作（命令/写入路径），供调用方留日志痕迹。
	Actions []string
	// Notes 如实说明：跳过原因、被忽略的非法取值等（不含失败原因——那些在 error 里）。
	Notes []string
	// Failures 逐项失败清单（按固定项序：主机名 → 时区 → NTP → 宿主解析器）。
	// error 返回值是它的 errors.Join 汇总；调用方据此逐项落告警（决策 #428）。
	Failures []BaselineItemFailure
}

// SystemBaselineApplier 系统基线的宿主落地器：命令经 Runner 注入、文件根经 Root 注入
// （单测不触碰宿主），与证书 / 日志保留 / 主机防火墙的既有落地器同一形态。
type SystemBaselineApplier struct {
	Runner   Runner                            // 宿主命令（nil = 直接执行；生产注入带超时的执行器）
	Root     string                            // 配置根（默认 "/"；单测注入临时目录）
	LookPath func(file string) (string, error) // 组件存在性判据（nil = exec.LookPath）

	mu sync.Mutex // 串行化 Apply（启动与提交回调可能并发抵达）
}

// NewSystemBaselineApplier 构造真实系统上的落地器。
func NewSystemBaselineApplier(run Runner) *SystemBaselineApplier {
	return &SystemBaselineApplier{Runner: run}
}

// Apply 把 committed 系统基线落到宿主（幂等）。
//
// error 汇总**失败项**（errors.Join，逐项互不牵连）；跳过与说明记在 Result.Notes；
// 逐项失败清单另见 Result.Failures（告警落点据此逐项报出，决策 #428）。
// 调用方（启动 / 提交后回调）据此逐条记日志，**一律不阻塞**提交或启动。
func (a *SystemBaselineApplier) Apply(ctx context.Context, sys *model.SystemConfig) (SystemBaselineResult, error) {
	var res SystemBaselineResult
	if a == nil || sys == nil {
		return res, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	// 逐项固定顺序（与结果字段/失败清单同序）：主机名 → 时区 → NTP → 宿主解析器。
	steps := []struct {
		item string
		step baselineStep
	}{
		{"主机名", a.applyHostname(ctx, sys.Hostname)},
		{"时区", a.applyTimezone(ctx, sys.Timezone)},
		{"NTP", a.applyChronySources(ctx, sys.Ntp)},
		{"宿主解析器", a.applyResolvedDNS(ctx, sys.DNSServers)},
	}
	res.Hostname, res.Timezone, res.NTP, res.DNS =
		steps[0].step.action, steps[1].step.action, steps[2].step.action, steps[3].step.action
	var errs []error
	for _, s := range steps {
		res.Actions = append(res.Actions, s.step.actions...)
		res.Notes = append(res.Notes, s.step.notes...)
		if s.step.err != nil {
			errs = append(errs, s.step.err)
			// 逐项失败清单（决策 #428）：调用方据此把失败注入告警表，无需解析汇总错误串。
			res.Failures = append(res.Failures, BaselineItemFailure{Item: s.item, Err: s.step.err.Error()})
		}
	}
	return res, errors.Join(errs...)
}

// baselineStep 单项的处置结果（内部结构，随 Apply 合流）。
type baselineStep struct {
	action  BaselineAction
	actions []string
	notes   []string
	err     error
}

// applyHostname 落实主机名：hostnamectl + /etc/hostname 一致（两条事实源都核对到才算落地）。
func (a *SystemBaselineApplier) applyHostname(ctx context.Context, want string) baselineStep {
	want = strings.TrimSpace(want)
	if want == "" {
		return baselineStep{action: BaselineSkipped}
	}
	// 幂等判据：运行态主机名与 /etc/hostname **都**是声明值才跳过；探测失败按未知处理、
	// 走应用路径（宁可多跑一次幂等的 set-hostname，也不把「查不出来」当「已是」）。
	cur, _ := a.run(ctx, "hostnamectl", "hostname")
	if strings.TrimSpace(cur) == want && a.fileEquals(hostnameFilePath, want) {
		return baselineStep{action: BaselineUnchanged}
	}
	out, err := a.run(ctx, "hostnamectl", "set-hostname", want)
	if err != nil {
		return baselineStep{action: BaselineFailed, err: fmt.Errorf(
			"主机名 %q 未落到宿主（hostnamectl set-hostname 失败: %v %s）；自查：hostnamectl hostname、systemctl status systemd-hostnamed",
			want, err, strings.TrimSpace(out))}
	}
	step := baselineStep{action: BaselineApplied, actions: []string{"hostnamectl set-hostname " + want}}
	if err := a.ensureHostnameFile(want); err != nil {
		step.action = BaselineFailed
		step.err = fmt.Errorf("主机名已由 hostnamectl 设置，但 %s 与声明值不一致: %w（自查：cat %s）",
			a.path(hostnameFilePath), err, a.path(hostnameFilePath))
	}
	return step
}

// applyTimezone 落实时区：值未变不打 set-timezone。
func (a *SystemBaselineApplier) applyTimezone(ctx context.Context, want string) baselineStep {
	want = strings.TrimSpace(want)
	if want == "" {
		return baselineStep{action: BaselineSkipped}
	}
	if cur, err := a.run(ctx, "timedatectl", "show", "-p", "Timezone", "--value"); err == nil &&
		strings.TrimSpace(cur) == want {
		return baselineStep{action: BaselineUnchanged}
	}
	out, err := a.run(ctx, "timedatectl", "set-timezone", want)
	if err != nil {
		return baselineStep{action: BaselineFailed, err: fmt.Errorf(
			"时区 %q 未落到宿主（timedatectl set-timezone 失败: %v %s）；自查：timedatectl、timedatectl list-timezones",
			want, err, strings.TrimSpace(out))}
	}
	return baselineStep{action: BaselineApplied, actions: []string{"timedatectl set-timezone " + want}}
}

// applyChronySources 落实 NTP 服务器声明：写本产品的源 drop-in 并热重载；未安装 chrony 如实跳过。
func (a *SystemBaselineApplier) applyChronySources(ctx context.Context, servers []model.NtpServer) baselineStep {
	want := RenderChronySources(servers)
	p := a.path(chronySourcesPath)
	prev, readErr := os.ReadFile(p)
	present := readErr == nil

	if want == "" {
		if !present {
			return baselineStep{action: BaselineSkipped} // 未声明且无残留：无动作
		}
		if err := os.Remove(p); err != nil {
			return baselineStep{action: BaselineFailed, err: fmt.Errorf("回收 NTP 源 drop-in %s: %w", p, err)}
		}
		step := baselineStep{action: BaselineApplied, actions: []string{"移除 " + p}}
		if !a.hasChrony() {
			step.notes = append(step.notes,
				"本机未安装 chrony（未找到 chronyc）：已移除 NFViS 的 NTP 源 drop-in，未触发重载")
			return step
		}
		via, err := a.reloadChrony(ctx)
		if err != nil {
			step.action = BaselineFailed
			step.err = fmt.Errorf("已移除 %s，但 chrony 重载失败: %w；自查：chronyc sources、systemctl status chrony", p, err)
			return step
		}
		step.actions = append(step.actions, via)
		return step
	}

	if present && string(prev) == want {
		return baselineStep{action: BaselineUnchanged} // 内容已是声明值：无动作可做
	}
	if !a.hasChrony() {
		return baselineStep{action: BaselineSkipped, notes: []string{
			"本机未安装 chrony（未找到 chronyc）：NTP 服务器声明未落地；安装 chrony 后重新提交配置（或重启 nfvis）即会应用"}}
	}
	if err := writeFile(p, want); err != nil {
		return baselineStep{action: BaselineFailed, err: fmt.Errorf("写入 NTP 源 drop-in %s: %w", p, err)}
	}
	step := baselineStep{action: BaselineApplied, actions: []string{"写入 " + p}}
	via, err := a.reloadChrony(ctx)
	if err != nil {
		step.action = BaselineFailed
		step.err = fmt.Errorf("已写入 %s，但 chrony 重载失败（重载成功前新源不生效）: %w；自查：chronyc sources、systemctl status chrony", p, err)
		return step
	}
	step.actions = append(step.actions, via)
	return step
}

// applyResolvedDNS 落实宿主解析器上游（systemd-resolved drop-in）；不成或未运行时如实跳过。
func (a *SystemBaselineApplier) applyResolvedDNS(ctx context.Context, servers []string) baselineStep {
	want, ignored := RenderResolvedDropIn(servers)
	var notes []string
	if len(ignored) > 0 {
		notes = append(notes, fmt.Sprintf(
			"宿主解析器忽略非 IP 取值 %s（上游只能是 IP 地址，请核对 set system dns server 的取值）",
			strings.Join(ignored, "、")))
	}
	p := a.path(resolvedDropInPath)
	prev, readErr := os.ReadFile(p)
	present := readErr == nil

	if want == "" {
		if len(servers) > 0 && len(ignored) == len(servers) {
			return baselineStep{action: BaselineFailed, notes: notes, err: errors.New(
				"宿主解析器未应用：声明的地址都不是合法 IP（例：set system dns server 8.8.8.8）")}
		}
		if !present {
			return baselineStep{action: BaselineSkipped, notes: notes}
		}
		if !a.resolvedActive(ctx) {
			return baselineStep{action: BaselineSkipped, notes: append(notes,
				"本机 systemd-resolved 未运行：未变更宿主解析器配置（如实跳过）")}
		}
		if err := os.Remove(p); err != nil {
			return baselineStep{action: BaselineFailed, notes: notes,
				err: fmt.Errorf("回收宿主解析器 drop-in %s: %w", p, err)}
		}
		if out, err := a.run(ctx, "systemctl", "restart", "systemd-resolved"); err != nil {
			return baselineStep{action: BaselineFailed, notes: notes, err: fmt.Errorf(
				"已移除 %s，但重启 systemd-resolved 失败: %v %s；自查：resolvectl status、systemctl status systemd-resolved",
				p, err, strings.TrimSpace(out))}
		}
		return baselineStep{action: BaselineApplied, notes: notes,
			actions: []string{"移除 " + p, "systemctl restart systemd-resolved"}}
	}

	if present && string(prev) == want {
		return baselineStep{action: BaselineUnchanged, notes: notes} // 内容已是声明值：不重启解析器
	}
	if !a.resolvedActive(ctx) {
		return baselineStep{action: BaselineSkipped, notes: append(notes,
			"本机 systemd-resolved 未运行：宿主解析器声明未落地（如实跳过）")}
	}
	if err := writeFile(p, want); err != nil {
		return baselineStep{action: BaselineFailed, notes: notes,
			err: fmt.Errorf("写入宿主解析器 drop-in %s: %w", p, err)}
	}
	// systemd-resolved 的配置变更只有重启是确定性生效路径（SIGHUP 在旧版本只刷缓存），
	// 故这里显式 restart；代价是清空解析缓存、丢掉已建的 DNS over TCP 连接（如实记录）。
	if out, err := a.run(ctx, "systemctl", "restart", "systemd-resolved"); err != nil {
		return baselineStep{action: BaselineFailed, notes: notes, err: fmt.Errorf(
			"已写入 %s，但重启 systemd-resolved 失败（新上游未生效）: %v %s；自查：resolvectl status、systemctl status systemd-resolved",
			p, err, strings.TrimSpace(out))}
	}
	return baselineStep{action: BaselineApplied, notes: notes,
		actions: []string{"写入 " + p, "systemctl restart systemd-resolved"}}
}

// RenderChronySources 渲染 chrony 源 drop-in 内容（纯函数，便于单测钉住字面）。
//
// 映射：取值是 IP ⇒ `server` 行，是主机名 ⇒ `pool` 行（chrony 的 pool 会跟踪该名字的
// 地址变化）；`prefer` 映射为 `prefer` 选项；多服务器**保持配置顺序**。空列表返回 ""。
func RenderChronySources(servers []model.NtpServer) string {
	var lines []string
	for _, s := range servers {
		host := strings.TrimSpace(s.Server)
		if host == "" {
			continue
		}
		directive := "pool"
		if _, err := netip.ParseAddr(host); err == nil {
			directive = "server"
		}
		line := directive + " " + host
		if s.Prefer {
			line += " prefer"
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	return "# 由 NFViS 生成（NTP 服务器声明，按产品配置渲染；请勿手工编辑）\n" +
		strings.Join(lines, "\n") + "\n"
}

// RenderResolvedDropIn 渲染 systemd-resolved drop-in 内容（纯函数）。
// 返回内容（"" = 无有效地址）与被忽略的非 IP 取值（如实报告，不静默丢弃）。
func RenderResolvedDropIn(servers []string) (string, []string) {
	var ok, ignored []string
	for _, s := range servers {
		v := strings.TrimSpace(s)
		if v == "" {
			continue
		}
		if _, err := netip.ParseAddr(v); err != nil {
			ignored = append(ignored, v)
			continue
		}
		ok = append(ok, v)
	}
	if len(ok) == 0 {
		return "", ignored
	}
	return "# 由 NFViS 生成（宿主解析器上游，按产品配置渲染；请勿手工编辑）\n" +
		"[Resolve]\nDNS=" + strings.Join(ok, " ") + "\n", ignored
}

// reloadChrony 让 chrony 重新读取源文件：chronyc reload sources → systemctl reload chrony
// → systemctl restart chrony 逐级退让，返回实际生效的方式（供日志留痕）。
func (a *SystemBaselineApplier) reloadChrony(ctx context.Context) (string, error) {
	attempts := [][]string{
		{"chronyc", "reload", "sources"},
		{"systemctl", "reload", "chrony"},
		{"systemctl", "restart", "chrony"},
	}
	failures := make([]string, 0, len(attempts))
	for _, argv := range attempts {
		out, err := a.run(ctx, argv[0], argv[1:]...)
		if err == nil {
			return strings.Join(argv, " "), nil
		}
		failures = append(failures, fmt.Sprintf("%s: %v %s",
			strings.Join(argv, " "), err, strings.TrimSpace(out)))
	}
	return "", errors.New(strings.Join(failures, "；"))
}

// hasChrony chrony 是否在本机（判据 = chronyc 命令存在）。
func (a *SystemBaselineApplier) hasChrony() bool {
	_, err := a.lookPath()("chronyc")
	return err == nil
}

// resolvedActive systemd-resolved 是否在运行（未运行时不做任何变更：写进去也不会生效，
// 如实跳过比「写了就算成功」诚实）。
func (a *SystemBaselineApplier) resolvedActive(ctx context.Context) bool {
	out, _ := a.run(ctx, "systemctl", "is-active", "systemd-resolved")
	return strings.TrimSpace(out) == "active"
}

// ensureHostnameFile 核对/补齐 /etc/hostname 与声明主机名一致（hostnamectl 会写它，
// 这里是第二条保证：写失败或文件被改都能收敛回来）。
func (a *SystemBaselineApplier) ensureHostnameFile(want string) error {
	p := a.path(hostnameFilePath)
	if cur, err := os.ReadFile(p); err == nil {
		if strings.TrimSpace(string(cur)) == want {
			return nil
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("读取 %s: %w", p, err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(want+"\n"), 0o644)
}

// fileEquals 读 root 下相对路径文件并与期望值比对（去首尾空白）。
func (a *SystemBaselineApplier) fileEquals(rel, want string) bool {
	b, err := os.ReadFile(a.path(rel))
	return err == nil && strings.TrimSpace(string(b)) == want
}

// path 把绝对路径映射到注入的配置根下（root 为空或 "/" 即原路径）。
func (a *SystemBaselineApplier) path(rel string) string { return join(a.Root, rel) }

// lookPath 组件存在性判据（可注入；单测据此模拟「本机没有 chrony」）。
func (a *SystemBaselineApplier) lookPath() func(string) (string, error) {
	if a.LookPath != nil {
		return a.LookPath
	}
	return exec.LookPath
}

// run 执行宿主命令（Runner 未注入时直接执行；生产路径注入带超时的执行器）。
func (a *SystemBaselineApplier) run(ctx context.Context, name string, args ...string) (string, error) {
	if a.Runner != nil {
		return a.Runner(ctx, name, args...)
	}
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}
