package system

// 决策 #424：系统基线（主机名/时区/NTP/宿主解析器）宿主落地的守护。
//
// 覆盖：三项（+解析器）各自的应用命令与文件字面（含 prefer 映射、多服务器顺序）；
// 幂等（值未变不写不重启，只做只读探测）；本机没有 chrony / systemd-resolved 未运行
// 时**如实跳过**（零写入、说明进 Notes）；失败逐项隔离（一项失败不拖累其余，错误如实返回）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// foundPaths 存在性判据：全部「已安装」（真实系统上的常态）。
func foundPaths(string) (string, error) { return "/usr/bin/x", nil }

// missingPaths 存在性判据：全部「未安装」（模拟本机没有 chrony）。
func missingPaths(string) (string, error) { return "", errors.New("not found") }

func newBaselineFixture(t *testing.T) (*SystemBaselineApplier, *fakeRunner, string) {
	t.Helper()
	root := t.TempDir()
	fr := &fakeRunner{replies: map[string]string{}}
	a := NewSystemBaselineApplier(fr.run)
	a.Root = root
	a.LookPath = foundPaths
	return a, fr, root
}

func TestRenderChronySourcesMapping(t *testing.T) {
	got := RenderChronySources([]model.NtpServer{
		{Server: "ntp.ubuntu.com", Prefer: true},
		{Server: "10.0.0.1"},
		{Server: "2001:db8::1", Prefer: true},
		{Server: "  "}, // 空值跳过，不产出占位行
	})
	want := "# 由 NFViS 生成（NTP 服务器声明，按产品配置渲染；请勿手工编辑）\n" +
		"pool ntp.ubuntu.com prefer\nserver 10.0.0.1\nserver 2001:db8::1 prefer\n"
	if got != want {
		t.Fatalf("chrony 源渲染不符：\n得到:\n%s\n期望:\n%s", got, want)
	}
	if RenderChronySources(nil) != "" {
		t.Fatal("空列表应返回空内容")
	}
}

func TestRenderResolvedDropInFiltersInvalid(t *testing.T) {
	got, ignored := RenderResolvedDropIn([]string{"8.8.8.8", "不是IP", "2001:4860:4860::8888"})
	want := "# 由 NFViS 生成（宿主解析器上游，按产品配置渲染；请勿手工编辑）\n" +
		"[Resolve]\nDNS=8.8.8.8 2001:4860:4860::8888\n"
	if got != want {
		t.Fatalf("resolved drop-in 渲染不符：\n得到:\n%s\n期望:\n%s", got, want)
	}
	if !slices.Equal(ignored, []string{"不是IP"}) {
		t.Fatalf("非 IP 取值应如实报告：%v", ignored)
	}
	if s, _ := RenderResolvedDropIn(nil); s != "" {
		t.Fatal("空列表应返回空内容")
	}
}

func TestSystemBaselineAppliesToHost(t *testing.T) {
	a, fr, root := newBaselineFixture(t)
	fr.replies["hostnamectl hostname"] = "nfvis\n"                  // 运行态仍是旧名
	fr.replies["timedatectl show"] = "Etc/UTC\n"                    // 时区仍是缺省
	fr.replies["systemctl is-active systemd-resolved"] = "active\n" // 解析器在运行

	sys := &model.SystemConfig{
		Hostname:   "nfvis-lab",
		Timezone:   "Asia/Shanghai",
		Ntp:        []model.NtpServer{{Server: "ntp.ubuntu.com", Prefer: true}, {Server: "10.0.0.1"}},
		DNSServers: []string{"8.8.8.8", "不是IP", "2001:4860:4860::8888"},
	}
	res, err := a.Apply(context.Background(), sys)
	if err != nil {
		t.Fatalf("首次应用不应报错: %v", err)
	}
	if res.Hostname != BaselineApplied || res.Timezone != BaselineApplied ||
		res.NTP != BaselineApplied || res.DNS != BaselineApplied {
		t.Fatalf("四项都应 applied：%+v", res)
	}
	// 命令序列：探测 → 写主机名；探测 → 写时区；写源 → 热重载；探测 → 写解析器 → 重启。
	wantCalls := []string{
		"hostnamectl hostname",
		"hostnamectl set-hostname nfvis-lab",
		"timedatectl show -p Timezone --value",
		"timedatectl set-timezone Asia/Shanghai",
		"chronyc reload sources",
		"systemctl is-active systemd-resolved",
		"systemctl restart systemd-resolved",
	}
	if !slices.Equal(fr.calls, wantCalls) {
		t.Fatalf("命令序列不符：\n得到: %v\n期望: %v", fr.calls, wantCalls)
	}
	// chrony 源 drop-in：pool/server 映射 + prefer 选项 + 顺序保持。
	chronyFile := filepath.Join(root, "etc", "chrony", "sources.d", "nfvis.sources")
	got, err := os.ReadFile(chronyFile)
	if err != nil {
		t.Fatalf("chrony drop-in 应落盘: %v", err)
	}
	wantChrony := "# 由 NFViS 生成（NTP 服务器声明，按产品配置渲染；请勿手工编辑）\n" +
		"pool ntp.ubuntu.com prefer\nserver 10.0.0.1\n"
	if string(got) != wantChrony {
		t.Fatalf("chrony drop-in 内容不符：\n%s", got)
	}
	// /etc/hostname 与声明值一致（hostnamectl 之外的第二条保证）。
	hostFile, err := os.ReadFile(filepath.Join(root, "etc", "hostname"))
	if err != nil || strings.TrimSpace(string(hostFile)) != "nfvis-lab" {
		t.Fatalf("/etc/hostname 应为 nfvis-lab：%q err=%v", hostFile, err)
	}
	// resolved drop-in：合法 IP 全部写入；非 IP 取值有如实说明（不静默丢弃）。
	dnsFile, err := os.ReadFile(filepath.Join(root, "etc", "systemd", "resolved.conf.d", "nfvis-dns.conf"))
	if err != nil {
		t.Fatalf("resolved drop-in 应落盘: %v", err)
	}
	wantDNS := "# 由 NFViS 生成（宿主解析器上游，按产品配置渲染；请勿手工编辑）\n" +
		"[Resolve]\nDNS=8.8.8.8 2001:4860:4860::8888\n"
	if string(dnsFile) != wantDNS {
		t.Fatalf("resolved drop-in 内容不符：\n%s", dnsFile)
	}
	if !hasNote(res.Notes, "不是IP") {
		t.Fatalf("非 IP 取值应有如实说明：%v", res.Notes)
	}
}

func TestSystemBaselineUnconfiguredMakesNoCalls(t *testing.T) {
	a, fr, root := newBaselineFixture(t)
	res, err := a.Apply(context.Background(), &model.SystemConfig{})
	if err != nil {
		t.Fatalf("未配置不应报错: %v", err)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("未配置不应起任何子进程：%v", fr.calls)
	}
	if res.Hostname != BaselineSkipped || res.Timezone != BaselineSkipped ||
		res.NTP != BaselineSkipped || res.DNS != BaselineSkipped {
		t.Fatalf("未配置四项都应 skipped：%+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "etc")); !os.IsNotExist(err) {
		t.Fatalf("未配置不应写任何文件：%v", err)
	}
}

func TestSystemBaselineIdempotent(t *testing.T) {
	a, fr, _ := newBaselineFixture(t)
	fr.replies["hostnamectl hostname"] = "nfvis\n"
	fr.replies["timedatectl show"] = "Etc/UTC\n"
	fr.replies["systemctl is-active systemd-resolved"] = "active\n"
	sys := &model.SystemConfig{
		Hostname: "nfvis-lab", Timezone: "Asia/Shanghai",
		Ntp:        []model.NtpServer{{Server: "ntp.ubuntu.com", Prefer: true}},
		DNSServers: []string{"8.8.8.8"},
	}
	if _, err := a.Apply(context.Background(), sys); err != nil {
		t.Fatalf("首次应用失败: %v", err)
	}
	// 二次应用：宿主已是声明值（探测读数 + drop-in 内容都不变）。
	fr.replies["hostnamectl hostname"] = "nfvis-lab\n"
	fr.replies["timedatectl show"] = "Asia/Shanghai\n"
	fr.calls = nil
	res, err := a.Apply(context.Background(), sys)
	if err != nil {
		t.Fatalf("二次应用不应报错: %v", err)
	}
	if res.Hostname != BaselineUnchanged || res.Timezone != BaselineUnchanged ||
		res.NTP != BaselineUnchanged || res.DNS != BaselineUnchanged {
		t.Fatalf("值未变应四项 unchanged：%+v", res)
	}
	wantCalls := []string{"hostnamectl hostname", "timedatectl show -p Timezone --value"}
	if !slices.Equal(fr.calls, wantCalls) {
		t.Fatalf("值未变只该有只读探测，不得有写命令：%v", fr.calls)
	}
	for _, c := range fr.calls {
		if strings.Contains(c, "set-") || strings.Contains(c, "restart") || strings.Contains(c, "reload") {
			t.Fatalf("值未变时出现写命令：%s", c)
		}
	}
	if len(res.Actions) != 0 {
		t.Fatalf("值未变不应有动作记录：%v", res.Actions)
	}
}

func TestSystemBaselineSkipsChronyWhenMissing(t *testing.T) {
	a, fr, root := newBaselineFixture(t)
	a.LookPath = missingPaths // 本机没有 chronyc
	res, err := a.Apply(context.Background(), &model.SystemConfig{
		Ntp: []model.NtpServer{{Server: "ntp.ubuntu.com"}},
	})
	if err != nil {
		t.Fatalf("未装 chrony 是如实跳过，不该报失败: %v", err)
	}
	if res.NTP != BaselineSkipped {
		t.Fatalf("应 skipped：%+v", res)
	}
	if !hasNote(res.Notes, "chrony") {
		t.Fatalf("跳过说明应点名 chrony：%v", res.Notes)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("未装 chrony 不得起任何 chrony/systemctl 子进程：%v", fr.calls)
	}
	if _, err := os.Stat(filepath.Join(root, "etc", "chrony")); !os.IsNotExist(err) {
		t.Fatalf("未装 chrony 不得写 drop-in：%v", err)
	}
}

func TestSystemBaselineSkipsResolvedWhenNotActive(t *testing.T) {
	a, fr, root := newBaselineFixture(t)
	fr.replies["systemctl is-active systemd-resolved"] = "inactive\n"
	res, err := a.Apply(context.Background(), &model.SystemConfig{DNSServers: []string{"8.8.8.8"}})
	if err != nil {
		t.Fatalf("解析器未运行是如实跳过，不该报失败: %v", err)
	}
	if res.DNS != BaselineSkipped {
		t.Fatalf("应 skipped：%+v", res)
	}
	if !hasNote(res.Notes, "systemd-resolved") {
		t.Fatalf("跳过说明应点名 systemd-resolved：%v", res.Notes)
	}
	if !slices.Equal(fr.calls, []string{"systemctl is-active systemd-resolved"}) {
		t.Fatalf("除探测外不得有动作：%v", fr.calls)
	}
	if _, err := os.Stat(filepath.Join(root, "etc")); !os.IsNotExist(err) {
		t.Fatalf("解析器未运行不得写 drop-in：%v", err)
	}
}

func TestSystemBaselineFailureIsolatedPerItem(t *testing.T) {
	a, fr, _ := newBaselineFixture(t)
	fr.failOn = "hostnamectl set-hostname"
	fr.replies["hostnamectl hostname"] = "nfvis\n"
	res, err := a.Apply(context.Background(), &model.SystemConfig{
		Hostname: "nfvis-lab",
		Timezone: "Asia/Shanghai",
	})
	if err == nil || !strings.Contains(err.Error(), "主机名") {
		t.Fatalf("主机名失败应如实报错：%v", err)
	}
	if res.Hostname != BaselineFailed {
		t.Fatalf("主机名应 failed：%+v", res)
	}
	if res.Timezone != BaselineApplied {
		t.Fatalf("一项失败不得拖累其余项：%+v", res)
	}
	if !slices.Contains(fr.calls, "timedatectl set-timezone Asia/Shanghai") {
		t.Fatalf("时区仍应被应用：%v", fr.calls)
	}
	// 时区值未变时同样不报错、不写。
	fr.calls = nil
	fr.replies["timedatectl show"] = "Asia/Shanghai\n"
	res2, err2 := a.Apply(context.Background(), &model.SystemConfig{Timezone: "Asia/Shanghai"})
	if err2 != nil || res2.Timezone != BaselineUnchanged {
		t.Fatalf("时区未变应 unchanged：%+v err=%v", res2, err2)
	}
	if !slices.Equal(fr.calls, []string{"timedatectl show -p Timezone --value"}) {
		t.Fatalf("时区未变只该探测：%v", fr.calls)
	}
}

func TestSystemBaselineRevertsNTPDropInWhenConfigCleared(t *testing.T) {
	a, fr, root := newBaselineFixture(t)
	if _, err := a.Apply(context.Background(), &model.SystemConfig{
		Ntp: []model.NtpServer{{Server: "ntp.ubuntu.com"}},
	}); err != nil {
		t.Fatalf("首次应用失败: %v", err)
	}
	p := filepath.Join(root, "etc", "chrony", "sources.d", "nfvis.sources")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("drop-in 应已落盘: %v", err)
	}
	fr.calls = nil
	// 删掉 NTP 声明：回收本产品的 drop-in 并热重载（回到宿主原有来源）。
	res, err := a.Apply(context.Background(), &model.SystemConfig{})
	if err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	if res.NTP != BaselineApplied {
		t.Fatalf("回收应报 applied：%+v", res)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("drop-in 应已移除：%v", err)
	}
	if !slices.Contains(fr.calls, "chronyc reload sources") {
		t.Fatalf("回收后应热重载 chrony：%v", fr.calls)
	}
	// 再跑一次（无残留）：不动作、不报错。
	fr.calls = nil
	res2, err2 := a.Apply(context.Background(), &model.SystemConfig{})
	if err2 != nil || res2.NTP != BaselineSkipped || len(fr.calls) != 0 {
		t.Fatalf("无残留时应安静跳过：%+v err=%v calls=%v", res2, err2, fr.calls)
	}
}

// hasNote 说明清单里是否含某子串（Notes 是给人读的整句）。
func hasNote(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}
