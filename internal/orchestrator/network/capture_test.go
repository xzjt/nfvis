package network

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePcapShell 记录 VPP CLI 命令并模拟 pcap 输出文件。
type fakePcapShell struct {
	calls   []string
	outPath string // 模拟 VPP 固定输出文件
	written int    // "Write N packets" 的 N
	err     error
}

func (f *fakePcapShell) Run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if f.err != nil {
		return "", f.err
	}
	if len(args) > 0 && args[len(args)-1] == "off" {
		if f.written > 0 {
			_ = os.WriteFile(f.outPath, make([]byte, 64), 0o644)
			return "Write " + itoa(f.written) + " packets to " + f.outPath + ", and stop capture...", nil
		}
		return "No packets captured...", nil
	}
	return "", nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func newCaptureFixture(t *testing.T) (*CaptureProvider, *fakePcapShell, string, string) {
	t.Helper()
	exportDir := t.TempDir()
	vppDir := t.TempDir()
	out := filepath.Join(vppDir, "rxtx.pcap")
	sh := &fakePcapShell{outPath: out, written: 6}
	p := NewCaptureProvider(sh, func(ifname string) (uint32, bool, error) {
		if ifname == "ens192" {
			return 1, true, nil
		}
		return 0, false, nil
	}, exportDir)
	p.SetVPPOutputPath(out)
	return p, sh, exportDir, out
}

func TestCaptureStartStopExport(t *testing.T) {
	p, sh, exportDir, _ := newCaptureFixture(t)
	ctx := context.Background()

	// 接口不存在（此时无活动会话，先验接口解析）
	if err := p.Start(ctx, "ens999", 10, ""); err == nil || !strings.Contains(err.Error(), "抓包接口") {
		t.Fatalf("接口不存在应报错: %v", err)
	}
	if err := p.Start(ctx, "ens192", 50, ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(sh.calls) != 1 || sh.calls[0] != "pcap trace rx tx intfc ens192 max 50" {
		t.Fatalf("开始命令: %v", sh.calls)
	}
	// 重复开始 → 409 语义
	if err := p.Start(ctx, "ens192", 10, ""); err == nil || !strings.Contains(err.Error(), "已有抓包会话") {
		t.Fatalf("重复开始应报活动会话: %v", err)
	}
	// ACL 过滤不支持
	if err := p.Start(ctx, "ens224", 10, "acl-x"); err == nil || !strings.Contains(err.Error(), "不支持 ACL 过滤") {
		t.Fatalf("filter_acl 应明确不支持: %v", err)
	}

	active, files := p.Status()
	if active == nil || active.Interface != "ens192" || len(files) != 0 {
		t.Fatalf("Status: %+v %v", active, files)
	}

	f, err := p.Stop(ctx, true)
	if err != nil {
		t.Fatalf("Stop(export): %v", err)
	}
	if f.Name == "" || !strings.HasSuffix(f.Name, ".pcap") || f.SizeBytes == 0 {
		t.Fatalf("导出文件: %+v", f)
	}
	if _, err := os.Stat(filepath.Join(exportDir, f.Name)); err != nil {
		t.Fatalf("导出文件应存在: %v", err)
	}
	// 会话已结束
	if active, _ := p.Status(); active != nil {
		t.Fatalf("停止后不应有活动会话: %+v", active)
	}
	if _, err := p.Stop(ctx, true); err == nil {
		t.Fatal("无会话时 stop 应报错")
	}
	// 下载路径与穿越拒绝
	if _, err := p.Path(f.Name); err != nil {
		t.Fatalf("Path: %v", err)
	}
	if _, err := p.Path("../etc/passwd"); err == nil {
		t.Fatal("穿越应拒绝")
	}
	if _, err := p.Path("x.txt"); err == nil {
		t.Fatal("非 .pcap 应拒绝")
	}
}

func TestCaptureStopWithoutExportDiscards(t *testing.T) {
	p, _, exportDir, out := newCaptureFixture(t)
	ctx := context.Background()
	if err := p.Start(ctx, "ens192", 10, ""); err != nil {
		t.Fatal(err)
	}
	f, err := p.Stop(ctx, false)
	if err != nil || f.Name != "" {
		t.Fatalf("不导出应返回空文件: %+v %v", f, err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("不导出应删除 VPP 输出文件: %v", err)
	}
	if got := p.List(); len(got) != 0 {
		t.Fatalf("导出目录应为空: %v", got)
	}
	_ = exportDir
}

func TestCaptureNoPackets(t *testing.T) {
	p, sh, _, _ := newCaptureFixture(t)
	sh.written = 0
	ctx := context.Background()
	if err := p.Start(ctx, "ens192", 10, ""); err != nil {
		t.Fatal(err)
	}
	f, err := p.Stop(ctx, true)
	if err != nil || f.Name != "" {
		t.Fatalf("无包时导出应为空且不报错: %+v %v", f, err)
	}
}
