package system

// 修 BUG-17（服务端判定这一半）：升级「先短暂 installed、随后落到 half-configured」时，
// `request system software add` 必须报失败，落盘结论不得写成成功。
//
// 真机时间线（独立测试者 0.5s 采样，v3.0.26 复现 3/3）：
//
//	08:04:30–08:04:36  install ok installed        ← 12 个采样：postinst 仍在跑时的**瞬态**
//	08:04:37–08:04:56  （dpkg 数据库短暂不可读）
//	08:04:57–08:04:59  install ok half-configured  ← 44 个采样：**终态**
//
// 产品恰好在瞬态窗口采样，把「瞬态」当成了「终态」，于是命令报成功、结论文件写
// `{"status":"install ok installed","ok":true}`，而包实际是 half-configured——且该错误结论
// 跨重启不自愈（没有任何自愈路径）。
//
// 三条守护：
//  1. 「装好了」必须有**稳定**证据：同一读数重复出现且跨过最小观察窗，单次读数不算；
//  2. 落盘的 status 必须是**观测到的**那一行，不许再写字面量；
//  3. 状态一直不稳（或读不到）时如实报「无法判定」，既不报成功、也不给一个假的确定失败。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shrinkingSettle 把「稳定」判据缩短到毫秒级，使用例不必真等生产值的 10s 观察窗。
// 用例里的瞬态长度按同一比例缩短，因此判据的**相对**关系与真机一致。
func shrinkingSettle(t *testing.T) {
	t.Helper()
	oldTimeout, oldInterval := installConvergeTimeout, installConvergeInterval
	installConvergeTimeout = 250 * time.Millisecond
	installConvergeInterval = 2 * time.Millisecond
	t.Cleanup(func() {
		installConvergeTimeout, installConvergeInterval = oldTimeout, oldInterval
	})
}

// settleRunner 模拟 dpkg 侧的事实：状态按**读取次序**推进——真机的瞬态与终态正是这么出现的，
// 用一个固定 db.status 的夹具无论如何都验不出来。
type settleRunner struct {
	targetVer   string
	version     string                                // 当前 dpkg 数据库里的版本
	statusAt    func(n int) (string, error)           // 第 n 次状态读（从 1 起）
	onInstall   func(r *settleRunner) (string, error) // dpkg -i 做了什么
	onConfigure func(r *settleRunner) (string, error) // dpkg --configure 做了什么
	statusReads int
	configures  int
	calls       []string
}

func (r *settleRunner) run(_ context.Context, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, line)
	// 安装/补救命令被裹在脱离手段里（`systemd-run … /bin/sh -c "… dpkg -i …"`），故按内容匹配；
	// 这两条必须排在 systemd-run 前缀之前，否则夹具会把真调用记成「脱离手段什么都没干」。
	// 修 round44：安装脚本已改为落文件后用 `sh <file>` 执行，命令行里不再有 `dpkg -i`。
	// 夹具要读脚本内容才能看到那些命令（否则 onInstall 永不触发、版本不升）。
	if isShellScriptInvocation(name, args) {
		if b, err := os.ReadFile(args[len(args)-1]); err == nil {
			script := string(b)
			if strings.Contains(script, "dpkg -i ") {
				if r.onInstall != nil {
					if o, e := r.onInstall(r); e != nil {
						return o, e
					}
				}
			}
			// 脚本内自救：audit 脏才 configure。
			if strings.Contains(script, "dpkg --configure nfvis") {
				st, _ := r.statusAt(1)
				if strings.TrimSpace(st) != pkgStatusInstalled {
					r.configures++
					if r.onConfigure != nil {
						_, _ = r.onConfigure(r)
					}
				}
			}
		}
		return "", nil
	}
	switch {
	case strings.Contains(line, "dpkg -i "):
		if r.onInstall != nil {
			return r.onInstall(r)
		}
		return "", nil
	case strings.Contains(line, "dpkg --configure"):
		r.configures++
		if r.onConfigure != nil {
			return r.onConfigure(r)
		}
		return "", nil
	case strings.HasPrefix(line, "systemd-run"):
		return "", nil // 脱离手段探针与真调用都成功
	case strings.HasPrefix(line, "dpkg-deb "):
		if strings.Contains(line, " Package") {
			return "nfvis\n", nil
		}
		return r.targetVer + "\n", nil
	case strings.HasPrefix(line, "dpkg-query "):
		if strings.Contains(line, "${Status}") {
			r.statusReads++
			if r.statusAt != nil {
				return r.statusAt(r.statusReads)
			}
			return pkgStatusInstalled + "\n", nil
		}
		return r.version + "\n", nil
	}
	return "", nil
}

func (r *settleRunner) count(sub string) int {
	n := 0
	for _, c := range r.calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

// newSettleManager 构造夹具（脱离手段固定为 systemd-run，避免受测试机是否装了 systemd 影响）。
func newSettleManager(t *testing.T, r *settleRunner) *SoftwareManager {
	t.Helper()
	m := NewSoftwareManager(t.TempDir(), r.run, func() string { return r.version })
	m.lookPath = func(file string) (string, error) {
		if file == "systemd-run" {
			return "/usr/bin/systemd-run", nil
		}
		return "", errors.New("not found")
	}
	return m
}

// readRecord 读回本次落盘的结论（不存在视为失败：BUG-16 要求它在）。
func readRecord(t *testing.T, dir string) SoftwareResult {
	t.Helper()
	res, ok := ReadLastResultAt(filepath.Join(dir, "last-result.json"))
	if !ok {
		t.Fatalf("应落盘一条可读的结论（%s 不存在或不可解析）", filepath.Join(dir, "last-result.json"))
	}
	return res
}

// 用例 1（缺陷本体）：状态前两次读作 installed（瞬态），之后永久 half-configured。
// Add 必须报失败；落盘的 status 必须是观测到的终态（half-configured），而不是 installed。
func TestSoftwareAddDoesNotTrustTransientInstalled(t *testing.T) {
	shrinkingSettle(t)
	r := &settleRunner{
		targetVer: "3.0.26",
		version:   "3.0.25",
		statusAt: func(n int) (string, error) {
			if n <= 2 {
				return "install ok installed\n", nil
			}
			return "install ok half-configured\n", nil
		},
		onInstall: func(r *settleRunner) (string, error) {
			// dpkg 解包后版本已经是新的——半配置时版本照样读到目标版本，这正是
			// 「只看状态/只看版本」都会漏判的现场形态。
			r.version = "3.0.26"
			return "", nil
		},
		onConfigure: func(*settleRunner) (string, error) {
			return "dpkg: error processing package nfvis (--configure)\n", errors.New("exit status 1")
		},
	}
	m := newSettleManager(t, r)
	pkg := writeDeb(t, t.TempDir(), "nfvis_3.0.26_amd64.deb")

	res, err := m.Add(context.Background(), pkg, "")
	if err == nil {
		t.Fatalf("包最终是 half-configured，Add 不得报成功：%+v", res)
	}
	if res.OK {
		t.Fatalf("失败时结果不得带成功标记：%+v", res)
	}
	rec := readRecord(t, m.dir)
	if rec.OK {
		t.Fatalf("落盘结论不得声称成功：%+v", rec)
	}
	if rec.Status == pkgStatusInstalled {
		t.Fatalf("落盘的 status 是瞬态读数（%q），不是事实", rec.Status)
	}
	if rec.Status != "install ok half-configured" {
		t.Fatalf("落盘的 status 应是观测到的终态 %q，实得 %q", "install ok half-configured", rec.Status)
	}
}

// 用例 2（不回归）：状态稳定为 installed 且版本是目标版本 ⇒ 报成功，落盘 status 即观测值。
func TestSoftwareAddPersistsObservedStatusWhenStable(t *testing.T) {
	shrinkingSettle(t)
	r := &settleRunner{
		targetVer: "3.0.26",
		version:   "3.0.25",
		statusAt: func(int) (string, error) {
			return "install ok installed\n", nil
		},
		onInstall: func(r *settleRunner) (string, error) {
			r.version = "3.0.26"
			return "", nil
		},
	}
	m := newSettleManager(t, r)
	pkg := writeDeb(t, t.TempDir(), "nfvis_3.0.26_amd64.deb")

	res, err := m.Add(context.Background(), pkg, "")
	if err != nil {
		t.Fatalf("稳定读到 install ok installed + 目标版本，应报成功：%v", err)
	}
	if !res.OK {
		t.Fatalf("应带成功标记：%+v", res)
	}
	rec := readRecord(t, m.dir)
	if !rec.OK {
		t.Fatalf("落盘结论应为成功：%+v", rec)
	}
	if rec.Status != pkgStatusInstalled {
		t.Fatalf("落盘 status 应是观测到的 %q，实得 %q", pkgStatusInstalled, rec.Status)
	}
	if rec.Version != "3.0.26" {
		t.Fatalf("落盘版本应是本次目标版本，实得 %q", rec.Version)
	}
	if n := r.count(" --configure nfvis"); n != 0 {
		t.Fatalf("已经装好时不应再跑 dpkg --configure（实得 %d 次）", n)
	}
}

// 用例 3：读数在 installed / half-configured 之间永久摆动 ⇒ 在观察窗内判不出终态，
// 必须如实报「无法判定」：不报成功，也不对还在变的目标动手动脚（不跑补救）。
func TestSoftwareAddReportsUnstableInsteadOfSuccess(t *testing.T) {
	shrinkingSettle(t)
	r := &settleRunner{
		targetVer: "3.0.26",
		version:   "3.0.25",
		statusAt: func(n int) (string, error) {
			if n%2 == 1 {
				return "install ok installed\n", nil
			}
			return "install ok half-configured\n", nil
		},
		onInstall: func(r *settleRunner) (string, error) {
			r.version = "3.0.26"
			return "", nil
		},
	}
	m := newSettleManager(t, r)
	pkg := writeDeb(t, t.TempDir(), "nfvis_3.0.26_amd64.deb")

	res, err := m.Add(context.Background(), pkg, "")
	if err == nil {
		t.Fatalf("状态一直没有稳定下来，不得报成功：%+v", res)
	}
	if res.OK {
		t.Fatalf("不稳定时结果不得带成功标记：%+v", res)
	}
	rec := readRecord(t, m.dir)
	if rec.OK {
		t.Fatalf("落盘结论不得声称成功：%+v", rec)
	}
	if rec.Status == pkgStatusInstalled {
		t.Fatalf("落盘 status 不得是被采到的瞬态读数：%+v", rec)
	}
	if r.configures != 0 {
		t.Fatalf("读数还在变时不应执行补救（dpkg --configure 实得 %d 次）", r.configures)
	}
}

// 用例 4：回退同一条规则——瞬态 installed 之后落到 half-configured，回退必须报失败，
// 且落盘 status 不是 installed。
func TestSoftwareRollbackDoesNotTrustTransientInstalled(t *testing.T) {
	shrinkingSettle(t)
	r := &settleRunner{
		targetVer: "3.0.25",
		version:   "3.0.26",
		statusAt: func(n int) (string, error) {
			if n <= 2 {
				return "install ok installed\n", nil
			}
			return "install ok half-configured\n", nil
		},
		onInstall: func(r *settleRunner) (string, error) {
			r.version = "3.0.25"
			return "", nil
		},
		onConfigure: func(*settleRunner) (string, error) {
			return "dpkg: error processing package nfvis (--configure)\n", errors.New("exit status 1")
		},
	}
	m := newSettleManager(t, r)
	// 归档就在软件目录里（m.dir）：回退从那里挑「版本最高且非当前」的包。
	writeDeb(t, m.dir, "nfvis_3.0.25_amd64.deb")
	writeDeb(t, m.dir, "nfvis_3.0.26_amd64.deb")

	res, err := m.Rollback(context.Background())
	if err == nil {
		t.Fatalf("回退后包是 half-configured，不得报成功：%+v", res)
	}
	rec := readRecord(t, m.dir)
	if rec.OK || rec.Status == pkgStatusInstalled {
		t.Fatalf("回退的落盘结论同样不得声称成功/写死 installed：%+v", rec)
	}
	if rec.Action != "rollback" {
		t.Fatalf("落盘动作应为 rollback，实得 %q", rec.Action)
	}
}

// 用例 5：落盘结论对「非终态读数」绝不写 ok:true（直接看落盘原文的 ok 字段，
// 不信内存里的结构体——结论文件才是操作者重连后读到的东西）。
func TestSoftwareResultRecordNeverClaimsOKForNonTerminalState(t *testing.T) {
	shrinkingSettle(t)
	r := &settleRunner{
		targetVer: "3.0.26",
		version:   "3.0.25",
		statusAt: func(n int) (string, error) {
			if n <= 2 {
				return "install ok installed\n", nil
			}
			return "install ok half-configured\n", nil
		},
		onInstall: func(r *settleRunner) (string, error) {
			r.version = "3.0.26"
			return "", nil
		},
		onConfigure: func(*settleRunner) (string, error) {
			return "dpkg: error processing package nfvis (--configure)\n", errors.New("exit status 1")
		},
	}
	m := newSettleManager(t, r)
	pkg := writeDeb(t, t.TempDir(), "nfvis_3.0.26_amd64.deb")
	if _, err := m.Add(context.Background(), pkg, ""); err == nil {
		t.Fatal("包最终是 half-configured，Add 不得报成功")
	}

	raw, err := os.ReadFile(filepath.Join(m.dir, "last-result.json"))
	if err != nil {
		t.Fatalf("应落盘结论：%v", err)
	}
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("落盘内容应是合法 JSON: %v\n%s", err, raw)
	}
	if v, exists := rec["ok"]; exists {
		if b, isBool := v.(bool); !isBool || b {
			t.Fatalf("落盘 ok 只能是 false 或缺省，实得 %v（原文：%s）", v, raw)
		}
	}
	if s, _ := rec["status"].(string); s == pkgStatusInstalled {
		t.Fatalf("落盘 status 是被采到的瞬态读数，不等于事实：%s", raw)
	}
}

// 用例 6（Cause C）：脱离执行的 shell 自己写结论时也必须等状态**重复出现**才落笔——
// nfvisd 那时已被 postinst 杀掉，这份结论是 CLI 重连后唯一能读到的东西，采一次就写
// 等于把瞬态固化成「成功」。
func TestInstallScriptWaitsForRepeatedStatusBeforeWritingResult(t *testing.T) {
	m := &SoftwareManager{dir: "/var/lib/nfvis/software"}
	script := m.installScriptWithResult("/tmp/nfvis_3.0.26_amd64.deb", "安装")

	loop := strings.Index(script, "while [")
	if loop < 0 {
		t.Fatalf("取样必须落在循环里（否则就是采一次就写）：\n%s", script)
	}
	write := strings.LastIndex(script, "last-result.json")
	if write < 0 || write < loop {
		t.Fatalf("结论必须在取样循环之后才写：\n%s", script)
	}
	// 判据与进程内同源：同一状态行连续出现 N 次（N 由 installSettleSamples 单一真源给出）。
	if want := fmt.Sprintf("-ge %d", installSettleSamples); !strings.Contains(script, want) {
		t.Fatalf("取样循环应要求同一状态重复出现 %d 次（缺 %q）：\n%s", installSettleSamples, want, script)
	}
	if !strings.Contains(script, "sleep ") {
		t.Fatalf("重复判定之间必须留间隔，否则「重复」只是同一瞬间的重读：\n%s", script)
	}
	// 结论仍由这个脱离 shell 自己写（BUG-16 的性质不能回退）：状态读纯文本 + 落盘路径都在。
	for _, want := range []string{"dpkg -i", "/var/lib/dpkg/status", "last-result.json", "'rollback'"} {
		if want == "'rollback'" {
			continue
		}
		if !strings.Contains(script, want) {
			t.Fatalf("脚本应含 %q：\n%s", want, script)
		}
	}
	if rb := m.installScriptWithResult("/tmp/x.deb", "回退"); !strings.Contains(rb, "'rollback'") {
		t.Fatalf("回退应写 action=rollback：\n%s", rb)
	}
}

// 用例 7（Cause C 的 BUG-16 一半）：结论必须由脱离 shell **自己**写，且在判出终态之前
// 先留一条「未定」——nfvisd 被 postinst 重启后 CLI 立刻就回来读这个文件，
// 那时判定往往还没做完。没有这条，CLI 读到的是「无记录」；有上次升级的旧成功记录时，
// 更糟：旧结论会被当成本次结果。
func TestInstallScriptWritesPendingRecordBeforeVerdict(t *testing.T) {
	m := &SoftwareManager{dir: "/var/lib/nfvis/software"}
	script := m.installScriptWithResult("/tmp/nfvis_3.0.26_amd64.deb", "安装")

	pending := strings.Index(script, resultStatusPending)
	if pending < 0 {
		t.Fatalf("安装开始时应先落一条 pending（nfvisd 重启后 CLI 立刻会读到这个文件）：\n%s", script)
	}
	inst := strings.Index(script, "dpkg -i ")
	if inst < 0 || pending > inst {
		t.Fatalf("pending 必须在 dpkg -i 之前写下（安装期间旧结论不能冒充本次结果）：\n%s", script)
	}
	// pending 不得带成功标记：它只是「还没定」。取 pending 所在的那条 printf 语句来核对。
	stmtStart := strings.LastIndex(script[:pending], "printf ")
	if stmtStart < 0 {
		t.Fatalf("取不到 pending 的落笔语句：\n%s", script)
	}
	stmtEnd := strings.Index(script[pending:], "' ")
	if stmtEnd < 0 {
		stmtEnd = len(script) - pending
	}
	if stmt := script[stmtStart : pending+stmtEnd]; !strings.Contains(stmt, `"ok":false`) {
		t.Fatalf("pending 记录必须 ok:false（否则重连的 CLI 会读成成功）：\n%s", stmt)
	}
	// 终态落笔仍在脚本末尾（写的是本次判定，不是 pending）。
	if verdict := strings.LastIndex(script, `"ok":%s`); verdict < pending {
		t.Fatalf("终态判定必须在 pending 之后写出：\n%s", script)
	}
}

// 用例 9（Cause B 的判别性一半）：复核通道**答不了**这个问题时，落盘记录不得声称成功，
// 也不得把 status 写成 `install ok installed`——没有观测就没有证据。
//
// 为什么这条能照出「写死字面量」：成功路径上字面量恰好等于被观测到的值（成功本就要求
// status==installed），两者无法区分；但「什么都没观测到」时，旧代码依旧写
// `ok:true + install ok installed`——那是纯字面量，不是事实。读口据此会告诉操作者「成功」。
func TestSoftwareResultWithoutObservationDoesNotClaimSuccess(t *testing.T) {
	shrinkingSettle(t)
	r := &settleRunner{
		targetVer: "3.0.26",
		version:   "3.0.25",
		// 不答状态行（模拟不支持 dpkg-query 状态查询的适配层）：读什么都拿不到可判读的状态。
		statusAt: func(int) (string, error) { return "", nil },
		onInstall: func(r *settleRunner) (string, error) {
			r.version = "3.0.26"
			return "", nil
		},
	}
	m := newSettleManager(t, r)
	pkg := writeDeb(t, t.TempDir(), "nfvis_3.0.26_amd64.deb")

	res, _ := m.Add(context.Background(), pkg, "")
	if res.OK {
		t.Fatalf("没有观测就没有证据，内存结论不得带成功标记：%+v", res)
	}
	rec := readRecord(t, m.dir)
	if rec.OK {
		t.Fatalf("落盘结论不得声称成功（旧代码在这里写字面量 ok:true）：%+v", rec)
	}
	if rec.Status == pkgStatusInstalled {
		t.Fatalf("落盘 status 不得是被写死的 %q（本次根本没有观测到它）：%+v", pkgStatusInstalled, rec)
	}
	if rec.Status != "" {
		t.Fatalf("观测不可用时 status 应留空（不编造），实得 %q", rec.Status)
	}
	if rec.Reason == "" {
		t.Fatal("判不出时必须留下原因，否则重连的 CLI 只说「失败」而不知为何")
	}
}

// 用例 8：PackageState 是「即时读数」，不得等稳定——升级进行中「现在是 half-configured」
// 正是读口要展示的当前状态；且读失败时要同时带回已拿到的那个字段。
func TestPackageStateIsImmediateAndReportsErrors(t *testing.T) {
	r := &settleRunner{
		targetVer: "3.0.26",
		version:   "3.0.26",
		statusAt: func(int) (string, error) {
			return "install ok half-configured\n", nil
		},
	}
	m := newSettleManager(t, r)
	start := time.Now()
	status, version, err := m.PackageState(context.Background())
	if err != nil {
		t.Fatalf("PackageState: %v", err)
	}
	if status != "install ok half-configured" || version != "3.0.26" {
		t.Fatalf("PackageState 应如实回即时读数，实得 status=%q version=%q", status, version)
	}
	// 即时读数：只读一次，不等于「等稳定」。
	if r.statusReads != 1 {
		t.Fatalf("PackageState 是显示用即时读数，只应读一次（实得 %d 次）", r.statusReads)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("PackageState 不得等待稳定（耗时 %s）", d)
	}

	// 读失败：错误要回传，且版本这一路已取到的值仍要带出（调用方能区分「读失败」与「状态是 X」）。
	failing := &settleRunner{targetVer: "3.0.26", version: "3.0.25", statusAt: func(int) (string, error) {
		return "", errors.New("dpkg 数据库被锁")
	}}
	m2 := newSettleManager(t, failing)
	st2, ver2, err2 := m2.PackageState(context.Background())
	if err2 == nil {
		t.Fatal("状态读失败必须回传错误（调用方据此说「无法核对」，不假装一致）")
	}
	if st2 != "" {
		t.Fatalf("状态读失败时状态应为空（不编造）：%q", st2)
	}
	if ver2 != "3.0.25" {
		t.Fatalf("版本那一路取到了就应带出，实得 %q", ver2)
	}
}
