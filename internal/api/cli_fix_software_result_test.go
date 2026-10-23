package api

// 修 BUG-16（v3.0.25 复测报告）：`request system software add` 命令侧拿不到任何结论。
//
// 背景：升级会重启 nfvisd（postinst 的 try-restart），承载该命令的连接随之断开。服务端
// 侧已把包落定为 `install ok installed`（决策 #496），但操作者只看到
// `%% 连接 nfvisd 失败: ... EOF` + 非零退出码 —— 会误判失败而重试或走回退，而升级其实已成功。
//
// 修法分两半：① 服务端在安装结束时把结论落盘（`/var/lib/nfvis/software/last-result.json`）；
// ② 新增读口 `show system software result`，CLI 重连后读它拿真实结果。
// 本用例钉住读口这一半（落盘那一半在 internal/system 的用例里钉）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
)

// 有结论记录：如实渲染成功/失败、版本与状态行。
func TestCLIShowSoftwareResultRendersRecord(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "last-result.json"), []byte(`{
  "action": "add", "version": "3.0.25", "previous": "3.0.24",
  "package": "nfvis_3.0.25_amd64.deb", "status": "install ok installed", "ok": true
}
`), 0o644); err != nil {
		t.Fatalf("写夹具: %v", err)
	}
	out := renderSoftwareResultFrom(t, filepath.Join(dir, "last-result.json"))
	for _, want := range []string{"结果: 成功", "版本: 3.0.25", "状态: install ok installed", "动作: 升级"} {
		if !strings.Contains(out, want) {
			t.Fatalf("渲染应含 %q，实际:\n%s", want, out)
		}
	}
}

// 失败记录：结论必须说「失败」并给出回退路径，不得含糊。
func TestCLIShowSoftwareResultRendersFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "last-result.json"), []byte(`{
  "action": "add", "version": "3.0.25", "status": "安装失败: 包未落定", "ok": false
}
`), 0o644); err != nil {
		t.Fatalf("写夹具: %v", err)
	}
	out := renderSoftwareResultFrom(t, filepath.Join(dir, "last-result.json"))
	if !strings.Contains(out, "结果: 失败") {
		t.Fatalf("失败记录必须如实报失败，实际:\n%s", out)
	}
	if !strings.Contains(out, "安装失败") {
		t.Fatalf("应带服务端记录的状态文本，实际:\n%s", out)
	}
}

// 无记录：如实说「无记录」，不编造「上次成功」。
func TestCLIShowSoftwareResultNoRecord(t *testing.T) {
	x, _ := newCLIKit(t)
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system software result").Output
	if strings.Contains(out, "%%") {
		t.Fatalf("无记录不是错误，实际:\n%s", out)
	}
	if !strings.Contains(out, "无升级/回退结论记录") {
		t.Fatalf("无记录时应如实说明，实际:\n%s", out)
	}
}

// renderSoftwareResultFrom 用给定路径渲染（生产读口固定读 LastResultPath；
// 渲染逻辑的路径参数化版本供单测覆盖三种形态）。
func renderSoftwareResultFrom(t *testing.T, path string) string {
	t.Helper()
	x, _ := newCLIKit(t)
	return x.renderSoftwareResultPath(path)
}
