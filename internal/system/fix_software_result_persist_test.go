package system

// 修 BUG-16 的服务端一半：升级/回退结束时把**结论**落盘，供 CLI 重连后读取。
//
// 为什么必须有这一半：`software add` 会重启 nfvisd，承载该命令的 HTTP 连接随之 EOF。
// 结论若不落盘，CLI 重连后无处可问——只能猜「可能成功了」，那正是报告指出的误操作诱因。

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 写入后能读回同一份结论（成功态）。
func TestRecordAndReadLastResultSuccess(t *testing.T) {
	dir := t.TempDir()
	m := &SoftwareManager{dir: dir}
	m.RecordResult(SoftwareResult{
		Action: "add", Version: "3.0.25", Previous: "3.0.24",
		Package: "nfvis_3.0.25_amd64.deb", Status: pkgStatusInstalled, OK: true,
	})

	got, ok := ReadLastResultAt(filepath.Join(dir, "last-result.json"))
	if !ok {
		t.Fatal("刚写入的结论应可读回")
	}
	if !got.OK || got.Version != "3.0.25" || got.Status != pkgStatusInstalled {
		t.Fatalf("读回的结论不符: %+v", got)
	}
}

// 失败态也要落盘：操作者重连后要能看到「失败 + 原因」，而不是只看到 EOF。
func TestRecordLastResultFailureIsReadable(t *testing.T) {
	dir := t.TempDir()
	m := &SoftwareManager{dir: dir}
	m.RecordResult(SoftwareResult{Action: "add", Version: "3.0.25", Status: "安装失败: 包未落定", OK: false})

	got, ok := ReadLastResultAt(filepath.Join(dir, "last-result.json"))
	if !ok {
		t.Fatal("失败结论同样应可读回（否则重连后只能猜）")
	}
	if got.OK {
		t.Fatalf("失败态不得读成成功: %+v", got)
	}
	if got.Status == "" {
		t.Fatal("失败态应带原因文本")
	}
}

// 文件不存在 / 内容损坏：如实返回 ok=false（不编造「上次成功」）。
func TestReadLastResultMissingOrCorrupt(t *testing.T) {
	dir := t.TempDir()
	if _, ok := ReadLastResultAt(filepath.Join(dir, "nope.json")); ok {
		t.Fatal("文件不存在应返回 ok=false")
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadLastResultAt(bad); ok {
		t.Fatal("损坏内容应返回 ok=false")
	}
}

// 落盘格式稳定（字段名是契约的一部分：CLI 侧按这些键读）。
func TestLastResultJSONShape(t *testing.T) {
	dir := t.TempDir()
	m := &SoftwareManager{dir: dir}
	m.RecordResult(SoftwareResult{Action: "rollback", Version: "3.0.24", Status: pkgStatusInstalled, OK: true})

	raw, err := os.ReadFile(filepath.Join(dir, "last-result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m2 map[string]any
	if err := json.Unmarshal(raw, &m2); err != nil {
		t.Fatalf("落盘内容应是合法 JSON: %v", err)
	}
	for _, k := range []string{"action", "version", "status", "ok"} {
		if _, ok := m2[k]; !ok {
			t.Fatalf("落盘 JSON 缺字段 %q（CLI 侧按它读）: %s", k, raw)
		}
	}
}

// 修 BUG-16 的关键一处：结论必须由**脱离执行的那个 shell 自己**在 dpkg 结束后写出。
//
// 为什么必须钉住：postinst 会重启 nfvisd，本进程在那时已被杀——`Add` 里回到本进程再
// RecordResult 根本执行不到（真机实证：升级成功、结论文件却不存在，CLI 重连后读不到）。
func TestInstallScriptWritesResultInsideDetachedShell(t *testing.T) {
	m := &SoftwareManager{dir: "/var/lib/nfvis/software"}
	script := m.installScriptWithResult("/tmp/nfvis_3.0.26_amd64.deb", "安装")

	for _, want := range []string{
		"dpkg -i",
		"/var/lib/dpkg/status", // 状态读纯文本（不拿 dpkg 锁，postinst 重启期间也读得到）
		"last-result.json",     // 结论落点
		"act",                  // 升级动作（awk 变量）写进结论
		pkgStatusInstalled,     // 成功态判据与 installSettled 同源
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("安装脚本应含 %q（否则 nfvisd 被杀后结论丢失）：\n%s", want, script)
		}
	}
	// 回退动作要写成 rollback（否则 CLI 侧把回退报成升级）。
	if rb := m.installScriptWithResult("/tmp/x.deb", "回退"); !strings.Contains(rb, "'rollback'") {
		t.Fatalf("回退应写 action=rollback：\n%s", rb)
	}
}

// 修 round44：拼进 shell 脚本的路径必须是 POSIX 分隔符。
//
// 为什么钉：这段文本由 Linux 的 /bin/sh 执行，而 `filepath.Join` 在 Windows 宿主上产出反斜杠，
// shell 会把 `\v`/`\l` 当转义 ⇒ 路径失效、结论写不出（真机实证：升级装上了、结论文件恒为 pending）。
// 单测跑在 Windows 上正好能抓到它——但前提是有这条断言。
func TestInstallScriptUsesPosixPaths(t *testing.T) {
	m := &SoftwareManager{dir: "/var/lib/nfvis/software"}
	s := m.installScriptWithResult("/tmp/x.deb", "安装")
	// 只查**路径段**：脚本里的 `\n`（printf 的换行转义）是合法的，不能一刀切禁反斜杠。
	if strings.Contains(s, `\var\`) || strings.Contains(s, `\root\`) || strings.Contains(s, `\lib\`) {
		t.Fatalf("路径段出现 Windows 分隔符（会破坏 shell 语义）：\n%s", s)
	}
	if !strings.Contains(s, "/var/lib/nfvis/software/last-result.json") {
		t.Fatalf("结论落点应是 POSIX 绝对路径：\n%s", s)
	}
}

// 修 round44（第二处 shell 破坏）：`launchShell` 不得用 fmt.Sprintf 拼脚本。
//
// 为什么：交给 /bin/sh 的是**脚本**，里面天然含 `%`——`${pair%%|*}` 的 `%%` 会被 Sprintf
// 折成 `%`、`printf '…%s…'` 的 `%s` 会被当格式动词吞掉。真机实证：`st=${pair%%|*}` 变成
// `st=`，采样恒空 ⇒ 结论永远停在 pending（升级装上了却报「未定」）。
// 这条只能静态钉：测试跑不到 Sprintf 的格式化阶段。
func TestLaunchShellDoesNotFormatScript(t *testing.T) {
	src, err := os.ReadFile("software.go")
	if err != nil {
		t.Fatalf("读 software.go: %v", err)
	}
	if strings.Contains(string(src), `fmt.Sprintf("%s >>%s 2>&1"`) {
		t.Fatal("launchShell 不能用 Sprintf 拼脚本（% 会被格式化改写，脚本语义被静默破坏）")
	}
}

// 生成脚本里的 `%%`/`%s` 必须原样保留到最终交给 shell 的文本里。
func TestLaunchShellPreservesShellPercent(t *testing.T) {
	// 直接验证拼接语义：含 %% 与 %s 的脚本经 launchShell 的拼装后一字不变。
	m := &SoftwareManager{dir: t.TempDir()}
	got := m.launchScriptForTest("echo ${pair%%|*} && printf '%s' x", "/tmp/log")
	if !strings.Contains(got, `${pair%%|*}`) || !strings.Contains(got, `printf '%s'`) {
		t.Fatalf("shell 的 %%/%%s 必须原样保留：\n%s", got)
	}
}

// 修 round44（第三处，决定性）：脱离执行必须走**脚本文件**，不能 `sh -c "<script>"`。
//
// 根因：systemd-run 会扫描参数里的 `NAME=value` 形态当环境变量赋值，而脚本里满是
// `${pair%%|*}` / `${pair#*|}` ⇒ 它报
//
//	Invalid environment variable name evaluates to an empty string: pair%%|*
//
// 并把那一段吞掉，shell 看到 `st=`（空）⇒ 采样恒空、结论永远停 pending。
// 真机对照：`sh -c "…${pair%%|*}…"` 报错；`sh /tmp/t.sh` 正常。
func TestLaunchShellUsesScriptFileNotDashC(t *testing.T) {
	src, err := os.ReadFile("software.go")
	if err != nil {
		t.Fatalf("读 software.go: %v", err)
	}
	text := string(src)
	idx := strings.Index(text, "func (m *SoftwareManager) launchShell(")
	body := text[idx:]
	if end := strings.Index(body[1:], "\nfunc "); end > 0 {
		body = body[:end+1]
	}
	if strings.Contains(body, `"-c"`) || strings.Contains(body, `"-c", script`) {
		t.Fatalf("launchShell 不得用 sh -c 传脚本（systemd-run 会把 ${x%%%%|*} 当赋值吞掉）：\n%s", body)
	}
	if !strings.Contains(body, "writeLaunchScript") {
		t.Fatalf("launchShell 应把脚本落成文件再执行：\n%s", body)
	}
}

// 落盘脚本带 shebang 且内容原样（含 %% 与 %s）。
func TestWriteLaunchScriptKeepsContent(t *testing.T) {
	m := &SoftwareManager{dir: t.TempDir()}
	p, err := m.writeLaunchScript("echo ${pair%%|*} && printf '%s' x")
	if err != nil {
		t.Fatalf("写脚本: %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读脚本: %v", err)
	}
	s := string(b)
	if !strings.HasPrefix(s, "#!/bin/sh\n") {
		t.Fatalf("应有 shebang：%q", s)
	}
	if !strings.Contains(s, "${pair%%|*}") || !strings.Contains(s, "printf '%s'") {
		t.Fatalf("脚本内容必须原样（%%/%%s 不得被改写）：\n%s", s)
	}
}

// 修 round44（第四处，也是 BUG-17 的真正根因）：判据必须**同时**看 dpkg --audit。
//
// 真机实证（本轮最关键）：dpkg 1.23 在配置被打断时，`/var/lib/dpkg/status` 的 Status 字段
// 可能**仍写 `install ok installed`**，而 dpkg.log 与内部记账是 `half-configured`——
// 实测该窗口持续 **16 秒以上**（12:05:52–12:06:08 逐秒采样：status=installed 而 audit=1）。
// 只按状态行判定必然在这个窗口里判成功，这正是 BUG-17 的全部来源。
func TestInstallSettledRequiresCleanAudit(t *testing.T) {
	// 状态行说 installed，但 audit 脏 ⇒ 不算落定。
	if installSettled(pkgStatusInstalled, nil, "3.0.28", nil, false, nil, "3.0.28") {
		t.Fatal("audit 脏时不得判落定（状态行会谎报 installed）")
	}
	// audit 读不到 ⇒ 拿不到证据 ⇒ 不算落定（宁可「无法判定」）。
	if installSettled(pkgStatusInstalled, nil, "3.0.28", nil, true, errors.New("dpkg --audit 失败"), "3.0.28") {
		t.Fatal("audit 读不到时不得判落定")
	}
	// 两者都干净才落定（回归）。
	if !installSettled(pkgStatusInstalled, nil, "3.0.28", nil, true, nil, "3.0.28") {
		t.Fatal("状态行 installed 且 audit 干净应判落定")
	}
}

// 脱离脚本也必须带 audit 判据（它写的是 CLI 最终读到的那份结论）。
func TestInstallScriptChecksAudit(t *testing.T) {
	m := &SoftwareManager{dir: t.TempDir()}
	s := m.installScriptWithResult("/tmp/x.deb", "安装")
	if !strings.Contains(s, "dpkg --audit") {
		t.Fatalf("脚本必须以 dpkg --audit 为判据之一（只看状态行会谎报 installed）：\n%s", s)
	}
}
