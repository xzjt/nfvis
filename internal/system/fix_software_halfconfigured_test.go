package system

// 缺陷回归（真机 v3.0.24 实证）：`request system software add` 把包留在 half-configured。
//
// 机理：CLI 命令经 nfvisd 执行，`dpkg -i` 是它的**子进程**；postinst 在升级场景里执行
// `systemctl try-restart nfvis.service`，systemd 停单元时向整个 cgroup 发 SIGTERM，dpkg
// 因此得到 "signal: terminated"，dpkg 判定 postinst 失败、把包留在 iF；而文件其实早已解开、
// 包也随后配置完成——调用方看到的「安装失败（可执行 rollback）」与事实相反，承诺的自动
// 回退也没有发生。
//
// 三条守护：
//  1. 安装必须从 nfvisd 的进程树/cgroup 里摘出去执行（systemd-run --scope + 日志重定向），
//     这样 nfvisd 被重启不会连带杀死 dpkg，dpkg 也不会因管道读端关闭而收 SIGPIPE；
//  2. 成败以 dpkg 数据库（`install ok installed` **且版本是目标版本**）为准，不信命令退出码；
//  3. 命令被打断、包还没落地时，报错必须点名真实状态与恢复命令。

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
)

// pkgDB 模拟 dpkg 数据库：状态与版本分开推进，才验得出「只看状态」的误判
// （升级前旧包本来也是 install ok installed）。
type pkgDB struct {
	status  string // 如 install ok installed / install ok half-configured / install ok unpacked
	version string
}

// fixRunner 模拟宿主命令。db 为 nil 表示这个夹具不回答 dpkg-query（模拟不支持的适配层）。
// onInstall / onConfigure 让用例决定「这一次 dpkg 到底做了什么」。
type fixRunner struct {
	db          *pkgDB
	onInstall   func(db *pkgDB) (out string, err error)
	onConfigure func(db *pkgDB) (out string, err error)
	calls       []string
}

func (f *fixRunner) run(ctx context.Context, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, line)
	// 修 round44：安装脚本改为落文件后用 `sh <file>` 执行（systemd-run 会吞 ${x%%|*}）。
	// 假 runner 要认这一形态并模拟脚本里的命令序列，否则「dpkg -i 被调用」这类断言会失效。
	if isShellScriptInvocation(name, args) {
		b, err := os.ReadFile(args[len(args)-1])
		if err != nil {
			return "", err
		}
		script := string(b)
		// 脚本内的命令**以 script: 前缀单独记账**：顶层 calls 只反映「谁被当作脱离执行
		// 的入口」，否则「dpkg -i 不得直跑」这类断言会被脚本内部调用误伤。
		var out strings.Builder
		// 路径里的引号形态在 Windows 宿主上与非 POSIX 一致（脚本内是 / 分隔，deb 路径是 \），
		// 故只判「脚本里有没有 dpkg -i」并取引号内内容，不依赖分隔符。
		if strings.Contains(script, "dpkg -i ") {
			pkg := ""
			if m := regexp.MustCompile(`dpkg -i '([^']*)'`).FindStringSubmatch(script); m != nil {
				pkg = m[1]
			}
			f.calls = append(f.calls, "script:dpkg -i "+pkg)
			{
				// 只记一条带前缀的（runScript 内部不再二次记账），避免「顶层 dpkg -i」误报。
				// 输出必须**连同错误一起**带回：上层靠「输出里有没有 dpkg frontend lock」
				// 决定要不要重试（R44-1 同族的语义），丢掉输出等于让重试路径永远不触发。
				o, e := f.installOnce(ctx, pkg)
				out.WriteString(o)
				if e != nil {
					return out.String(), e
				}
			}
		}
		// 脚本只**观测** audit（判据的一部分），不自救：补救由进程内 installAndVerify 做一次，
		// 两处都补会让「恰好一次 configure」的语义失效。
		if strings.Contains(script, "dpkg --audit") {
			_, _ = f.run(ctx, "dpkg", "--audit")
		}
		// runDetached 的脚本就是一条 `dpkg --configure nfvis`——照实执行，否则补救路径的
		// 断言（「恰好一次」「输出回传」）永远看不到效果。
		if strings.Contains(script, "dpkg --configure nfvis") {
			if o, e := f.run(ctx, "dpkg", "--configure", "nfvis"); e != nil {
				return out.String(), e
			} else {
				out.WriteString(o)
			}
		}
		if strings.Contains(script, "${Status}") {
			_, _ = f.run(ctx, "dpkg-query", "-W", "-f=${Status}", "nfvis")
		}
		if strings.Contains(script, "${Version}") {
			_, _ = f.run(ctx, "dpkg-query", "-W", "-f=${Version}", "nfvis")
		}
		return out.String(), nil
	}
	switch {
	case strings.HasPrefix(line, "dpkg-deb "):
		if strings.Contains(line, " Package") {
			return "nfvis\n", nil
		}
		return "3.0.25\n", nil // Version
	case strings.HasPrefix(line, "dpkg-query "):
		if f.db == nil {
			return "", nil
		}
		if strings.Contains(line, "${Status}") {
			return f.db.status + "\n", nil
		}
		return f.db.version + "\n", nil
	case strings.Contains(line, "dpkg --audit"):
		// 数据库视角：状态不是 installed 就列出来（与真机 dpkg --audit 行为一致）。
		if f.db != nil && f.db.status != "" && f.db.status != pkgStatusInstalled {
			return " nfvis                NFViS NFV infrastructure appliance\n", nil
		}
		return "", nil
	case strings.Contains(line, "dpkg --configure"):
		if f.onConfigure != nil {
			return f.onConfigure(f.db)
		}
		return "", nil
	case strings.Contains(line, "dpkg -i "):
		if f.onInstall != nil {
			return f.onInstall(f.db)
		}
		return "", nil
	}
	return "", nil
}

// installOnce 执行一次 dpkg -i 但**不记入 calls**（由 runScript 记带前缀的一条）。
func (f *fixRunner) installOnce(_ context.Context, pkg string) (string, error) {
	if f.onInstall != nil {
		return f.onInstall(f.db)
	}
	return "", nil
}

// count 统计包含某子串的调用次数（断言「调用几次」这类事实）。
func (f *fixRunner) count(sub string) int {
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

// newFixSoftware 构造夹具：lookPath 固定「有 systemd-run、无 setsid」，
// 使断言不受测试机是否装了 systemd 影响（Windows 上两者都没有，直跑分支会掩盖回归）。
func newFixSoftware(t *testing.T, fr *fixRunner) *SoftwareManager {
	t.Helper()
	m := NewSoftwareManager(t.TempDir(), fr.run, func() string { return "3.0.24" })
	m.lookPath = func(file string) (string, error) {
		if file == "systemd-run" {
			return "/usr/bin/systemd-run", nil
		}
		return "", errors.New("not found")
	}
	return m
}

// 用例 1：dpkg 的退出路径报 "signal: terminated"（真机形态），但包已读回 installed 且版本
// 已是目标版本 ⇒ 必须报成功，且结果里不得出现「安装失败」；也不该再跑 --configure。
func TestSoftwareAddSucceedsWhenPackageReadsBackInstalled(t *testing.T) {
	db := &pkgDB{status: "install ok installed", version: "3.0.25"} // dpkg 其实装完了
	fr := &fixRunner{db: db, onInstall: func(*pkgDB) (string, error) {
		// 真机形态：dpkg 自身的输出正常，但我们这一侧的等待被打断。
		return "", errors.New("signal: terminated")
	}}
	m := newFixSoftware(t, fr)
	pkg := writeDeb(t, t.TempDir(), "nfvis_3.0.25_amd64.deb")

	res, err := m.Add(context.Background(), pkg, "")
	if err != nil {
		t.Fatalf("包已读回 install ok installed，不应报失败：%v", err)
	}
	if strings.Contains(res.Output, "安装失败") {
		t.Fatalf("实际已装好，结果不得声称安装失败：%q", res.Output)
	}
	if !strings.Contains(res.Output, "已实际生效") {
		t.Fatalf("退出路径被中断但升级已生效，结果须写明这一点：%q", res.Output)
	}
	if n := fr.count(" --configure nfvis"); n != 0 {
		t.Fatalf("状态已 installed 时不应再跑 dpkg --configure（实得 %d 次）", n)
	}
}

// 用例 2：dpkg -i 报错且包停在 iF（half-configured）⇒ 补救执行一次 `dpkg --configure nfvis`，
// 第二次读回 installed 即算成功。
func TestSoftwareAddConfiguresHalfConfiguredPackageOnce(t *testing.T) {
	db := &pkgDB{status: "install ok half-configured", version: "3.0.25"}
	fr := &fixRunner{db: db,
		onInstall: func(*pkgDB) (string, error) {
			return "dpkg: error processing package nfvis\n", errors.New("exit status 1")
		},
		onConfigure: func(db *pkgDB) (string, error) {
			db.status = "install ok installed"
			return "Setting up nfvis (3.0.25) ...\n", nil
		},
	}
	m := newFixSoftware(t, fr)
	pkg := writeDeb(t, t.TempDir(), "nfvis_3.0.25_amd64.deb")

	res, err := m.Add(context.Background(), pkg, "")
	if err != nil {
		t.Fatalf("补救后已读回 installed，应报成功：%v", err)
	}
	if n := fr.count("dpkg --configure nfvis"); n != 1 {
		t.Fatalf("应恰好补救一次 dpkg --configure nfvis，实得 %d 次", n)
	}
	if !strings.Contains(res.Output, "Setting up nfvis") {
		t.Fatalf("补救命令的输出须回传给操作者：%q", res.Output)
	}
}

// 用例 3：补救之后仍不是 installed ⇒ 必须报错，错误里点名**真实包状态**与**恢复命令**。
func TestSoftwareAddReportsActualStateAndRecoveryWhenInstallTrulyFails(t *testing.T) {
	db := &pkgDB{status: "install ok unpacked", version: "3.0.25"}
	fr := &fixRunner{db: db,
		onInstall: func(*pkgDB) (string, error) {
			return "dpkg: dependency problems prevent configuration of nfvis\n", errors.New("exit status 1")
		},
		onConfigure: func(*pkgDB) (string, error) {
			return "dpkg: error: dependency problems\n", errors.New("exit status 1")
		},
	}
	m := newFixSoftware(t, fr)
	pkg := writeDeb(t, t.TempDir(), "nfvis_3.0.25_amd64.deb")

	res, err := m.Add(context.Background(), pkg, "")
	if err == nil {
		t.Fatal("包停在 unpacked，必须报失败")
	}
	for _, want := range []string{"install ok unpacked", "dpkg --configure nfvis", "dpkg -i "} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误文案须含 %q（真实状态 + 恢复命令），实得：%v", want, err)
		}
	}
	if strings.Contains(res.Output, "已实际生效") {
		t.Fatalf("真失败时不得声称已生效：%q", res.Output)
	}
}

// 用例 3b：dpkg 根本没跑起来（包完全没落地）时，同样要如实说「未安装」并给恢复命令；
// 不能因为旧包还 installed 就当成升级成功——这正是「只看状态」会漏掉的误判。
func TestSoftwareAddDoesNotClaimSuccessForUntouchedOldPackage(t *testing.T) {
	db := &pkgDB{status: "install ok installed", version: "3.0.24"} // 旧包没动
	fr := &fixRunner{db: db, onInstall: func(*pkgDB) (string, error) {
		return "dpkg: error: cannot access archive\n", errors.New("exit status 2")
	}}
	m := newFixSoftware(t, fr)
	pkg := writeDeb(t, t.TempDir(), "nfvis_3.0.25_amd64.deb")

	if _, err := m.Add(context.Background(), pkg, ""); err == nil {
		t.Fatal("版本仍是旧版本，不得报升级成功")
	} else if !strings.Contains(err.Error(), "3.0.24") {
		t.Fatalf("错误须点出仍在跑的实际版本：%v", err)
	}
}

// 用例 4（回归）：dpkg 前端锁被 apt/unattended-upgrades 占用时仍按原语义重试，
// 锁释放后升级照常成功。
func TestSoftwareAddRetriesWhenDpkgFrontendLocked(t *testing.T) {
	db := &pkgDB{status: "install ok installed", version: "3.0.24"}
	installs := 0
	fr := &fixRunner{db: db, onInstall: func(db *pkgDB) (string, error) {
		installs++
		if installs <= 2 {
			return "dpkg: error: dpkg frontend lock was locked by another process with pid 4242\n",
				errors.New("exit status 2")
		}
		db.version = "3.0.25"
		return "", nil
	}}
	m := newFixSoftware(t, fr)
	pkg := writeDeb(t, t.TempDir(), "nfvis_3.0.25_amd64.deb")

	if _, err := m.Add(context.Background(), pkg, ""); err != nil {
		t.Fatalf("锁释放后应安装成功：%v", err)
	}
	if installs != 3 {
		t.Fatalf("前两次撞锁后应重试，第三次成功（实得 %d 次 dpkg -i）", installs)
	}
}

// 用例 5：安装命令必须以「脱离 nfvisd 进程树」的形态调用（记录 argv 断言，不真的起进程）。
func TestSoftwareInstallRunsDetachedFromNfvisd(t *testing.T) {
	db := &pkgDB{status: "install ok installed", version: "3.0.24"}
	fr := &fixRunner{db: db, onInstall: func(db *pkgDB) (string, error) {
		db.version = "3.0.25"
		return "", nil
	}}
	m := newFixSoftware(t, fr)
	pkg := writeDeb(t, t.TempDir(), "nfvis_3.0.25_amd64.deb")

	if _, err := m.Add(context.Background(), pkg, ""); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// 契约（round44 起）：安装是「systemd-run 起一个 transient unit 去跑脚本文件」，
	// 脚本内部才执行 dpkg -i。故断言分两层——顶层不得出现裸 dpkg -i；顶层必须有一条
	// systemd-run 调用且指向 .sh；脚本内部确实含 dpkg -i。
	seen := 0
	for _, c := range fr.calls {
		if strings.HasPrefix(c, "dpkg -i ") {
			t.Fatalf("安装不得作为 nfvisd 的同 cgroup 子进程直跑（postinst 重启会连带杀掉它）：%s", c)
		}
		if strings.HasPrefix(c, "script:dpkg -i ") {
			seen++
			continue
		}
		if strings.Contains(c, "systemd-run") && !strings.Contains(c, "/bin/true") {
			if !strings.Contains(c, ".sh") {
				t.Fatalf("systemd-run 应执行脚本文件（systemd-run 会吞脚本里的 ${x%%%%|*}）：%s", c)
			}
			if strings.Contains(c, "--scope") {
				t.Fatalf("不得用 --scope：--scope 让脚本活在调用方 cgroup 里，nfvisd 重启时会被整组收走：%s", c)
			}
		}
	}
	if seen == 0 {
		t.Fatalf("脚本里没有 dpkg -i：%v", fr.calls)
	}
}

// 用例 6：没有 systemd 的底座退回 setsid（仍脱离本进程的 session）。
func TestSoftwareInstallFallsBackToSetsidWithoutSystemd(t *testing.T) {
	db := &pkgDB{status: "install ok installed", version: "3.0.24"}
	fr := &fixRunner{db: db, onInstall: func(db *pkgDB) (string, error) {
		db.version = "3.0.25"
		return "", nil
	}}
	m := newFixSoftware(t, fr)
	m.lookPath = func(file string) (string, error) {
		if file == "setsid" {
			return "/usr/bin/setsid", nil
		}
		return "", errors.New("not found")
	}
	pkg := writeDeb(t, t.TempDir(), "nfvis_3.0.25_amd64.deb")

	if _, err := m.Add(context.Background(), pkg, ""); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// 无 systemd：顶层应出现 setsid 且指向脚本文件；不得有裸 dpkg -i。
	sawSetsid := false
	for _, c := range fr.calls {
		if strings.HasPrefix(c, "dpkg -i ") {
			t.Fatalf("无 systemd 时安装不得直跑 dpkg -i：%s", c)
		}
		if strings.Contains(c, "setsid") {
			sawSetsid = true
			if !strings.Contains(c, ".sh") {
				t.Fatalf("setsid 应执行脚本文件：%s", c)
			}
		}
	}
	if !sawSetsid {
		t.Fatalf("无 systemd 时应走 setsid 脱离 session：%v", fr.calls)
	}
}

// 用例 7：脱离手段自己起不来（systemd-run 报「连不上 bus」）且命令没跑过 ⇒ 降级直跑一次；
// 不能因为手段失败就当作安装结束（那会让升级静默不发生）。
func TestSoftwareInstallFallsThroughWhenLauncherFails(t *testing.T) {
	db := &pkgDB{status: "install ok installed", version: "3.0.24"}
	launches, direct := 0, 0
	fr := &fixRunner{db: db, onInstall: func(db *pkgDB) (string, error) {
		direct++
		db.version = "3.0.25"
		return "", nil
	}}
	base := fr.run
	override := func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "systemd-run" {
			launches++
			fr.calls = append(fr.calls, name+" "+strings.Join(args, " "))
			return "Failed to connect to bus: No such file or directory", errors.New("exit status 1")
		}
		return base(ctx, name, args...)
	}
	m := NewSoftwareManager(t.TempDir(), override, func() string { return "3.0.24" })
	m.lookPath = func(file string) (string, error) {
		if file == "systemd-run" {
			return "/usr/bin/systemd-run", nil
		}
		return "", errors.New("not found") // 无 setsid → 只剩直跑
	}
	pkg := writeDeb(t, t.TempDir(), "nfvis_3.0.25_amd64.deb")

	if _, err := m.Add(context.Background(), pkg, ""); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if launches != 1 {
		t.Fatalf("应只尝试一次 systemd-run，实得 %d", launches)
	}
	if direct != 1 {
		t.Fatalf("systemd-run 起不来后应降级直跑一次（实得 %d 次）", direct)
	}
}
