// M5-7：软件升级/回退与电源、NTP（FR-OPS-001~003）。
//
// 语义：
//   - add：本地 .deb 或 http(s) URL（可选 sha256 强校验）→ 校验包名/依赖（`dpkg-deb -f`）
//     → 归档到软件目录（供 rollback）→ `dpkg -i` 安装（postinst 负责重启 nfvisd）→ 读回版本报告。
//     安装**脱离 nfvisd 的 cgroup** 执行、并以 dpkg 数据库状态复核成败（见 installOnce / installAndVerify）：
//     postinst 重启 nfvisd 时不能连带杀死 dpkg，命令的退出路径也不能当作安装结果。
//   - rollback：从归档目录选**版本序上严格低于当前版本的上一版本** → `dpkg -i` 回退（FR-OPS-002）。
//     版本比较按 Debian 语义（CompareDebVersion，等价 dpkg --compare-versions）：不是
//     「归档里最高且非当前」——那样归档中若存有更高版本（含预发布版）会把「回退」做成升级。
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
	posixpath "path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	//
	// 修 BUG-23（v3.0.29 外部测试报告）：**不得带 omitempty**。结论文件是对外产物——
	// CLI 重连后读它判成败，外部脚本也直接解析它。带 omitempty 时失败态（false 是零值）
	// 会**没有 `ok` 键**，于是 `jq '.ok == false'` 得到 null、Go 的 `*bool` 得到 nil：
	// 「没有这个键」被读成「没这个信息」而不是「不是成功」。成功写 `"ok":true`、失败无键的
	// 不对称结构，是比错值更难发现的误判来源（读口靠 reason/status 仍能判对，所以产品
	// 自身不会报错，只有外部脚本会静静走错分支）。成败两个方向的键集合必须一致。
	OK bool `json:"ok"`
	// Status 复核时的 dpkg 状态行（如 `install ok installed`；取不到则为空）。
	//
	// 修 BUG-17：这里必须是**观测到的**那一行，不允许再写字面量。曾经这里恒写
	// `install ok installed`，于是「结论文件」只证明「文件说成功」，不证明「包真的成功」——
	// 真机上包落在 half-configured，记录却写着 ok:true，而这个错误结论跨重启不自愈。
	Status string `json:"status,omitempty"`
	// Reason 判不出/失败时的原因（状态行之外的说明）。与 Status 分开：Status 只放观测事实，
	// 原因放这里，操作者据此知道「发生了什么」以及「下一步做什么」。
	Reason string `json:"reason,omitempty"`
	// Attempt 本次升级/回退尝试的标识（纳秒时间戳）。结论文件曾有「双写者」：进程侧与
	// 脱离脚本都会写它（修 BUG-18 场景 B：脚本晚 ~12s 用自己的判定覆盖进程侧结论）。
	// 带上标识后读侧能分清「这是哪次尝试的结论」，上一次的旧记录不再冒充本次结果。
	Attempt string `json:"attempt,omitempty"`
	// Time 本次尝试的发起时间（RFC3339）。结论文件此前没有任何时间字段，一条旧记录可以
	// 永远冒充「最近一次」；有落笔时间，操作者一眼能看出它有多旧。
	Time string `json:"time,omitempty"`
}

// LastResultPath 最近一次升级/回退的结论落点（修 BUG-16）。
//
// 为什么必须落盘：`software add` 会重启 nfvisd，承载该命令的 HTTP 连接随之 EOF——操作者
// 只看到「连接 nfvisd 失败」+ 非零退出码，会误判失败而重试或走回退，而升级其实已成功。
// 结论写在**不随进程消失**的文件里，CLI 重连后就能读到「到底成没成」，不靠猜、也不必
// 为此新增一条读命令（命令树是契约，为一次复核扩命令面不划算）。
const LastResultPath = DefaultSoftwareDir + "/last-result.json"

// installRCName dpkg 退出码的落点：脱离脚本在 `dpkg -i` 返回后**立即**写下
// `<attempt> <rc>`，进程侧判定读它（修 BUG-18 / R44-1）。
//
// 为什么必须经文件：dpkg 在 transient unit 里跑，`launchShell` 拿到的退出码是 systemd-run
// 客户端的、与 dpkg 无关（R44-1：脚本以 printf 收尾时整段退出码恒 0）——「dpkg 到底成没成」
// 这个事实只有跑 dpkg 的那个 shell 知道，它写下来，这边才读得到。
const installRCName = "install.rc"

// stagingSuffix 归档暂存名后缀：Add 先把包拷成 `<归档名>.<attempt>.staging`，dpkg 成功
// （rc==0）才由脱离 shell 晋升为正式归档名——坏包/失败包**不进回退池**（修 BUG-18 的归档
// 副作用：以前无条件先归档，一个同版本坏包就能把归档里的好包覆盖掉；真机实测归档文件
// 与 broken.deb 哈希一致）。后缀让 archived() 的正则匹配不到暂存件。
const stagingSuffix = ".staging"

// dpkgRCWait installDeb 每次尝试后等 install.rc 出现的上界：坏包/撞锁在秒级失败，
// 正常安装要跑十几秒——等不到就按「dpkg 仍在正常推进」返回（等待由 settlePackage 承担）。
// var（非 const）：单测把它与收敛节奏一起缩短（见 shrinkingSettle）。
var dpkgRCWait = 5 * time.Second

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

// nowFn 当前时间（now 未注入时用真实时钟——脚本生成/stamp/sweep 都要调它，
// 裸构造的 SoftwareManager 不许因此 panic）。
func (m *SoftwareManager) nowFn() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// stamp 给结论补上尝试标识与时间（读侧据此分辨「哪次尝试、多旧的记录」——修 BUG-18
// 的 P2 项：此前结论文件无任何时间字段，一条旧记录可以永远冒充「最近一次」）。
func (m *SoftwareManager) stamp(res SoftwareResult, job installJob) SoftwareResult {
	res.Attempt, res.Time = job.attempt, m.nowFn().UTC().Format(time.RFC3339)
	return res
}

// rcPath dpkg 退出码文件的路径。
func (m *SoftwareManager) rcPath() string { return filepath.Join(m.dir, installRCName) }

// readDpkgRC 读脱离脚本落下的 dpkg 退出码。attempt 前缀不符（上一轮残渣）不算数：
// 这份证据只属于本次尝试。
func (m *SoftwareManager) readDpkgRC(at installAttempt) (int, bool) {
	raw, err := os.ReadFile(m.rcPath())
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(raw))
	if len(f) != 2 || f[0] != at.attempt {
		return 0, false
	}
	rc, err := strconv.Atoi(f[1])
	if err != nil {
		return 0, false
	}
	return rc, true
}

// waitDpkgRC 有界地等 install.rc 出现：坏包/撞锁在秒级失败（等得到），正常安装要跑
// 十几秒（等不到就按未知返回——不阻塞，等待由 settlePackage 承担）。
func (m *SoftwareManager) waitDpkgRC(at installAttempt, max time.Duration) (int, bool) {
	deadline := time.Now().Add(max)
	for {
		if rc, known := m.readDpkgRC(at); known {
			return rc, true
		}
		if !time.Now().Before(deadline) {
			return 0, false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// sweepStaging 清掉老的归档暂存残渣：判定失败而进程又没能删（如整机重启打断）的暂存件
// 留在软件目录里没有用途；超过 1 小时的一律清（新于此的可能是还在跑的尝试的活件）。
func (m *SoftwareManager) sweepStaging() {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	cut := m.nowFn().Add(-time.Hour)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), stagingSuffix) {
			continue
		}
		if info, ierr := e.Info(); ierr == nil && info.ModTime().Before(cut) {
			_ = os.Remove(filepath.Join(m.dir, e.Name()))
		}
	}
}

// retryHintFor 失败文案里的重试指引：本地包还能照路径重装；URL 下载件是临时文件、
// 指过去只会让人扑空——指到「再跑一次命令」。
func retryHintFor(pkg string) string {
	if isURL(pkg) {
		return "重新下载后再次执行 request system software add " + pkg
	}
	return "执行 dpkg -i " + pkg + " 重试"
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
	// unit 本次尝试的 transient unit 名（systemd-run 形态；其余为空）——进程侧判定
	// 完成后按它停掉后台脚本（修 BUG-18 场景 B 的双写者竞态）。
	unit string
	// logPath 安装日志路径（判定时再读一次拿全文：launch 返回那一刻 dpkg 多半还没写完）。
	logPath string
	// attempt 本次尝试的标识（install.rc 的前缀防上一轮残渣冒充）。
	attempt string
	// rc / rcKnown dpkg 自身的退出码（脚本在 dpkg 返回后立即落 install.rc）。
	// unknown = 没读到（适配层不写、或 dpkg 还在跑）——此时沿用只看状态的旧语义。
	rc      int
	rcKnown bool
}

// text 合并本次尝试的全部输出。
func (a installAttempt) text() string { return joinLines(a.cmdOut, a.log) }

// 复核节奏（测试可缩短）。都是**判定**用的上界，不参与安装本身的超时。
var (
	installConvergeTimeout  = 60 * time.Second // 等「稳定观测」的上界
	installConvergeInterval = 2 * time.Second  // 采样间隔（两次采样之间必须留间隔，否则「重复」只是同一瞬间的重读）
	// installSettleSamples 是「装好了」的**证据门槛**：同一状态行连续出现这么多次
	// （配合上面的间隔 ⇒ 见 settleMinSpan，这里＝10s）。为什么不取「读 3 次」（报告给的下限）：
	// 真机复测（v3.0.26，0.5s 采样，3/3 复现）里 postinst 期间状态会读作
	// `install ok installed` 约 7s，随后 dpkg 数据库被锁约 20s，最终落到
	// `install ok half-configured`——3 次 × 2s 只有 4s，仍在瞬态内，等于没修。
	// 故门槛按**实测瞬态**定：观察窗必须明显长于 7s。
	installSettleSamples = 6
	installVerifyTimeout = 2 * time.Minute // 一次状态复核/补救的上界
)

// settleMinSpan 稳定判据隐含的**最小时间跨度**（次数 × 间隔），由两个常量单一推出，
// 不另立一个时间常量——否则两者迟早互相矛盾。取值的理由见 installSettleSamples。
func settleMinSpan() time.Duration {
	if installSettleSamples < 2 {
		return 0
	}
	return time.Duration(installSettleSamples-1) * installConvergeInterval
}

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
	// 修 R51-L1：包里的 Version 是**由包自己声明**的字符串，此前未做任何格式检查就流向两处
	// root 权限的落点——归档路径 `filepath.Join(m.dir, "nfvis_<ver>_amd64.deb")`
	// （`../../../../tmp/x` 经 Join 折叠后真的写到软件目录之外）与脱离执行的 root shell 脚本
	// 里的 reason 字符串（引号/反引号/`$()` 会破开那条被 BUG-16/17/18 加固过的脚本）。
	// 版本号按 Debian 字符集白名单校验，不合格即**受理前**拒绝，不碰任何文件。
	if verr := validateDebVersion(newVer); verr != nil {
		return res, fmt.Errorf("软件包 %s 的 Version 字段不可用（%v）：拒绝安装（该值会被拼进归档路径与以 root 执行的安装脚本）",
			filepath.Base(path), verr)
	}

	// 归档（供回退）后安装。修 BUG-18 的归档副作用：先拷成**暂存名**，dpkg 成功（rc==0）
	// 才由脱离 shell 晋升为正式归档——以前无条件先归档，一个同版本坏包就能把归档里的
	// 好包覆盖掉，回退池从此只能选到坏包（真机实测：/var/lib/nfvis/software/ 里的
	// nfvis_3.0.28_amd64.deb 与 broken.deb 哈希一致）。
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return res, fmt.Errorf("创建软件目录: %w", err)
	}
	m.sweepStaging()
	attempt := strconv.FormatInt(m.nowFn().UnixNano(), 10)
	archived := filepath.Join(m.dir, fmt.Sprintf("nfvis_%s_amd64.deb", newVer))
	staging := fmt.Sprintf("%s.%s%s", archived, attempt, stagingSuffix)
	if err := copyFileTo(path, staging); err != nil {
		return res, fmt.Errorf("归档软件包: %w", err)
	}
	job := installJob{
		action: "安装", act: "add",
		staging: staging, canonical: archived, promote: true,
		targetVer: newVer, attempt: attempt,
		retryHint: retryHintFor(pkg),
	}
	inst := m.installAndVerify(ctx, job)
	if !inst.ok {
		// 修 BUG-16：失败也要留结论——连接可能已随 nfvisd 重启断掉，CLI 重连后靠这份
		// 落盘结论才知道「到底成没成」，而不是只看到 EOF。
		// 修 BUG-17：Status 只放**稳定观测到**的事实（没稳定就留空），原因进 reason——
		// 以前这里写 `"安装失败: …"`、成功分支写死 `install ok installed`，两处都在
		// 用字面量冒充事实。
		m.RecordResult(m.stamp(SoftwareResult{Action: "add", Version: newVer, Package: filepath.Base(archived),
			Previous: res.Previous, Status: inst.status, Reason: inst.reason, OK: false}, job))
		if inst.unverified {
			// 复核通道答不了 ⇒ 沿用旧口径（命令成功即成功），但结论文件里绝不写 ok:true。
			// 暂存件也不删：判定未定，dpkg 可能已把它装好（删了回退池就缺这一版）。
			res.Package, res.Output = filepath.Base(archived), strings.TrimSpace(inst.out)
			res.Version = newVer
			return res, nil
		}
		// 判定失败：暂存件不进回退池（晋升只在 rc==0 时发生，这里兜底删掉）。
		_ = os.Remove(staging)
		return res, errors.New(inst.reason)
	}
	res.Package, res.Output = filepath.Base(archived), strings.TrimSpace(inst.out)
	// 以包内版本为准报告（安装后版本读回由 dpkg-query 另行校验，避免时钟/缓存歧义）
	res.Version = newVer
	res.OK = true
	res.Status = inst.status
	res = m.stamp(res, job)
	m.RecordResult(res)
	return res, nil
}

// Rollback 回退到归档中的**上一版本**（FR-OPS-002）：版本序上严格低于当前版本的最大者。
//
// 为什么不能是「归档里版本最高且非当前」（修 round49 缺陷）：当归档里存着比**当前**版本更高的
// 包时（例如装着 3.0.30、归档里留着 3.0.31），那样会把更高的包「回退」上去——契约写的是
// 「回退到上一版本」（docs/NFViS-openapi.yaml 的 /system/software:rollback），行为却成了升级。
// 更危险的是预发布版：旧比较器把 `~` 当普通分隔符，`3.0.30~dev1` 排在 `3.0.30` **之后**，
// 于是「最高且非当前」会选中预发布版（甚至把它装到比它还新的正式版机器上）。
// 升级/换版本应走 `request system software add`，由操作者显式点名目标版本。
//
// 判定全部交给 CompareDebVersion（Debian 语义）：
//   - 归档里没有高于当前版本的包时，结果与旧行为一致（选出最大者）；
//   - 当前版本可能不在归档中（手工 dpkg -i 装上的版本）——此时仍按版本大小判断，不放弃；
//   - 没有任何更低版本时**如实报错并列出归档可见版本**，绝不往上装。
func (m *SoftwareManager) Rollback(ctx context.Context) (SoftwareResult, error) {
	cur := m.currentVersion(ctx)
	res := SoftwareResult{Action: "rollback", Previous: cur}
	cands, err := m.archived()
	if err != nil {
		return res, err
	}
	if len(cands) == 0 {
		return res, fmt.Errorf("无可回退的版本：归档目录 %s 为空（仅能回退到经 request system software add 安装过的版本）", m.dir)
	}
	// archived 已按版本升序 ⇒ 最后一个「严格低于当前版本」的就是上一版本。
	var pick string
	for _, c := range cands {
		if CompareDebVersion(c.version, cur) < 0 {
			pick, res.Version = c.path, c.version
		}
	}
	if pick == "" {
		// 归档里的版本全部 >= 当前版本：如实说清「为什么没有可回退的」，并把可见版本列出来
		// （操作者据此判断是不是归档没留旧包，而不是以为功能坏了）。
		versions := make([]string, 0, len(cands))
		for _, c := range cands {
			versions = append(versions, c.version)
		}
		return res, fmt.Errorf("无可回退的版本：归档中没有任何低于当前版本 %s 的包（可见版本：%s）；"+
			"回退只安装版本序上更旧的包，要装同版本或更高版本请用 request system software add 显式指定",
			cur, strings.Join(versions, "、"))
	}
	return m.rollbackInstall(ctx, res, pick, res.Version)
}

// RollbackTo 回退到归档中的**指定版本**（FR-OPS-002；round49 收口）。
//
// 由来：命令树一直声明 `request system software rollback [to <版本>]`（tree_oper.go），
// 但 CLI 解析后**丢掉了 `to`**、REST 端点也没有版本参数——于是操作者点名目标版本会得到一个
// 「回退完成：A → B」，而 B 是自动选版的结果，与点名无关。这是「参数被接受但静默无效」，
// 比拒绝更危险：救援动作看起来按操作者的意思执行了。本函数把指定版本真正落地。
//
// 三类请求都如实拒绝，不静默改道：
//   - 归档里没有该版本 → 报错并列出可见版本（并指向 `request system software add`）；
//   - 目标 = 当前版本 → 回退无意义，报错；
//   - 目标 > 当前版本 → 那是升级，报错并指向 `software add`（与无参回退的选版口径一致：
//     回退只安装版本序上更旧的包）。
func (m *SoftwareManager) RollbackTo(ctx context.Context, version string) (SoftwareResult, error) {
	cur := m.currentVersion(ctx)
	res := SoftwareResult{Action: "rollback", Previous: cur}
	version = strings.TrimSpace(version)
	if version == "" {
		return res, fmt.Errorf("回退目标版本不能为空（用法：request system software rollback to <版本>）")
	}
	// 先与当前版本比：等于/高于当前时，结论与「归档里有没有这个包」无关，先说清最贴切的原因
	// （真机验证发现：等于当前而归档无此包时，此前只报「归档中没有该版本」，操作者要多想一步）。
	switch CompareDebVersion(version, cur) {
	case 0:
		return res, fmt.Errorf("目标版本 %s 已是当前版本，无需回退", version)
	case 1:
		return res, fmt.Errorf("目标版本 %s 高于当前版本 %s：那是升级不是回退，请用 request system software add 显式安装",
			version, cur)
	}
	cands, err := m.archived()
	if err != nil {
		return res, err
	}
	var pick string
	versions := make([]string, 0, len(cands))
	for _, c := range cands {
		versions = append(versions, c.version)
		if CompareDebVersion(c.version, version) == 0 {
			pick = c.path
		}
	}
	if pick == "" {
		if len(versions) == 0 {
			return res, fmt.Errorf("无可回退的版本：归档目录 %s 为空（仅能回退到经 request system software add 安装过的版本）", m.dir)
		}
		return res, fmt.Errorf("归档中没有版本 %s（可见版本：%s）；回退只安装归档里已有的包，"+
			"其它来源请用 request system software add 显式安装", version, strings.Join(versions, "、"))
	}
	return m.rollbackInstall(ctx, res, pick, version)
}

// rollbackInstall 执行一次回退安装（Rollback 与 RollbackTo 共用；version 是要安装的目标版本）。
func (m *SoftwareManager) rollbackInstall(ctx context.Context, res SoftwareResult, pick, version string) (SoftwareResult, error) {
	res.Version = version
	job := installJob{
		action: "回退", act: "rollback",
		staging: pick, canonical: pick, promote: false,
		targetVer: version,
		attempt:   strconv.FormatInt(m.nowFn().UnixNano(), 10),
		retryHint: "执行 dpkg -i " + pick + " 重试",
	}
	inst := m.installAndVerify(ctx, job)
	if !inst.ok {
		m.RecordResult(m.stamp(SoftwareResult{Action: "rollback", Version: res.Version,
			Package: filepath.Base(pick), Status: inst.status, Reason: inst.reason, OK: false}, job))
		if inst.unverified {
			res.Package, res.Output = filepath.Base(pick), strings.TrimSpace(inst.out)
			res.Version = m.currentVersion(context.Background())
			return res, nil
		}
		return res, errors.New(inst.reason)
	}
	res.Package, res.Output = filepath.Base(pick), strings.TrimSpace(inst.out)
	// 版本读回不继承调用方 ctx：回退同样会重启 nfvisd，那个 ctx 可能已随重启失效。
	res.Version = m.currentVersion(context.Background())
	res.OK = true
	// 修 BUG-17：同样只落**观测到**的状态行（曾恒写 install ok installed）。
	res.Status = inst.status
	res = m.stamp(res, job)
	m.RecordResult(res)
	return res, nil
}

// archived 列出归档包（按版本升序，比较器为 Debian 语义的 CompareDebVersion）。
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
	sort.Slice(out, func(i, j int) bool { return CompareDebVersion(out[i].version, out[j].version) < 0 })
	return out, nil
}

// CompareDebVersion 按 Debian 的版本比较规则比较两个版本号（等价于 dpkg
// `--compare-versions`）：a<b 返回 -1、a==b 返回 0、a>b 返回 1。
//
// 为什么不能沿用「按 `.`/`-`/`+`/`~` 切段再逐段比」的朴素做法（本函数取代的旧实现，
// 缺陷见 round49）——四条都会给出错误结论：
//   - `~` 的排序位置特殊：它排在**任何**字符之前，**包括段末**。故
//     `3.0.30~dev1 < 3.0.30 < 3.0.30.1`；朴素做法把 `~` 当分隔符切掉，`3.0.30~dev1`
//     变成「段数更多的 3.0.30」⇒ 排在正式版之后，于是一个**预发布版**被当成「更新」的版本
//     （回退时正好选中它）；
//   - 数字段要按**数值**比：`3.0.9 < 3.0.31`（字典序会给出反结论），
//     `3.0.30~dev10 > 3.0.30~dev9`；
//   - 数字段与字母段交错时，同一位置上的数字段**更小**（order('1')=0 < order('a')=97）：
//     `1.0.1 < 1.0.a`；而两侧都停在数字段上时先比长度/数值，故 `1.0.10 > 1.0.1a`（"10" 比 "1" 长）；
//   - epoch（`1:1.0`）优先于其余全部字段：`1:1.0 > 2.0`。
//
// 形态 `[epoch:]upstream[-revision]`：epoch 缺省 0（`0:2.0` 与 `2.0` 相等）；revision 取
// **最后一个** `-` 之后的部分，没有 `-` 时按空串（空串小于任何非空 revision，故
// `3.0.30 < 3.0.30-1`）。规则与 dpkg 的 verrevcmp 同构（见 debVerRevCmp），故可与之逐一对照。
func CompareDebVersion(a, b string) int {
	ae, au, ar := splitDebVersion(a)
	be, bu, br := splitDebVersion(b)
	// epoch 是纯数字串，交给同一个比较器按数值比（空串 = 0）。
	if c := debVerRevCmp(ae, be); c != 0 {
		return c
	}
	if c := debVerRevCmp(au, bu); c != 0 {
		return c
	}
	return debVerRevCmp(ar, br)
}

// compareVersion 保留为 CompareDebVersion 的薄包装。
//
// 历史：它曾是「切段 + 逐段比」的简化比较器（且语义与 dpkg 不一致，见 round49）；现在
// 只做转发，全仓唯一的调用点是 archived() 的排序。保留它是因为「归档排序用哪个比较器」
// 是一个被测试钉住的内部点，改名会让守护与历史记录对不上。
func compareVersion(a, b string) int { return CompareDebVersion(a, b) }

// splitDebVersion 把 `[epoch:]upstream[-revision]` 拆成三部分。
//
// epoch 只在前缀是**纯数字**时成立（与 dpkg 的 parseversion 一致）：`1:1.0` 的 epoch 是 1；
// 冒号前不是纯数字时（畸形输入）整串都算 upstream，不做截断——畸形值仍要有确定性的序，
// 而不是让整条命令报错。
func splitDebVersion(v string) (epoch, upstream, revision string) {
	rest := v
	if i := strings.IndexByte(v, ':'); i > 0 && isASCIIDigits(v[:i]) {
		epoch, rest = v[:i], v[i+1:]
	}
	if i := strings.LastIndexByte(rest, '-'); i >= 0 {
		return epoch, rest[:i], rest[i+1:]
	}
	return epoch, rest, ""
}

// debVerRevCmp 比较单个片段（upstream 或 revision 或 epoch），等价 dpkg 的 verrevcmp。
//
// 交替推进：先取两侧的**非数字段**逐字符比（次序见 debVerOrder），再取**数字段**按数值比
// （跳前导零 → 比长度 → 比字典序，等价于比数值且不会整数溢出）。任一轮分出胜负即返回。
func debVerRevCmp(a, b string) int {
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		// 非数字段：只要还有一侧停在非数字字符上就继续比。
		// 段末按 debVerOrder 的 0 分支（256）参与比较：它排在字母之后、`.` 之前——配合
		// `~` 的 -1，得到 `3.0.30~dev1 < 3.0.30`；配合 `.` 的 302，得到 `1.0 < 1.0.0`。
		for (i < len(a) && !isASCIIDigit(a[i])) || (j < len(b) && !isASCIIDigit(b[j])) {
			ac, bc := debVerOrder(debVerChar(a, i)), debVerOrder(debVerChar(b, j))
			if ac != bc {
				if ac < bc {
					return -1
				}
				return 1
			}
			i++
			j++
		}
		// 跳过前导零：`1.0.01 == 1.0.1`（与 dpkg 一致）。
		for i < len(a) && a[i] == '0' {
			i++
		}
		for j < len(b) && b[j] == '0' {
			j++
		}
		// 数字段：先比长度（去掉前导零后更长者数值更大），再比字典序。
		si, sj := i, j
		for i < len(a) && isASCIIDigit(a[i]) {
			i++
		}
		for j < len(b) && isASCIIDigit(b[j]) {
			j++
		}
		na, nb := a[si:i], b[sj:j]
		if len(na) != len(nb) {
			if len(na) < len(nb) {
				return -1
			}
			return 1
		}
		if na != nb {
			if na < nb {
				return -1
			}
			return 1
		}
	}
	return 0
}

// debVerChar 取片段的第 i 个字符；越界返回 0（NUL），与 dpkg 读 `*a`/`*b` 的形态一致——
// 「段末」也是一个可比较的取值。
func debVerChar(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return 0
}

// debVerOrder 单字符的排序权重，逐字对齐 dpkg 的 order()：
//
//	数字 → 0；字母 → 其 ASCII 值；`~` → -1；段末（NUL）→ 256；其它（`.`/`+`/`-`…）→ 值 + 256。
//
// 于是 `~` < 数字 < 字母 < 段末（NUL）< 其它（`.`/`+`/`-`…）：`~` 排在一切之前（含段末），
// 同一位置上的数字段小于字母段（order 0 < order('a')=97），而**段末排在字母之后、`.` 之前**
// ——这正是 `1.0.1a < 1.0.1`（dpkg 的著名怪癖，故预发布必须用 `~`）与 `1.0 < 1.0.0` 的来源。
func debVerOrder(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return 0
	case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
		return int(c)
	case c == '~':
		return -1
	case c == 0:
		return 256
	}
	return int(c) + 256
}

// isASCIIDigit 判断单字节是否是十进制数字（版本号只允许 ASCII）。
func isASCIIDigit(c byte) bool { return c >= '0' && c <= '9' }

// isASCIIDigits 判断字符串非空且全为十进制数字（epoch 的判据）。
func isASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isASCIIDigit(s[i]) {
			return false
		}
	}
	return true
}

// installJob 一次安装/回退尝试的全部输入：脱离脚本按它生成（含目标版本与尝试标识——
// 修 BUG-18：脚本终判必须比对目标版本并吸收 dpkg 退出码，缺一个都会把「包没被改动」
// 判成成功），进程侧判定也按它做（版本核对 + 退出码否决 + 失败文案的重试指引）。
type installJob struct {
	action    string // 展示用动作名（安装/回退）
	act       string // 结论里的 action（add/rollback）
	staging   string // dpkg -i 执行的包路径（Add=暂存名；Rollback=归档原件）
	canonical string // 成功后晋升的归档名（Add；Rollback 与 staging 同）
	promote   bool   // 脱离 shell 是否在 rc==0 后把 staging 晋升为 canonical
	targetVer string // 目标版本（脚本终判与进程侧版本核对的同一真源）
	attempt   string // 尝试标识（结论 attempt 字段、install.rc 前缀、unit 名后缀）
	retryHint string // 失败文案里的重试指引（用户还能拿到的路径/命令）
}

// installAndVerify 安装/回退的统一收口：下发 → 以 dpkg 数据库里的**真实状态与版本**判定
// 结果 → 停掉不再需要写结论的后台脚本。
//
// 为什么不能看命令退出码：升级时 postinst 会 `systemctl try-restart nfvis.service`，而本函数
// 的调用方（CLI/REST）就活在那个被重启的进程里——命令侧看到的「被终止」与「包是否装好」
// 是两件事（真机 v3.0.24：报 signal: terminated，包随后是 iF）。但** dpkg 自身的退出码**
// 不在此列（修 BUG-18）：它在脱离 shell 里跑完才落下，坏包在校验阶段就被拒、包根本没动过，
// 此时状态/版本/audit 的「全绿」是旧包的读数，退出码是唯一能戳穿它的证据。
//
// 判定顺序（judgeInstall）：
//  1. 判定一律先等一次稳定观测（见 settlePackage）——dpkg 退出 0 并不代表事务已收尾；
//  2. 判定只认 `dpkg-query` 的 `install ok installed`，**版本必须是本次的目标版本**，
//     且 **dpkg 退出码必须为 0**（同版本重装失败时版本核对恒真，退出码是最后一道）；
//  3. 停在 half-configured 等「已解包未配置」的中间态时，补救执行一次 `dpkg --configure nfvis`
//     （补救成功即算成功——configure 自身的成功就是证据，不要求 dpkg -i 那次退出 0）；
//  4. 仍不到位就如实报出**真实状态**与**恢复命令**，不谎称原因、也不承诺并不存在的自动回退。
func (m *SoftwareManager) installAndVerify(ctx context.Context, job installJob) installOutcome {
	at := m.installDeb(ctx, job)
	oc := m.judgeInstall(ctx, job, at)
	// 进程侧结论已出：脱离脚本的采样/落笔到此为止。只要 dpkg 已退出（rc 已知），
	// 停掉它的 transient unit——否则它会晚 ~12s 用自己的判定**覆盖**这里的结论
	//（BUG-18 场景 B 真机实证：02:06:30 进程侧写下正确的 ok:false，02:06:31 被脚本
	// 覆盖成 ok:true 且 version 还是旧包号）。rc 未知时**不停**：dpkg 可能仍在事务中，
	// 杀它只会制造 BUG-17 那种 half-configured。unverified 也不停：脚本终判是那时
	// 唯一还会落笔的结论。
	if !oc.unverified {
		m.stopDetachedScript(at)
	}
	return oc
}

// judgeInstall 只看事实做判定（从 installAndVerify 拆出，便于先得出结论、再决定
// 是否停掉后台脚本）。
func (m *SoftwareManager) judgeInstall(ctx context.Context, job installJob, at installAttempt) installOutcome {
	obs := m.settlePackage()
	// dpkg 退出码在判定前再读一次：installDeb 只等了 dpkgRCWait，慢安装的 rc 可能
	// 在那之后才落——而稳定观测（≥settleMinSpan）天然长于 dpkg 收尾，此刻读得到。
	rc, rcKnown := m.readDpkgRC(at)
	if rcKnown {
		at.rc, at.rcKnown = rc, true
		at.log = m.readInstallLog(at.logPath) // dpkg 已退出，install.log 此刻才有全文
	}
	if obs.stable && installSettled(obs.status, obs.statusErr, obs.version, obs.verErr, obs.auditClean, obs.auditErr, job.targetVer) {
		// 修 BUG-18 场景 A（同版本坏包）：状态/版本/audit 全绿可能是**旧包本来就是这个
		// 状态**——dpkg 在校验阶段就失败、包根本没动过；版本核对在同版本重装时恒真。
		// 只有 dpkg 自己的退出码能戳穿：rc!=0 说明这次安装没有正常完成，
		// 「读数与升级前一致」不构成成功证据。
		if rcKnown && rc != 0 {
			return m.rcFailureOutcome(job, obs, at, rc)
		}
		return installOutcome{out: m.installedReport(job.action, at, obs), status: obs.status, version: obs.version, ok: true}
	}
	// 复核拿不到可判读的状态行时**不能**据此判失败——那只是「这个执行通道答不了这个问题」，
	// 旧口径（以命令结果为准）必须保留，否则任何不支持 dpkg-query 的适配都会让升级恒报错。
	// 但**结论里不写成功断言**：没有观测就没有证据（修 BUG-17 的 Cause B）。
	if !obs.observable {
		if at.err == nil {
			return installOutcome{out: m.installedReport(job.action, at, obs), unverified: true,
				reason: fmt.Sprintf("%s命令成功，但无法复核包状态（dpkg-query 未给出可判读的状态行）："+
					"结论不作成功断言，请用 dpkg -l nfvis 核对实际状态。", job.action)}
		}
		return m.failureOutcome(job, obs, at, "")
	}
	// 读数还在变（或观察窗内没稳定过）：判不出，不能报成功；也不宜再插一条 --configure——
	// dpkg 可能仍在推进，并发事务只会把事情弄得更糟（真机实测这时状态会在两者间反复）。
	if !obs.stable {
		return m.failureOutcome(job, obs, at, "")
	}
	// 只对「已解包、未配置」的中间态做补救：一次 configure 就能收口；而「包根本没落地」
	//（not-installed）补救没有意义（那要先重装），直接如实报错。
	// 补救本身会再跑一遍 postinst（同样重启 nfvisd），故与安装走同一条脱离路径。
	recovery := ""
	// 补救触发条件放宽：状态行说「已解包未配置」**或** audit 不干净（后者最关键——真机实证
	// dpkg 被打断时状态行仍写 installed，而 audit 列出该包；此时只按状态行不会补救，
	// 包就永远停在 half-configured）。audit 判据带来的是「能自救」而不只是「能报错」。
	needsRecovery := isConfigurableStatus(obs.status) ||
		(obs.auditErr == nil && !obs.auditClean && obs.status == pkgStatusInstalled)
	if needsRecovery {
		rec := m.runDetached(ctx, "dpkg --configure nfvis")
		recovery = "dpkg --configure nfvis: " + strings.TrimSpace(joinLines(rec.text(), errText(rec.err)))
		// 补救之后同样要等稳定观测：configure 会重跑 postinst，瞬态读数在这里一样会骗人。
		obs = m.settlePackage()
		if obs.stable && installSettled(obs.status, obs.statusErr, obs.version, obs.verErr, obs.auditClean, obs.auditErr, job.targetVer) {
			// 补救成功：dpkg -i 那次的非零退出码被 configure 的成功解释了
			//（configure 自身的成功就是「包已收口」的证据），不因 rc!=0 翻回失败。
			return installOutcome{out: m.installedReport(job.action, at, obs, recovery),
				status: obs.status, version: obs.version, ok: true}
		}
	}
	return m.failureOutcome(job, obs, at, recovery)
}

// rcFailureOutcome dpkg 以非零退出、而状态读数却「全绿」时的收口（BUG-18 场景 A）。
//
// 这个组合的真身是「包根本没被改动」（坏包在校验阶段被拒、依赖冲突在解包前被拒…）：
// 旧包的 installed + audit 干净 + 版本相同让一切状态判据失明。失败文案必须点名这一点，
// 并带上 dpkg 的真实输出（经 install.log 回传——修 R44-1 后 dpkg 的报错才真正落进日志）。
func (m *SoftwareManager) rcFailureOutcome(job installJob, obs pkgObservation, at installAttempt, rc int) installOutcome {
	ver := obs.version
	if ver == "" {
		ver = "（未知）"
	}
	msg := fmt.Sprintf("%s未完成：dpkg -i 失败（退出码 %d），包未被更新——状态/版本读数（%s / %s）"+
		"与升级前一致，不构成成功证据。恢复：%s；仍失败请查看安装日志 %s 的 dpkg 输出。",
		job.action, rc, obs.status, ver, job.retryHint, m.installLogPath())
	// dpkg 的原始报错是操作者最需要的东西（坏包在校验阶段被拒的那一行）——
	// 它在 install.log 里，但把它（尾部）直接带进失败文案，操作者不必再去翻文件。
	if tail := tailLines(at.log, 3); tail != "" {
		msg += "\ndpkg 输出（尾部）:\n" + tail
	}
	return installOutcome{out: strings.TrimSpace(at.text()), version: obs.version, ok: false, reason: msg}
}

// tailLines 取文本的最后 n 个非空行（截尾而非截头：失败原因通常在输出的末尾）。
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	var keep []string
	for i := len(lines) - 1; i >= 0 && len(keep) < n; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			keep = append([]string{t}, keep...)
		}
	}
	return strings.Join(keep, "\n")
}

// installOutcome 一次安装/回退的**判定结论**。
//
// status/version 只放**稳定观测到**的事实（见 settlePackage）：读数不稳或观测不可得时留空，
// 原因写进 reason。把瞬态读数填进 status 正是本条缺陷的成因——记录写着 installed、
// 包实际半配置，而这份错误记录跨重启不自愈。
type installOutcome struct {
	out     string // 操作者可见文本（命令输出 + 判定说明）
	status  string // 稳定观测到的 dpkg 状态行（无稳定观测则空）
	version string // 观测到的已装版本
	ok      bool   // 是否真正落定为成功
	reason  string // 失败/判不出时的原因（供 API/结论文件使用）
	// unverified 复核通道答不了这个问题（dpkg-query 未给出可判读的状态行），但命令本身成功。
	// 与「失败」「判不出」三者分开：调用方沿用旧口径（不报错），而**落盘结论不写成功**。
	unverified bool
}

// pkgObservation 一次「稳定观测」的结果：判定只看它，不看单次读数。
type pkgObservation struct {
	status    string
	statusErr error
	version   string
	verErr    error
	// auditClean dpkg --audit 不列 nfvis（**真装好**的数据库视角）。
	// 与 status 并列而非替代：dpkg 被打断时 status 可能仍写 installed 而 audit 列出该包。
	auditClean bool
	auditErr   error
	// observable 这个执行通道是否答过「可判读的状态行」。false = dpkg-query 根本不提供
	// 这个答案，等待不会改变它（与「暂时读不到」区别对待）。
	observable bool
	// stable 同一状态行连续出现 ≥installSettleSamples 次（跨过 settleMinSpan）。
	stable bool
	// samples / waited 本次观测的规模，供文案如实说明「等了多久、看了几次」。
	samples int
	waited  time.Duration
}

// settlePackage 等到**稳定的**包状态，再交给判定。
//
// 为什么必须等（真机 v3.0.26，0.5s 采样 3/3 复现）：升级期间包状态会在 postinst 运行中短暂
// 读作 `install ok installed`（实测约 7s、12 个采样），随后 dpkg 数据库短暂不可读（约 20s），
// 最终落到 `install ok half-configured`。产品恰在瞬态窗口采样 ⇒ 报成功并落盘成功结论，
// 与事实相反。故「装好了」必须有**稳定**证据：
//   - 同一状态行连续出现 installSettleSamples 次，两次采样之间隔 installConvergeInterval
//     （隐含最小跨度 settleMinSpan，长于实测瞬态；不是把同一瞬间重读几遍）；
//   - 上界复用 installConvergeTimeout（不再引入第二套超时概念），越界即如实返回「没稳定」。
//
// 读数失败（dpkg 事务期间常见）不构成证据：稳定候选从头开始计。
func (m *SoftwareManager) settlePackage() pkgObservation {
	start := time.Now()
	deadline := start.Add(installConvergeTimeout)
	var obs pkgObservation
	cand := "" // 当前连续出现的状态行（空 = 还没有候选）
	same := 0  // 该候选已连续出现的次数
	for {
		status, serr := m.packageStatus()
		version, verr := m.installedVersion()
		// audit 与状态行**一起**构成判据：dpkg 被打断时状态行可能仍写 installed 而
		// audit 列出该包（真机实证：该窗口持续 16s+，见 packageAuditClean 的注释）。
		// audit 读不到时按「不可判定」处理（空候选），不当作干净。
		auditClean, aerr := m.packageAuditClean()
		obs.samples++
		obs.status, obs.statusErr = status, serr
		obs.version, obs.verErr = version, verr
		obs.auditClean, obs.auditErr = auditClean, aerr
		obs.waited = time.Since(start)
		if serr == nil && status != "" && !statusKnown(status, nil) {
			// 这个通道答的不是状态行：再等只会拿到同一个形状，立即如实返回「不可观测」。
			return obs
		}
		if statusKnown(status, serr) {
			obs.observable = true
			// 候选键 = 「状态行 + audit 是否干净」：audit 变脏等于状态变了，计数必须重来。
			key := status
			if aerr != nil || !auditClean {
				key = status + "|audit-dirty"
			}
			if key != cand {
				cand, same = key, 1
			} else {
				same++
			}
			if same >= installSettleSamples {
				obs.stable = true
				return obs
			}
		} else {
			cand, same = "", 0
		}
		if !time.Now().Before(deadline) {
			return obs
		}
		time.Sleep(installConvergeInterval)
	}
}

// installSettled 判定「这次安装真的落定了」：状态必须是 install ok installed，**且**版本是
// 目标版本。两个条件缺一不可——半配置（half-configured/unpacked）时 dpkg 已经把 Version 改成
// 新版本、旧包没被触碰时状态又本来就是 installed：只看其中一个都会得出错误结论。
func installSettled(status string, serr error, installed string, verr error, auditClean bool, auditErr error, target string) bool {
	if serr != nil || status != pkgStatusInstalled {
		return false
	}
	// audit 必须干净：dpkg 被打断时状态行仍可能是 installed 而实际半配置（真机实证 16s+ 窗口）。
	// audit 读不到 ⇒ 拿不到证据 ⇒ 不算落定（宁可报「无法判定」也不谎报成功）。
	if auditErr != nil || !auditClean {
		return false
	}
	if target == "" || verr != nil || installed == "" {
		// 版本这一路取不到证据（适配层不答/查不到）时不据此判失败。
		return true
	}
	return installed == target
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

// installedReport 判定为成功时给操作者的说明。
//
// 只有**稳定观测到** install ok installed 才会走到这里（见 settlePackage），所以这里可以
// 明确说「已实际生效」。命令侧被打断也要说明：操作者最需要知道的是升级真的发生了、
// nfvisd 会（或已经）重启——而不是命令的结束码。
func (m *SoftwareManager) installedReport(action string, at installAttempt, obs pkgObservation, extra ...string) string {
	var head string
	if at.err != nil && looksInterrupted(at.err) {
		head = fmt.Sprintf("%s命令的结束状态不可信（%v）——已按 dpkg 数据库复核：包已是 %s，本次%s已实际生效。"+
			"nfvisd 由安装脚本重启以加载新版本；若本会话中途断开，属预期（重启的正是执行本命令的进程）。",
			action, at.err, obs.status, action)
	}
	parts := append([]string{head, at.log}, extra...)
	return strings.TrimSpace(joinLines(parts...))
}

// failureOutcome 真失败/判不出时的收口：点名包的**真实状态**与**照做的恢复命令**，
// 并把同一个原因同时给文本（out）与结论（reason），两者不各写一份、避免漂移。
//
// 四种形态分开说，因为操作者的下一步完全不同：
//   - 读数仍在变（观察窗内没稳定过）→ 如实说「无法判定」，不动手（dpkg 可能还在跑）；
//   - 读数取不到（通道答不了）→ 说「无法复核」，同样不声称任何结论；
//   - 稳定地停在某个非 installed 状态 → 给恢复命令；
//   - 状态已是 installed 但版本不是目标（修 BUG-19）→ 主句说**版本**：旧文案
//     「当前状态为 "install ok installed"（未达到 "install ok installed"）」两侧是同一个
//     字符串，读起来像逻辑错误，而操作者最需要读懂报错的正是这一刻。
func (m *SoftwareManager) failureOutcome(job installJob, obs pkgObservation, at installAttempt, recovery string) installOutcome {
	state := obs.status
	switch {
	case obs.statusErr != nil:
		state = fmt.Sprintf("读不到（dpkg-query 失败: %v）", obs.statusErr)
	case state == "":
		state = "空（dpkg 数据库里查不到 nfvis）"
	}
	out := at.text()
	if recovery != "" {
		out = joinLines(out, recovery)
	}
	var msg string
	switch {
	case !obs.observable:
		// 通道答不了这个问题：不猜结论，也不给「恢复命令」（没有事实依据去让人重装）。
		msg = fmt.Sprintf("%s结果无法判定：读不到可判读的 nfvis 包状态（dpkg-query 未返回状态行）。"+
			"请用 dpkg -l nfvis 核对实际状态；确认是 half-configured 时执行 dpkg --configure nfvis 收口。", job.action)
	case !obs.stable:
		// 关键的一类（真机形态）：等在 installConvergeTimeout 内没能稳定下来。
		// 这时**不能**报成功——曾经的瞬态 installed 就是这样被当成终态的。
		// 文案给出「同一读数需持续多久才算数」（settleMinSpan），操作者据此判断要不要再等一等。
		msg = fmt.Sprintf("%s结果无法判定：等了 %s（共 %d 次采样），包状态始终没有连续 %s 保持不变"+
			"（最后一次为 %q；它在安装/配置过程中会短暂显示为 %q）。此时不宣称成功、也不自动补救"+
			"（dpkg 可能仍在推进）。请稍后用 dpkg -l nfvis 复核；若停在 half-configured，"+
			"执行 dpkg --configure nfvis 收口，否则%s。",
			job.action, installConvergeTimeout, obs.samples, settleMinSpan(), state, pkgStatusInstalled, job.retryHint)
	default:
		// 恢复命令按状态给出：已解包/半配置 → configure 即可；其余 → 重新下发同一个包。
		var fix string
		if obs.statusErr == nil && isConfigurableStatus(obs.status) {
			fix = fmt.Sprintf("恢复：执行 dpkg --configure nfvis（当前状态 %s）；仍失败时%s。", obs.status, job.retryHint)
		} else {
			fix = fmt.Sprintf("恢复：%s；仍失败请查看安装日志 %s 的 dpkg 输出。", job.retryHint, m.installLogPath())
		}
		if obs.statusErr == nil && obs.status == pkgStatusInstalled && obs.version != "" && obs.version != job.targetVer {
			// 修 BUG-19：状态行已达标、差的是版本——主句说版本，不再把状态写进
			// 「未达到」的宾语里（那正是自相矛盾文案的来源）。
			msg = fmt.Sprintf("%s未完成：包未更新到目标版本——已装 %s（目标 %s），状态 %q。%s",
				job.action, obs.version, job.targetVer, obs.status, fix)
		} else {
			msg = fmt.Sprintf("%s未完成：包 nfvis 当前状态为 %q（未达到 %q）。%s",
				job.action, state, pkgStatusInstalled, fix)
			if obs.verErr == nil && obs.version != "" {
				msg += fmt.Sprintf(" 已安装版本为 %s。", obs.version)
			}
		}
	}
	if at.err != nil {
		msg += fmt.Sprintf(" 底层错误: %v。", at.err)
	}
	// dpkg 的原始报错直接带进失败文案（尾部）：它是操作者最需要的东西——
	// 「为什么没装上」的答案在 dpkg 的输出里，不该让人去翻文件（完整输出仍在 install.log）。
	if at.rcKnown && at.rc != 0 {
		if tail := tailLines(at.log, 3); tail != "" {
			msg += "\ndpkg 输出（尾部）:\n" + tail
		}
	}
	// status 只带**稳定观测到**的事实：不稳/不可观测时不把最后那次读数写进结论文件，
	// 否则「记录里的一行」又会被后来的读视图当成事实引用。
	status := ""
	if obs.stable {
		status = obs.status
	}
	return installOutcome{out: strings.TrimSpace(out), status: status, version: obs.version, ok: false, reason: msg}
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

// packageStatus 读 dpkg 数据库里 nfvis 的状态行（如 `install ok installed`）。
func (m *SoftwareManager) packageStatus() (string, error) {
	return m.queryPackage("${Status}")
}

// packageAuditClean dpkg 的**数据库一致性**视角：`dpkg --audit` 不列 nfvis 才算真装好。
//
// 为什么状态行不够（真机实证，本轮最重要的一条）：dpkg 1.23 在配置被信号打断时，
// `/var/lib/dpkg/status` 的 `Status:` 可能**仍然写着 `install ok installed`**，而
// `dpkg.log` 与数据库内部记账是 `half-configured`——此时只有 `dpkg --audit` 会把它列出来：
//
//	12:05:52  status half-configured        （dpkg.log）
//	至今       Status: install ok installed （status 文件字段，未回退）
//	dpkg --audit → "nfvis  … only half configured"
//
// 实测该「状态字段假装 installed」的窗口持续 **16 秒以上**（12:05:52–12:06:08 采样）。
// 只按状态行判定就会在这个窗口里判成功——这正是 BUG-17 的全部来源。
// 故收敛判据改为「状态行 installed **且** audit 干净」，两者缺一不算落定。
//
// 读不到 audit（命令失败）时返回 error：调用方据此「无法判定」，不猜。
func (m *SoftwareManager) packageAuditClean() (bool, error) {
	out, err := m.runBounded(installVerifyTimeout, "dpkg", "--audit")
	if err != nil {
		return false, fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
	}
	// audit 对**其它**包的半装也会报；只关心 nfvis 这一行（实测格式：` nfvis   <desc>`）。
	for _, ln := range strings.Split(out, "\n") {
		f := strings.Fields(ln)
		if len(f) > 0 && f[0] == "nfvis" {
			return false, nil
		}
	}
	return true, nil
}

// installedVersion 读 dpkg 数据库里的 nfvis 版本（与状态分开查：状态不区分版本）。
func (m *SoftwareManager) installedVersion() (string, error) {
	return m.queryPackage("${Version}")
}

// queryPackage 读 dpkg 数据库里的某一个字段（状态行 / 版本）。
//
// 单一实现：`dpkg-query` 的两条读法只差格式串，各写一份迟早漂移（例如只给其中一处加
// 有界性）。上界走 runBounded，且不继承调用方 ctx——安装会重启执行本命令的进程。
func (m *SoftwareManager) queryPackage(format string) (string, error) {
	out, err := m.runBounded(installVerifyTimeout, "dpkg-query", "-W", "-f="+format, "nfvis")
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
	}
	return strings.TrimSpace(out), nil
}

// PackageState 当前 nfvis 包的 dpkg 状态行与已装版本（供读视图交叉核对）。
//
// 与内部的 packageStatus/installedVersion 同源（同一个 queryPackage），只是导出给 api 层用。
// **有意不等稳定**：这是显示用的即时读数，不是判定——升级进行中「现在是 half-configured」
// 本就是操作者要看到的事实，读口会把它标注为「当前状态」。
// 出错时同时返回已经拿到的那个字段（可能为空）：调用方要能区分「读失败」与「状态是 X」。
func (m *SoftwareManager) PackageState(ctx context.Context) (status, version string, err error) {
	_ = ctx // 读数是**判定/显示**用的事实查询，不随请求取消：调用方进程重启期间也要答得出
	status, serr := m.packageStatus()
	version, verr := m.installedVersion()
	if serr != nil {
		return status, version, serr
	}
	return status, version, verr
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
//
// 修 R44-1 的重试一半：以前判「撞锁」看的是 launchShell 的退出码（恒 0）与刚 launch
// 完、多半还没落盘的日志——真机上重试**从未触发过**。现在 dpkg 的退出码经 install.rc
// 传回，坏包/撞锁这类秒级失败在 dpkgRCWait 内就能读到，重试第一次真正可用。
func (m *SoftwareManager) installDeb(ctx context.Context, job installJob) installAttempt {
	const attempts = 6
	var at installAttempt
	for i := 0; i < attempts; i++ {
		at = m.installOnce(ctx, job)
		if at.err == nil {
			if rc, known := m.waitDpkgRC(at, dpkgRCWait); known {
				at.rc, at.rcKnown = rc, true
				at.log = m.readInstallLog(at.logPath) // dpkg 已退出，日志此刻才有全文
				if rc == 0 || !isFrontendLock(at.text()) {
					return at
				}
			} else {
				// install.rc 没出现：dpkg 多半仍在正常推进，等待由 settlePackage 承担。
				return at
			}
		} else if !isFrontendLock(at.text()) {
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

// isFrontendLock dpkg 前端锁被占用的输出签名（apt/unattended-upgrades 在跑）。
func isFrontendLock(text string) bool {
	return strings.Contains(text, "dpkg frontend lock") || strings.Contains(text, "frontend lock was locked")
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
func (m *SoftwareManager) installOnce(ctx context.Context, job installJob) installAttempt {
	logPath := m.installLogPath()
	_ = os.Remove(logPath) // 上一次的残渣会误导本次排障
	_ = os.Remove(m.rcPath())
	// 修 BUG-16（关键一处）：结论必须由**脱离执行的那个 shell 自己**在 dpkg 结束后写出。
	// 为什么不能等回到本进程再写：postinst 会重启 nfvisd，本进程在那时已被杀——`Add` 里
	// 后面的 RecordResult 根本执行不到（真机实证：升级成功但结论文件不存在）。
	cmdOut, err, unit := m.launchShell(ctx, logPath, m.installScriptWithResult(job), "nfvis-install", job.attempt)
	return installAttempt{cmdOut: cmdOut, log: m.readInstallLog(logPath), err: err,
		unit: unit, logPath: logPath, attempt: job.attempt}
}

// installScriptWithResult dpkg 安装 + **在同一个脱离 shell 里**落结论与退出码。
//
// 结论文件格式与 ReadLastResultAt 读取的一致（action/attempt/time/version/status/reason/ok）。
//
// 修 BUG-17（Cause C）：取样必须等**同一状态重复出现**才落笔——曾经 `dpkg -i` 一退出就
// 采一次快照，而真机上那一刻正是 `install ok installed` 的瞬态，这份由脱离 shell 写出的
// 结论把瞬态固化成「成功」，而它恰恰是 nfvisd 被杀后 CLI 唯一能读到的东西。
//
// 修 BUG-18（本脚本的三个判据缺口）：
//   - 终判必须比对**目标版本**（字面量，不用采样读到的 $ver——同版本重装失败时 $ver 恰好
//     等于目标、完全失去判别力，且把它写进 version 字段会把「装到了什么版本」记错）；
//   - 必须吸收 **dpkg -i 的退出码**（rc!=0 一票否决——状态/版本/audit 全绿可能是旧包的
//     读数，坏包在校验阶段就被拒、包根本没动过）；
//   - 落笔带 attempt/time，读侧能分清「哪次尝试、多旧的记录」。
//
// 落笔分**两次**，两次都不能谎报：
//   - 一开始先写一条 `pending`（ok:false）：这一半是给 BUG-16 用的——nfvisd 被 postinst 重启
//     后，CLI 重连的第一时间就会来读这个文件，那时本次安装往往还没结束；没有这条，
//     CLI 会读到「无记录」而说不清状态。同时它也顶掉了**上一次**升级的成功结论。
//   - 同一状态行连续出现 installSettleSamples 次、且版本/退出码/audit 全达标才写 ok:true；
//     等不满就如实写 ok:false（状态留最后一次观测 + reason 说明差在哪）。绝不为了
//     「让文件好看」而放宽判据。
func (m *SoftwareManager) installScriptWithResult(job installJob) string {
	// 结论由**脱离执行的那个 shell 自己**在 dpkg 结束后写出（nfvisd 那时已被 postinst 杀掉）。
	//
	// 处关键取舍：
	//   · 状态与版本优先走 `dpkg-query`（数据库真实视图）；读不到（事务期间拿不到锁等）
	//     才回退读 /var/lib/dpkg/status 文件（只作最后手段，它会读到陈旧字段——真机实证
	//     dpkg 被打断时文件说 installed、dpkg-query 说 half-configured）；
	//   · 结论**写进独立文件**，且整段用 `{ … }` 分组把最终输出重定向到该文件：
	//     launchShell 会在整条命令后追加 `>>install.log 2>&1`，若不加分组，脚本内最后一个
	//     重定向会被外层覆盖，结论落进日志而不是结果文件（真机实证：log 里有正确 JSON、
	//     last-result.json 恒为空）；
	//   · 两次落笔都走「临时文件 + mv」：读口可能恰在 nfvisd 刚起来时打开这个文件，直接
	//     覆盖会让它读到半截 JSON——那会被读成「无记录」，操作者据此误判。
	// 拼进 shell 脚本的路径必须用 **POSIX 分隔符**：`filepath.Join` 在 Windows 上产出反斜杠，
	// 而这段文本最终由 Linux 上的 /bin/sh 执行——反斜杠会被 shell 当转义，路径直接失效
	//（单测在 Windows 宿主上跑，曾因此生成 `'\var\lib\…'` 而看不出来）。
	res := posixpath.Join(m.dir, "last-result.json")
	quotedRes := shellQuote(res)
	rcFile := posixpath.Join(m.dir, installRCName)
	quotedRC := shellQuote(rcFile)
	ts := m.nowFn().UTC().Format(time.RFC3339)
	target := shellQuote(job.targetVer)
	// 修 R51-L1：目标版本进脚本一律经 shellQuote（比较操作数本就是这样）。reason 里此前是
	// **裸插值**进双引号字符串——版本号含 `"`/反引号/`$()` 就能破开这条被 BUG-16/17/18 加固过的
	// 脚本。这里改为先把「值」放进 shell 变量，再用 `$tv` 在双引号里取（值的引用与拼接彻底分离，
	// 不依赖 validateDebVersion 是否拦得住——那是第二道）。
	tv := shellQuote(job.targetVer)
	installedLit := shellQuote(pkgStatusInstalled)
	// auditCleanSh `dpkg --audit` 不列 nfvis 的 shell 判据（与 packageAuditClean 同义）。
	// `grep -q "  nfvis "` 只认缩进两空格的包名行，避免匹配到描述文本里的 nfvis。
	auditCleanSh := "! dpkg --audit 2>/dev/null | grep -qE '^ +nfvis( |$)'"
	auditDirtySh := "dpkg --audit 2>/dev/null | grep -qE '^ +nfvis( |$)'"
	// 循环上界与进程内判定同源：脱离脚本不能无限等——它没有调用方在看，卡死会留下一个
	// 永远占着 unit 的空转进程。
	maxIter := int(installConvergeTimeout / installConvergeInterval)
	if maxIter < 1 {
		maxIter = 1
	}
	// sleep 只收秒粒度：间隔不足 1s 时仍按 1s（循环次数同上界一起收敛）。
	interval := int((installConvergeInterval + time.Second - 1) / time.Second)
	if interval < 1 {
		interval = 1
	}
	// awk 只负责从 dpkg 状态文件里取「nfvis 那一段」的 Status 与 Version，输出 `status|version`。
	awk := "awk '" +
		"BEGIN{st=\"\";ver=\"\"} " +
		"/^Package: nfvis$/{inpkg=1;next} inpkg&&/^Status:/{sub(/^Status: /,\"\");st=$0} " +
		"inpkg&&/^Version:/{sub(/^Version: /,\"\");ver=$0} " +
		"inpkg&&/^$/{inpkg=0} " +
		"END{printf \"%s|%s\", st, ver}' /var/lib/dpkg/status"
	// 先落 pending（ok 恒 false）：安装进行中读到它是**诚实**的「未定」，而不是上次的成功。
	pending := "printf '{\"action\":\"%s\",\"attempt\":\"" + job.attempt + "\",\"time\":\"" + ts +
		"\",\"version\":\"\",\"status\":\"" + resultStatusPending + "\",\"ok\":false}\\n' " +
		shellQuote(job.act) + " > " + quotedRes + ".tmp && mv " + quotedRes + ".tmp " + quotedRes
	// dpkg 的退出码必须显式吸收（修 R44-1/BUG-18）：`rc=$?` 紧跟 dpkg，第一时间落到
	// install.rc（进程侧判定读它）；随后 rc==0 才把暂存件晋升为正式归档（坏包不进回退池）。
	dpkgRun := "dpkg -i " + shellQuote(job.staging) + "; rc=$?; " +
		"printf '%s %s\\n' " + shellQuote(job.attempt) + " \"$rc\" > " + quotedRC + ".tmp && mv " + quotedRC + ".tmp " + quotedRC + "; "
	promote := ""
	if job.promote {
		promote = "if [ \"$rc\" -eq 0 ]; then mv " + shellQuote(job.staging) + " " +
			shellQuote(job.canonical) + " || true; fi; "
	}
	// 采样与终判。终判四条同时成立才 ok=true：状态行 installed、audit 干净、**dpkg 退出码
	// 为 0**、**已装版本==目标版本**。差哪一条，reason 就说哪一条——失败结论也要能读懂。
	loop := "{ prev=''; n=0; i=0; st=''; ver=''; tv=" + tv + "; " +
		// 同一状态行连续出现才累加，换值就从头计；中途读不到（空串）同样打断计数——
		// 「一直读不到」与「一直是 installed」是两码事，前者不是证据。
		"while [ \"$i\" -lt " + strconv.Itoa(maxIter) + " ]; do " +
		"pair=$(dpkg-query -W -f='${Status}|${Version}' nfvis 2>/dev/null); " +
		"if [ -z \"$pair\" ]; then pair=$(" + awk + "); fi; " +
		"st=${pair%%|*}; ver=${pair#*|}; " +
		// audit 与状态行一起构成判据：dpkg 被打断时状态行可能仍写 installed 而 audit 列出该包
		//（真机实证：该窗口持续 16s+）。audit 干净才把 key 记成纯状态行，否则键变 ⇒ 计数重来。
		"if " + auditCleanSh + "; then key=\"$st\"; else key=\"$st|audit-dirty\"; fi; " +
		"if [ -n \"$st\" ] && [ \"$key\" = \"$prev\" ]; then n=$((n+1)); else prev=$key; n=1; fi; " +
		"i=$((i+1)); " +
		"if [ \"$n\" -ge " + strconv.Itoa(installSettleSamples) + " ]; then break; fi; " +
		"sleep " + strconv.Itoa(interval) + "; " +
		"done; " +
		"ok=false; " +
		"if [ \"$n\" -ge " + strconv.Itoa(installSettleSamples) + " ] && [ \"$st\" = " + installedLit + " ] && " +
		auditCleanSh + " && [ \"$rc\" -eq 0 ] && [ \"$ver\" = " + target + " ]; then ok=true; fi; " +
		"rsn=''; " +
		"if [ \"$ok\" != true ]; then " +
		"if [ \"$rc\" -ne 0 ]; then rsn=\"dpkg -i 失败（退出码 $rc），包未被更新（当前 $st / $ver）\"; " +
		"elif [ -z \"$st\" ]; then rsn='观察窗内未取得可判读的包状态'; " +
		"elif [ \"$st\" != " + installedLit + " ]; then rsn=\"包状态停在 $st，未达到 install ok installed\"; " +
		"elif " + auditDirtySh + "; then rsn='dpkg --audit 仍列出 nfvis（半配置/被打断）'; " +
		"elif [ \"$ver\" != " + target + " ]; then rsn=\"已装版本 $ver 未达到目标 $tv\"; " +
		"else rsn='状态在观察窗内未稳定（最后观测见 install.log）'; fi; fi; " +
		// 结论的 version 写**目标版本**：这份记录回答「这次要装什么、装成了没」；写采样值
		// 会把「装到了什么版本」记错（真机场景 B：目标 3.0.26、记录写 3.0.28）。
		"printf '{\"action\":\"%s\",\"attempt\":\"" + job.attempt + "\",\"time\":\"" + ts +
		"\",\"version\":\"%s\",\"status\":\"%s\",\"reason\":\"%s\",\"ok\":%s}\\n' " +
		shellQuote(job.act) + " " + target + " \"$st\" \"$rsn\" \"$ok\" > " + quotedRes + ".tmp && mv " + quotedRes + ".tmp " + quotedRes + "; }"
	return "mkdir -p " + shellQuote(m.dir) + "; " +
		"{ " + pending + "; }; " +
		dpkgRun +
		promote +
		loop
}

// resultStatusPending 结论文件里「安装进行中、结论未定」的标记。
//
// 它不是 dpkg 状态行（dpkg 是「want-flag ok|… status」三段式），读视图/操作者一眼能分清
// 「还没定」与「定成了什么」。放在 Status 字段里是为了不改记录格式（CLI 读口按 status/ok 读，
// 新增字段等于改契约）。
const resultStatusPending = "pending"

// runDetached 执行一条**会重启 nfvisd** 的补救命令（dpkg --configure），同样脱离本进程的
// cgroup 并把输出落到安装日志。它没有可重试的锁语义（configure 撞锁就是真失败），故只跑一次。
func (m *SoftwareManager) runDetached(ctx context.Context, command string) installAttempt {
	logPath := m.installLogPath() + ".configure"
	_ = os.Remove(logPath)
	cmdOut, err, _ := m.launchShell(ctx, logPath, command, "nfvis-configure", strconv.FormatInt(m.nowFn().UnixNano(), 10))
	return installAttempt{cmdOut: cmdOut, log: m.readInstallLog(logPath), err: err, logPath: logPath}
}

// launchShell 以「与调用方不同 cgroup/session」的形态执行命令，返回脱离手段自身的诊断输出
// 与 transient unit 名（systemd-run 形态；其余为空）。token 是 unit 名后缀——用本次尝试的
// 标识，让进程侧判定后能按名字停掉它（stopDetachedScript）。
//
// 只跑**一次**：手段由 detacher 事先探定，不在失败后改换手段重跑——dpkg 不幂等，换手段再跑
// 一遍就是重复解包同一颗包。手段本身不可用时由 detacher 提前降级，代价是一次 `/bin/true` 探针。
func (m *SoftwareManager) launchShell(ctx context.Context, logPath, command, unitPrefix, token string) (string, error, string) {
	// 不要用 fmt.Sprintf 拼这段：command 是**shell 脚本**，里面天然含 `%`
	//（`${pair%%|*}`、`printf '…%s…'`）。Sprintf 会把 `%%` 折叠成 `%`、把 `%s` 当格式动词，
	// 结果交给 /bin/sh 的脚本被静默改写（真机实证：`st=${pair%%|*}` 变成 `st=`，
	// 采样永远为空、结论恒停在 pending）。用字符串拼接，一个字符都不动。
	script := m.launchScriptForTest(command, logPath)
	// 修 round44（第三处，且是**决定性**的一处）：脚本必须落成**文件**再用 `sh <file>` 执行，
	// 不能作为 `sh -c "<script>"` 的参数传。
	//
	// 根因：`systemd-run` 会扫描命令参数里的 `NAME=value` 形态并当作**环境变量赋值**处理。
	// 我们的脚本里满是 `${pair%%|*}`、`${pair#*|}`——systemd-run 把它们误判成赋值，
	// 直接报 `Invalid environment variable name evaluates to an empty string: pair%%|*`
	// 并把那一段吞掉，于是 shell 看到的是 `st=` （空），采样恒空、结论永远停在 pending。
	// 真机对照（同一脚本、同一 scope）：
	//   systemd-run … /bin/sh -c "pair=a|b; echo ${pair%%|*}"  → Invalid environment variable …
	//   systemd-run … /bin/sh /tmp/t.sh                        → st=[install ok installed] ✓
	//
	// 落文件还顺带解决三件事：命令行长度无上限、脚本内容不经多层引号、排障时可直接看文件。
	scriptPath, werr := m.writeLaunchScript(script)
	if werr != nil {
		return "", fmt.Errorf("写执行脚本: %w", werr), ""
	}
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
	unit := fmt.Sprintf("%s-%s", unitPrefix, token)
	switch m.detacher(ctx) {
	case detachSystemdRun:
		// 系统级 unit：自持 cgroup，重启 nfvisd 波及不到；单元名带固定前缀便于排障
		//（只放 nfvis-install / nfvis-configure 两族，异常残留一眼能认出来）。
		// 用 **transient service**（不带 --scope）而不是 --scope：
		// `--scope` 让命令活在**调用方的 cgroup 里**，父进程（nfvisd）被 postinst 重启时
		// 整组会被 systemd 收走——脚本后半段（稳定采样 + 自救）根本没机会跑，结论只剩 pending
		// 或空（真机实证：install.log 全空、结论 status=""）。
		// 不带 --scope 时 systemd-run 起的是独立 unit，与本进程的生死无关；
		// 真机对照（同一脚本）：--scope ⇒ 结论空/pending；transient service ⇒ ok:true + audit 干净。
		out, err := m.run(detached, "systemd-run", "--collect", "--quiet", "--unit="+unit,
			"/bin/sh", scriptPath)
		return out, err, unit
	case detachSetsid:
		// setsid 把命令放进新 session：无 systemd 的底座上 postinst 的 try-restart 本就不执行
		//（它先判 command -v systemctl），故不存在 cgroup 连坐，脱离 session 已经足够。
		out, err := m.run(detached, "setsid", "/bin/sh", scriptPath)
		return out, err, ""
	}
	// 一个脱离手段都没有：功能仍可用，但回到「被重启波及」的旧风险，必须在输出里写明。
	out, err := m.run(ctx, "/bin/sh", scriptPath)
	return joinLines("未找到可用的 systemd-run/setsid，已直跑（升级可能被 nfvisd 重启波及）", out), err, ""
}

// stopDetachedScript 停掉本次安装的 transient unit（前提：dpkg 已退出——install.rc 已知）。
//
// 修 BUG-18 场景 B 的双写者竞态：脱离脚本晚 ~12s 用自己的终判覆盖进程侧结论。脚本侧
// 判据已补齐后两边的结论**内容**一致了，但仍要停——「最后落笔的人」必须唯一，否则任何
// 一侧的回归都会以覆盖的形式复活。`systemctl stop` 会等 unit 的进程真正终止才返回，
// 返回后脚本不可能再写结论文件；unit 已结束/不存在时是幂等 no-op。
// 只在 systemd-run 形态下可停（setsid 无 unit 名可寻）。
func (m *SoftwareManager) stopDetachedScript(at installAttempt) {
	if at.unit == "" || m.detacher(context.Background()) != detachSystemdRun {
		return
	}
	if _, known := m.readDpkgRC(at); !known {
		// dpkg 还没退出（或 rc 没落下来）：绝不停——杀了正在推进的 dpkg，
		// 制造出来的正是 BUG-17 那种 half-configured。
		return
	}
	_, _ = m.runBounded(30*time.Second, "systemctl", "stop", at.unit)
}

// writeLaunchScript 把待执行脚本落成文件（0600，软件目录下），返回其路径。
//
// 落盘而非 `sh -c`：见 launchShell 的注释（systemd-run 会把脚本里的 `${x%%|*}`
// 误判成环境变量赋值并吞掉）。文件按时间戳区分，便于排障时对照 install.log。
func (m *SoftwareManager) writeLaunchScript(script string) (string, error) {
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return "", err
	}
	now := time.Now
	if m.now != nil {
		now = m.now
	}
	p := filepath.Join(m.dir, fmt.Sprintf("run-%d.sh", now().UnixNano()))
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o600); err != nil {
		return "", err
	}
	return p, nil
}

// launchScriptForTest 拼「脚本 + 重定向」的最终命令（单测用它钉住 % 不被格式化）。
//
// 修 R44-1 的重定向一半：重定向必须罩住**整段**（`{ …; } >>log 2>&1`），不能只追加在末尾——
// 那只会罩住最后一个分组，`dpkg -i` 的输出根本进不了 install.log（真机实证 v3.0.28：
// 坏包场景 install.log 恒 0 字节，操作者永远看不到 dpkg 拒绝坏包的那行报错）。
func (m *SoftwareManager) launchScriptForTest(command, logPath string) string {
	return "{ " + command + "; } >>" + shellQuote(logPath) + " 2>&1"
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
		if _, err := m.run(ctx, "systemd-run", "--quiet", "/bin/true"); err == nil {
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

// debVersionAllowed 合法的 Debian 版本字符集（与 dpkg 的 version 语法一致：数字、字母、
// `.`、`+`、`~`、`:`、`-`）。
//
// 用白名单而非黑名单（修 R51-L1）：该值会经 `filepath.Join` 拼成归档路径、并被插进以 root
// 执行的 shell 脚本，任何 `/`、引号、反引号、`$`、空白或换行都必须先被拒之门外——黑名单永远
// 漏（例如漏掉 `\`、换行、制表符、`;`）。
var debVersionAllowed = regexp.MustCompile(`^[A-Za-z0-9.+~:-]+$`)

// validateDebVersion 校验从包内读到的 Version 是否可用于拼路径与脚本（修 R51-L1）。
func validateDebVersion(v string) error {
	if v == "" {
		return errors.New("版本为空")
	}
	// `..` 不在白名单之外（`.` 是合法字符），但它出现在路径里就是越界意图，显式拒掉。
	// 真实版本号（3.0.32~r50 等）不含连续的 `..`。
	if strings.Contains(v, "..") || !debVersionAllowed.MatchString(v) {
		return fmt.Errorf("版本号含非法字符: %q", v)
	}
	return nil
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
