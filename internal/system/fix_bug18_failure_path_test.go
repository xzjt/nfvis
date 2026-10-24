package system

// 修 BUG-18（v3.0.28 复测报告 §3，真机逐秒采样实证）：软件升级的**失败路径**双双谎报成功。
//
//	场景 A（同版本坏包）：dpkg 在校验阶段就拒绝、包根本没动过，而旧包本来就是
//	  `install ok installed` + audit 干净 + 版本==目标（同版本重装）——一切状态判据失明，
//	  命令报「升级完成」rc=0，结论文件 ok:true。
//	场景 B（跨版本坏包）：进程侧判对了失败并落下 ok:false，但脱离脚本的终判
//	  （只看状态+audit，不比对目标版本、不吸收 dpkg 退出码）晚 ~12s 用 ok:true
//	  **覆盖**了正确结论（真机时间线：02:06:30 ok:false → 02:06:31 ok:true）。
//	BUG-19：场景 B 的失败文案「当前状态为 "install ok installed"（未达到 "install ok installed"）」
//	  两侧是同一个字符串——状态达标而版本不达标时，主句读起来像逻辑错误。
//
// 修法（三道闸 + 归档保护，全部在下面钉住）：
//  1. dpkg 退出码经 install.rc 落盘（R44-1）：rc!=0 一票否决「状态全绿」的假成功；
//  2. 脚本终判比对**目标版本**（字面量）并要求 rc==0；结论 version 写目标版本；
//  3. 进程侧判定完成后停掉脱离 unit（rc 已知时）——最后落笔的人唯一；
//  4. 归档走暂存名，rc==0 才晋升——坏包不进回退池（真机副作用：同版本坏包把归档里
//     的好包覆盖掉，哈希实测与 broken.deb 一致）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// bug18Runner 模拟宿主命令，重点模拟**脱离脚本新增的两件事**：
//   - dpkg 退出后把 `<attempt> <rc>` 落到 install.rc（进程侧判定靠它）；
//   - rc==0 时执行归档晋升（mv staging canonical），rc!=0 时不动（坏包不进回退池）。
type bug18Runner struct {
	dir         string // 软件目录（install.rc / last-result.json / 归档所在）
	status      string
	version     string
	targetVer   string
	onInstall   func(r *bug18Runner) (out string, rc int)
	onConfigure func(r *bug18Runner) (out string, rc int)
	writeRC     bool // false = 模拟不写 rc 的适配层（进程侧按未知处理）
	calls       []string
}

func (r *bug18Runner) run(ctx context.Context, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, line)
	if isShellScriptInvocation(name, args) {
		b, err := os.ReadFile(args[len(args)-1])
		if err != nil {
			return "", err
		}
		script := string(b)
		var out string
		rc := 0
		if strings.Contains(script, "dpkg -i ") {
			if r.onInstall != nil {
				out, rc = r.onInstall(r)
			}
			// rc 落盘：attempt 前缀从脚本里取（与真实脚本同构）。
			if r.writeRC {
				if am := regexp.MustCompile(`'([0-9]+)' "\$rc"`).FindStringSubmatch(script); am != nil {
					_ = os.WriteFile(filepath.Join(r.dir, installRCName),
						[]byte(am[1]+" "+strconv.Itoa(rc)+"\n"), 0o644)
				}
			}
			// dpkg 输出进 install.log（修 R44-1 的重定向后，真实脚本整段都落日志）。
			if out != "" {
				f, _ := os.OpenFile(filepath.Join(r.dir, installLogName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
				if f != nil {
					_, _ = f.WriteString(out + "\n")
					_ = f.Close()
				}
			}
			// 归档晋升只在 rc==0 时执行（与脚本里的 `if [ "$rc" -eq 0 ]; then mv …` 同构）。
			if rc == 0 {
				if mv := regexp.MustCompile(`mv '([^']+)' '([^']+)'`).FindStringSubmatch(script); mv != nil {
					_ = os.Rename(mv[1], mv[2])
				}
			}
		}
		if strings.Contains(script, "dpkg --configure nfvis") {
			cout, crc := "", 0
			if r.onConfigure != nil {
				cout, crc = r.onConfigure(r)
			}
			out += cout
			if crc != 0 {
				return out, errors.New("exit status " + strconv.Itoa(crc))
			}
		}
		return out, nil
	}
	switch {
	case strings.HasPrefix(line, "dpkg-deb "):
		if strings.Contains(line, " Package") {
			return "nfvis\n", nil
		}
		return r.targetVer + "\n", nil
	case strings.HasPrefix(line, "dpkg-query "):
		if strings.Contains(line, "${Status}") {
			return r.status + "\n", nil
		}
		return r.version + "\n", nil
	case strings.Contains(line, "dpkg --audit"):
		if r.status != "" && r.status != pkgStatusInstalled {
			return " nfvis   NFViS\n", nil
		}
		return "", nil
	case strings.HasPrefix(line, "systemd-run") || strings.HasPrefix(line, "systemctl"):
		return "", nil
	}
	return "", nil
}

func (r *bug18Runner) count(sub string) int {
	n := 0
	for _, c := range r.calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

func newBug18Manager(t *testing.T, r *bug18Runner) *SoftwareManager {
	t.Helper()
	shrinkingSettle(t)
	r.dir = t.TempDir()
	m := NewSoftwareManager(r.dir, r.run, func() string { return r.version })
	m.lookPath = func(file string) (string, error) {
		if file == "systemd-run" {
			return "/usr/bin/systemd-run", nil
		}
		return "", errors.New("not found")
	}
	return m
}

// 场景 A（缺陷本体）：同版本坏包，dpkg 退出码 2、包分毫未动。
// 命令必须报失败并点名退出码；结论文件 ok:false 且带 attempt/time；
// 坏包不得留在归档目录（暂存件已清、正式归档未产生）；判定后停掉脱离 unit。
func TestSoftwareAddSameVersionBrokenDebFailsHonestly(t *testing.T) {
	r := &bug18Runner{
		status: pkgStatusInstalled, version: "3.0.25", targetVer: "3.0.25", writeRC: true,
		onInstall: func(*bug18Runner) (string, int) {
			return "dpkg-deb: error: archive nfvis_3.0.25_amd64.deb has premature member ctrl.tar", 2
		},
	}
	m := newBug18Manager(t, r)
	pkg := writeDeb(t, t.TempDir(), "nfvis_3.0.25_amd64.deb")

	res, err := m.Add(context.Background(), pkg, "")
	if err == nil {
		t.Fatalf("同版本坏包：dpkg 失败、包未被改动，不得报成功：%+v", res)
	}
	for _, want := range []string{"退出码 2", "不构成成功证据", "dpkg-deb: error"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("失败文案须含 %q（真实退出码 + 为什么状态读数不算数 + dpkg 原始报错），实得：%v", want, err)
		}
	}
	rec := readRecord(t, m.dir)
	if rec.OK {
		t.Fatalf("结论不得写成功：%+v", rec)
	}
	if !strings.Contains(rec.Reason, "退出码 2") {
		t.Fatalf("结论原因须点名 dpkg 退出码：%+v", rec)
	}
	if rec.Attempt == "" || rec.Time == "" {
		t.Fatalf("结论须带尝试标识与时间（防旧记录冒充新结论）：%+v", rec)
	}
	// 坏包不进回退池：暂存件已删、正式归档名未产生。
	if _, serr := os.Stat(filepath.Join(m.dir, "nfvis_3.0.25_amd64.deb")); serr == nil {
		t.Fatal("坏包不得以正式归档名留在回退池（会覆盖/顶替好包）")
	}
	if entries, _ := os.ReadDir(m.dir); true {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), stagingSuffix) {
				t.Fatalf("判定失败后暂存件应清除，仍在：%s", e.Name())
			}
		}
	}
	// 双写者收口：进程侧判定完成后停掉脱离 unit。
	if n := r.count("systemctl stop nfvis-install-"); n != 1 {
		t.Fatalf("dpkg 已退出（rc 已知）时应停掉脱离 unit（实得 %d 次 systemctl stop）", n)
	}
}

// 场景 B（缺陷本体 + BUG-19）：跨版本坏包，dpkg 退出码 2、版本停在旧值。
// 失败主句必须说**版本差距**（不再出现「当前状态为 X（未达到 X）」的自相矛盾）；
// 结论 version 写目标版本（写采样值会把「装到了什么版本」记错）。
func TestSoftwareAddCrossVersionBrokenDebReportsVersionGap(t *testing.T) {
	r := &bug18Runner{
		status: pkgStatusInstalled, version: "3.0.28", targetVer: "3.0.26", writeRC: true,
		onInstall: func(*bug18Runner) (string, int) {
			return "dpkg-deb: error: archive has premature member", 2
		},
	}
	m := newBug18Manager(t, r)
	pkg := writeDeb(t, t.TempDir(), "nfvis_3.0.26_amd64.deb")

	_, err := m.Add(context.Background(), pkg, "")
	if err == nil {
		t.Fatal("跨版本坏包：包停在旧版本，必须报失败")
	}
	for _, want := range []string{"未更新到目标版本", "已装 3.0.28", "目标 3.0.26"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("失败文案须含 %q（版本差距是主句），实得：%v", want, err)
		}
	}
	if strings.Contains(err.Error(), `未达到 "install ok installed"`) {
		t.Fatalf("状态已达标时不得再出现自相矛盾的状态句（BUG-19）：%v", err)
	}
	rec := readRecord(t, m.dir)
	if rec.OK || rec.Version != "3.0.26" {
		t.Fatalf("结论应为失败且 version=目标版本 3.0.26（写采样值 3.0.28 是记错事实）：%+v", rec)
	}
	if n := r.count("systemctl stop nfvis-install-"); n != 1 {
		t.Fatalf("判定后应停掉脱离 unit，防它稍后覆盖本结论（实得 %d 次）", n)
	}
}

// 成功路径（不回归）：rc==0、状态/版本/audit 达标 ⇒ 报成功；暂存件晋升为正式归档；
// 结论带 attempt/time；判定后停 unit（脚本采样不再需要）。
func TestSoftwareAddSucceedsWithExitCodeAndPromotesArchive(t *testing.T) {
	r := &bug18Runner{
		status: pkgStatusInstalled, version: "3.0.28", targetVer: "3.0.29", writeRC: true,
		onInstall: func(r *bug18Runner) (string, int) {
			r.version = "3.0.29"
			return "Setting up nfvis (3.0.29)", 0
		},
	}
	m := newBug18Manager(t, r)
	pkg := writeDeb(t, t.TempDir(), "nfvis_3.0.29_amd64.deb")

	res, err := m.Add(context.Background(), pkg, "")
	if err != nil {
		t.Fatalf("正常升级应成功：%v", err)
	}
	if !res.OK {
		t.Fatalf("应带成功标记：%+v", res)
	}
	if _, serr := os.Stat(filepath.Join(m.dir, "nfvis_3.0.29_amd64.deb")); serr != nil {
		t.Fatalf("成功后暂存件应晋升为正式归档：%v", serr)
	}
	if entries, _ := os.ReadDir(m.dir); true {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), stagingSuffix) {
				t.Fatalf("成功后不应残留暂存件：%s", e.Name())
			}
		}
	}
	rec := readRecord(t, m.dir)
	if !rec.OK || rec.Attempt == "" || rec.Time == "" {
		t.Fatalf("成功结论应带 ok/attempt/time：%+v", rec)
	}
}

// 补救优先于退出码否决（不回归）：dpkg -i 失败留下 half-configured，但
// `dpkg --configure nfvis` 补救成功 ⇒ 报成功——configure 自身的成功就是证据，
// 不得因 dpkg -i 那次的 rc!=0 翻回失败。
func TestSoftwareAddRecoveryBeatsNonZeroExitCode(t *testing.T) {
	r := &bug18Runner{
		status: "install ok half-configured", version: "3.0.25", targetVer: "3.0.25", writeRC: true,
		onInstall: func(*bug18Runner) (string, int) {
			return "dpkg: error processing package nfvis (--configure)", 1
		},
		onConfigure: func(r *bug18Runner) (string, int) {
			r.status = pkgStatusInstalled
			return "Setting up nfvis (3.0.25)", 0
		},
	}
	m := newBug18Manager(t, r)
	pkg := writeDeb(t, t.TempDir(), "nfvis_3.0.25_amd64.deb")

	if _, err := m.Add(context.Background(), pkg, ""); err != nil {
		t.Fatalf("补救后已收敛到 installed+目标版本，应报成功：%v", err)
	}
}

// rc 未知（适配层不写 install.rc）时沿用只看状态的旧语义——不能让不支持该文件的
// 适配路径从「成功」变「失败」，也不能停 unit（dpkg 可能还在跑，杀了它只会制造
// half-configured）。
func TestSoftwareAddUnknownExitCodeKeepsOldSemanticsAndDoesNotStop(t *testing.T) {
	r := &bug18Runner{
		status: pkgStatusInstalled, version: "3.0.29", targetVer: "3.0.29",
		writeRC:   false, // 适配层不落 install.rc
		onInstall: func(*bug18Runner) (string, int) { return "", 0 },
	}
	m := newBug18Manager(t, r)
	pkg := writeDeb(t, t.TempDir(), "nfvis_3.0.29_amd64.deb")

	if _, err := m.Add(context.Background(), pkg, ""); err != nil {
		t.Fatalf("rc 未知时不得仅因缺退出码判失败：%v", err)
	}
	if n := r.count("systemctl stop"); n != 0 {
		t.Fatalf("dpkg 是否已退出未知时不得停 unit（实得 %d 次 stop）", n)
	}
}

// readDpkgRC 只认本次尝试的前缀：上一轮残渣不得冒充本次的退出码证据。
func TestReadDpkgRCRejectsStaleAttempt(t *testing.T) {
	m := &SoftwareManager{dir: t.TempDir()}
	_ = os.WriteFile(m.rcPath(), []byte("999 0\n"), 0o644)
	if _, known := m.readDpkgRC(installAttempt{attempt: "1"}); known {
		t.Fatal("attempt 前缀不符（上一轮残渣）不得当作本次的退出码")
	}
	_ = os.WriteFile(m.rcPath(), []byte("1 2\n"), 0o644)
	if rc, known := m.readDpkgRC(installAttempt{attempt: "1"}); !known || rc != 2 {
		t.Fatalf("前缀相符应读到 rc=2（实得 known=%v rc=%d）", known, rc)
	}
	if _, known := m.readDpkgRC(installAttempt{attempt: "1"}); !known {
		t.Fatal("同一次尝试内可重复读")
	}
}

// 脚本终判的四条判据（修 BUG-18 的 shell 侧缺口）：
// rc==0 + 状态行 installed + audit 干净 + **版本==目标**；结论 version 写目标字面量；
// rc 与 attempt 落 install.rc；归档晋升只在 rc==0。
func TestInstallScriptVerdictChecksVersionAndExitCode(t *testing.T) {
	m := &SoftwareManager{dir: t.TempDir()}
	job := installJob{action: "安装", act: "add",
		staging: "/tmp/stage.deb", canonical: "/tmp/nfvis_3.0.26_amd64.deb",
		promote: true, targetVer: "3.0.26", attempt: "42"}
	s := m.installScriptWithResult(job)

	// ok=true 的条件必须同时含 rc 与目标版本比对。
	if !strings.Contains(s, `[ "$rc" -eq 0 ]`) {
		t.Fatalf("终判必须吸收 dpkg 退出码（rc!=0 一票否决）：\n%s", s)
	}
	if !strings.Contains(s, `[ "$ver" = '3.0.26' ]`) {
		t.Fatalf("终判必须比对目标版本（同版本坏包场景 $ver 恰等于目标，状态判据全失明）：\n%s", s)
	}
	// rc 落盘紧跟 dpkg（attempt 前缀防上一轮残渣）。
	if !strings.Contains(s, "rc=$?") {
		t.Fatalf("dpkg 之后必须立刻捕获退出码：\n%s", s)
	}
	if !strings.Contains(s, `'42' "$rc"`) {
		t.Fatalf("install.rc 必须带 attempt 前缀：\n%s", s)
	}
	// 结论 version 写目标字面量，不写采样值。
	verdictAt := strings.LastIndex(s, `"ok":%s`)
	if verdictAt < 0 {
		t.Fatalf("脚本应有终态落笔：\n%s", s)
	}
	printfArgs := s[strings.LastIndex(s[:verdictAt], "printf '"):]
	if !strings.Contains(printfArgs, "'add' '3.0.26'") {
		t.Fatalf("终态结论的 version 应是目标版本字面量（写 $ver 会把装到的版本记错）：\n%s", printfArgs)
	}
	// 归档晋升只在 rc==0（坏包不进回退池）。
	if !strings.Contains(s, `if [ "$rc" -eq 0 ]; then mv '/tmp/stage.deb' '/tmp/nfvis_3.0.26_amd64.deb'`) {
		t.Fatalf("归档晋升必须以 rc==0 为前提：\n%s", s)
	}
	// 回退不晋升（它装的本来就是归档原件）。
	rbJob := job
	rbJob.promote = false
	if strings.Contains(m.installScriptWithResult(rbJob), "then mv ") {
		t.Fatal("回退不得包含归档晋升语句")
	}
}

// R44-1 的重定向一半：日志重定向必须罩住**整段**脚本——只追加在末尾时
// `dpkg -i` 的输出进不了 install.log（真机实证：坏包场景日志恒 0 字节，
// 操作者永远看不到 dpkg 拒绝坏包的那行报错）。
func TestLaunchScriptRedirectsWholeScript(t *testing.T) {
	m := &SoftwareManager{dir: t.TempDir()}
	got := m.launchScriptForTest("mkdir -p /x; dpkg -i '/tmp/y.deb'; { echo verdict; }", "/tmp/log")
	if !strings.HasPrefix(got, "{ ") || !strings.HasSuffix(got, "; } >>'/tmp/log' 2>&1") {
		t.Fatalf("重定向必须以分组罩住整段脚本：\n%s", got)
	}
}
