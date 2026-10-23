package system

// 修 BUG-16 的服务端一半：升级/回退结束时把**结论**落盘，供 CLI 重连后读取。
//
// 为什么必须有这一半：`software add` 会重启 nfvisd，承载该命令的 HTTP 连接随之 EOF。
// 结论若不落盘，CLI 重连后无处可问——只能猜「可能成功了」，那正是报告指出的误操作诱因。

import (
	"encoding/json"
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
