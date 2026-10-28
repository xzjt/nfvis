package system

// round51 R51-L1：包里声明的 Version 被无条件信任，而它流向两处 root 权限的落点——
// 归档路径（`filepath.Join(m.dir, "nfvis_<ver>_amd64.deb")`，`..` 段折叠后真的写出去）与
// 脱离执行的 root shell 脚本里的 reason 字符串（引号/反引号/`$()` 会破开那条被 BUG-16/17/18
// 加固过的脚本）。
//
// 本文件钉住：① 非法版本在**受理前**（任何文件操作之前）被拒并点名；② 目标版本进脚本一律经
// shellQuote（值经变量传入，不再裸插值）。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ① 非法版本的安装请求在受理前被拒：不得归档、不得 dpkg -i、不得写出软件目录之外。
func TestFixR51SoftwareRejectsMalformedPackageVersion(t *testing.T) {
	// 软件目录**不放在临时根**上：`..` 越界时会落在它的父目录（由 t.TempDir 一并回收），
	// 「有没有写出去」因此可断言且不污染宿主。
	root := t.TempDir()
	dir := filepath.Join(root, "software")
	src := writeDeb(t, t.TempDir(), "nfvis_evil_amd64.deb")

	for _, bad := range []string{
		"../../../r51-escaped",                // 越界：`nfvis_` 前缀吃掉第一个 `..`，第三个才真出去
		"../../../../tmp/r51-pwned",           // 报告里的形态（更深的越界）
		`3.0.26"; touch /tmp/r51-pwn; echo "`, // 破开脚本的引号
		"3.0.26`id`",                          // 反引号执行
		"3.0.26 $(id)",                        // 命令替换
		"3.0.26 x",                            // 空白
		"..",                                  // 纯上级段
	} {
		fr := &fakeRunner{replies: map[string]string{" Package": "nfvis\n", "Version": bad + "\n"}}
		m := NewSoftwareManager(dir, fr.run, func() string { return "3.0.25" })
		res, err := m.Add(context.Background(), src, "")
		if err == nil {
			t.Fatalf("非法版本 %q 应被拒，实际成功: %+v", bad, res)
		}
		if !strings.Contains(err.Error(), "Version") {
			t.Fatalf("拒绝文案要点名 Version 字段，实际: %v", err)
		}
		joined := strings.Join(fr.calls, "\n")
		if strings.Contains(joined, "dpkg -i") {
			t.Fatalf("非法版本不得走到 dpkg -i: %v", fr.calls)
		}
		// 归档路径按产品同一算法算（filepath.Join 折叠 `..`）：这条同时覆盖「落在软件目录内」
		// 与「越界到软件目录之外」两种形态。
		archived := filepath.Join(dir, "nfvis_"+bad+"_amd64.deb")
		if _, serr := os.Stat(archived); serr == nil {
			t.Fatalf("非法版本 %q 不得落盘归档 %s（这就是越界写入）", bad, archived)
		}
		// 前提自证（越界是真的，不是空断言）：`nfvis_` 前缀会把**第一个** `..` 吃成普通名字
		// `nfvis_..`，故两个 `..` 只抵平一层、三个才真从软件目录折出去（正落在 root 里）。
		if bad == "../../../r51-escaped" && filepath.Dir(archived) != root {
			t.Fatalf("用例前提失效：%q 折叠后应落在 %s，实际 %s", bad, root, filepath.Dir(archived))
		}
		entries, rerr := os.ReadDir(root)
		if rerr != nil {
			t.Fatal(rerr)
		}
		for _, e := range entries {
			if e.Name() != "software" {
				t.Fatalf("非法版本（%q）不得在软件目录之外留下文件: %s", bad, e.Name())
			}
		}
	}
	// 正控：正常版本照旧归档（不得把好包一起挡掉）。
	fr := &fakeRunner{replies: map[string]string{" Package": "nfvis\n", "Version": "3.0.32~r50\n",
		"dpkg-query": "3.0.25\n"}, dir: dir}
	m := NewSoftwareManager(dir, fr.run, func() string { return "3.0.25" })
	if _, err := m.Add(context.Background(), src, ""); err != nil {
		t.Fatalf("正常版本不应被拒: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "nfvis_3.0.32~r50_amd64.deb")); err != nil {
		t.Fatalf("正常版本应照旧归档: %v", err)
	}
}

// ② 目标版本进脚本只能出现**单引号包裹**的形态；reason 里改经 `$tv` 取（不再裸插值）。
// 一个含 `"` 的版本在修复前会把 reason 的字符串提前闭合、把后面的内容当语句执行。
func TestFixR51SoftwareScriptQuotesTargetVersion(t *testing.T) {
	m := NewSoftwareManager(t.TempDir(), (&fakeRunner{}).run, func() string { return "3.0.25" })
	evil := `3.0.26"; touch /tmp/nfvis-r51-pwn; echo "`
	s := m.installScriptWithResult(installJob{
		action: "安装", act: "add", staging: "/tmp/x.deb", canonical: "/tmp/y.deb",
		targetVer: evil, attempt: "1"})

	// reason 里不得裸插值版本号（修复前的形态：`未达到目标 3.0.26"; touch …`）。
	if strings.Contains(s, "未达到目标 "+evil) {
		t.Fatalf("reason 里不得裸插值目标版本:\n%s", s)
	}
	if !strings.Contains(s, "未达到目标 $tv") {
		t.Fatalf("reason 应经 shell 变量 $tv 取目标版本:\n%s", s)
	}
	// 值只能以 shellQuote 形态出现（单引号整段包裹 ⇒ 里面的引号/分号都是数据）。
	if !strings.Contains(s, "tv="+shellQuote(evil)) {
		t.Fatalf("目标版本必须以 shellQuote 形态赋给 tv:\n%s", s)
	}
	if strings.Contains(s, `tv="`+evil) {
		t.Fatalf("目标版本不得进双引号字符串:\n%s", s)
	}

	// 脚本仍可解析（有 sh 的机器上做一次语法检查：Windows 上装了 Git Bash 也会命中）。
	if sh, err := exec.LookPath("sh"); err == nil {
		cmd := exec.Command(sh, "-n")
		cmd.Stdin = strings.NewReader(s)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("脚本应仍是合法 shell（sh -n）：%v\n%s\n---脚本---\n%s", err, out, s)
		}
	}
}
