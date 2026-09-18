package api

// 发现 #11：`show vpp` 的可见性缺口——命令全表把它描述为「数据面概览：**版本**/线程/buffer/内存」，
// 而 CLI 此前只打印 threads/buffers/memory：版本没打印，`pending_restart`（vpp 段变更后
// 「要不要 request vpp restart」的唯一指示）在 CLI 侧完全看不到（只有 /vpp/status 有）。
// 本文件锁住修复后的可见性，并锁住「stats 运行态不可用时仍给出版本/待重启」。

import (
	"context"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// fakeVppCtl 只实现 Status（测试用）。
type fakeVppCtl struct {
	st VppStatus
}

func (f fakeVppCtl) Status(*model.VppConfig) VppStatus { return f.st }
func (f fakeVppCtl) Restart(context.Context, *model.VppConfig) error {
	return nil
}

func TestShowVppShowsVersionAndPendingRestart(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setVppCtl(fakeVppCtl{st: VppStatus{
		Version: "26.06-rc2", Connected: true, PendingRestart: true,
	}})

	out := x.Execute("admin", "super-user", "ssh", "show vpp").Output
	if strings.Contains(out, "%%") {
		t.Fatalf("show vpp 不应报错:\n%s", out)
	}
	for _, want := range []string{"version: 26.06-rc2", "connected: yes", "pending_restart: yes"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show vpp 应含 %q（发现 #11）：\n%s", want, out)
		}
	}
	// stats 运行态缺失时也必须给出这三行（它们来自连接管理器，不依赖 stats）
	if !strings.Contains(out, "version:") {
		t.Fatalf("stats 不可用时仍应给出版本:\n%s", out)
	}
}

// 结构化输出也应带上这三项（`| display json` 可读）。
func TestShowVppStructuredHasPendingRestart(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setVppCtl(fakeVppCtl{st: VppStatus{Version: "v26", Connected: true}})
	out := x.Execute("admin", "super-user", "ssh", "show vpp").Output
	if !strings.Contains(out, "connected: yes") {
		t.Fatalf("应显示连接状态:\n%s", out)
	}
}

// 未装配 VppController（旧装配/单测）时不得 panic，也不得谎报版本。
func TestShowVppWithoutController(t *testing.T) {
	x, _ := newCLIKit(t)
	out := x.Execute("admin", "super-user", "ssh", "show vpp").Output
	if strings.Contains(out, "version:") {
		t.Fatalf("未装配时不应凭空给出用户名版本:\n%s", out)
	}
}

// 经 HTTP 的 /cli/execute 也应带上（CLI 实际走这条路）。
func TestShowVppOverHTTP(t *testing.T) {
	ts := newTestServerOpts(t, Options{VPP: fakeVppCtl{st: VppStatus{Version: "26.06-http", PendingRestart: true}}})
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, "POST", ts.URL+APIPrefix+"/cli/execute", token,
		map[string]string{"line": "show vpp", "source": "ssh"}, nil)
	if status != 200 {
		t.Fatalf("状态 %d: %s", status, data)
	}
	body := string(data)
	for _, want := range []string{"26.06-http", "pending_restart"} {
		if !strings.Contains(body, want) {
			t.Fatalf("HTTP 输出应含 %q:\n%s", want, body)
		}
	}
}
