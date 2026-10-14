package compute

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// ---------- 探测纯函数 ----------

// TestConsolePortInUse 直测探测语义：空闲 → false；被占用 → true。
func TestConsolePortInUse(t *testing.T) {
	// 空闲：取一个派生端口，确认无人占用时可绑定。
	freePort := model.ConsoleTelnetPort("free-probe-vm")
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort)))
	if err != nil {
		t.Skipf("测试前置：端口 %d 已被占用，跳过空闲分支: %v", freePort, err)
	}
	_ = ln.Close()
	if inUse, err := consolePortInUse(freePort); err != nil || inUse {
		t.Fatalf("空闲端口应判可绑定（false,nil），实得 (%v,%v)", inUse, err)
	}

	// 占用：持有一个监听，探测应判占用。
	occPort := model.ConsoleTelnetPort("occupied-probe-vm")
	hold, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(occPort)))
	if err != nil {
		t.Fatalf("测试前置：占用端口 %d 失败: %v", occPort, err)
	}
	defer hold.Close()
	if inUse, err := consolePortInUse(occPort); err != nil || !inUse {
		t.Fatalf("被占用端口应判占用（true,nil），实得 (%v,%v)", inUse, err)
	}
}

// ---------- 启动路径前置判定 ----------

// telnetStartFixture 装配一台「有效控制台为 telnet」的 VM，并在 provider 里登记其当前声明
// 与镜像画像（与域组装同源）。返回 provider、mock 与派生端口。
func telnetStartFixture(t *testing.T, name string) (*Provider, *mockLibvirt, int) {
	t.Helper()
	vm := vmFixture(name)
	// 预置主盘，避免定义期走克隆路径（本用例只关心端口前置判定）。
	store := newMockStorage("/vms/" + name + "/disk.qcow2")
	api := newMockLibvirt()
	api.present[name] = true
	api.states[name] = domShutoff // 关机态：否则幂等分支直接成功，判不到前置
	p := newTestProvider(api, store, nil)
	// 有效控制台 = telnet（经镜像画像显式设定，与 specFor 取同一来源）。
	p.SetVMImageProfiles([]model.VMImageProfile{{Image: vm.Image, Console: "telnet"}})
	p.setVMSpec(vm, model.AllocatedResources{HugepageSize: "1G"})

	form, _ := model.EffectiveConsole(vm, []model.VMImageProfile{{Image: vm.Image, Console: "telnet"}})
	if form != "telnet" {
		t.Fatalf("夹具前置：有效控制台应为 telnet，实得 %q", form)
	}
	return p, api, model.ConsoleTelnetPort(name)
}

// (a) 端口被占用 ⇒ start 被拒且不调用 libvirt，错误为产品化文案（含端口与两条出路）。
func TestStartVM_TelnetPortOccupiedRefused(t *testing.T) {
	const name = "fw-vm"
	p, api, port := telnetStartFixture(t, name)

	hold, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("测试前置：占用派生端口 %d 失败: %v", port, err)
	}
	defer hold.Close()

	err = p.StartVM(context.Background(), name)
	if err == nil {
		t.Fatal("telnet 控制台端口被占用时应拒绝启动")
	}
	if len(api.started) != 0 {
		t.Fatalf("拒绝时不得调用 libvirt Start: %v", api.started)
	}
	if len(api.defined) != 0 {
		t.Fatalf("拒绝时不得重定义域（不产生半成品域）: %v", api.defined)
	}
	for _, want := range []string{name, strconv.Itoa(port), "已被本机其它进程占用", "serial/vnc", "VNF 名"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("产品化文案应含 %q：%v", want, err)
		}
	}
	// 用户可见文本不得含决策号。
	if strings.Contains(err.Error(), "#485") || strings.Contains(err.Error(), "决策") {
		t.Errorf("用户可见文案不应含决策号：%v", err)
	}
}

// (b) 端口空闲 ⇒ 行为与既有一致（照常定义并启动一次）。
func TestStartVM_TelnetPortFreeProceeds(t *testing.T) {
	const name = "fw-vm"
	p, api, port := telnetStartFixture(t, name)

	// 前置：确认派生端口空闲（否则跳过，避免环境偶发占用造成假红）。
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Skipf("测试前置：派生端口 %d 已被本机进程占用，跳过空闲用例: %v", port, err)
	}
	_ = ln.Close()

	if err := p.StartVM(context.Background(), name); err != nil {
		t.Fatalf("端口空闲时应照常启动: %v", err)
	}
	if len(api.started) != 1 || api.started[0] != name {
		t.Fatalf("应真的启动一次: %v", api.started)
	}
}

// (c) 非 telnet 形态（serial）不受该判定影响：即使派生端口被占用，start 也照常放行。
func TestStartVM_NonTelnetPortOccupiedStillStarts(t *testing.T) {
	const name = "fw-vm"
	vm := vmFixture(name)
	store := newMockStorage("/vms/" + name + "/disk.qcow2")
	api := newMockLibvirt()
	api.present[name] = true
	api.states[name] = domShutoff
	p := newTestProvider(api, store, nil)
	p.SetVMImageProfiles([]model.VMImageProfile{{Image: vm.Image, Console: "serial"}})
	p.setVMSpec(vm, model.AllocatedResources{HugepageSize: "1G"})

	// 占用派生端口（telnet 才用；serial 形态不绑它）。
	port := model.ConsoleTelnetPort(name)
	hold, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("测试前置：占用端口 %d 失败: %v", port, err)
	}
	defer hold.Close()

	if err := p.StartVM(context.Background(), name); err != nil {
		t.Fatalf("serial 形态不应受 telnet 端口占用影响: %v", err)
	}
	if len(api.started) != 1 {
		t.Fatalf("serial 形态应照常启动: %v", api.started)
	}
}

// (d) restart 的 off→start 分支同口径：端口被占用时拒绝且不进 libvirt。
func TestRestartVM_TelnetPortOccupiedOnOffBranch(t *testing.T) {
	const name = "fw-vm"
	p, api, port := telnetStartFixture(t, name)

	hold, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("测试前置：占用派生端口 %d 失败: %v", port, err)
	}
	defer hold.Close()

	err = p.RestartVM(context.Background(), name)
	if err == nil {
		t.Fatal("off→start 分支端口被占用时应拒绝")
	}
	if !strings.Contains(err.Error(), "已被本机其它进程占用") {
		t.Fatalf("应返回同一产品化文案：%v", err)
	}
	if len(api.started) != 0 || len(api.rebooted) != 0 {
		t.Fatalf("拒绝时不得调用 libvirt Start/Reboot: started=%v rebooted=%v", api.started, api.rebooted)
	}
}

// (e) 运行中 restart 走 ACPI 重启，不经该前置判定（不改变既有重启语义）。
func TestRestartVM_RunningSkipsTelnetPortCheck(t *testing.T) {
	const name = "fw-vm"
	p, api, port := telnetStartFixture(t, name)
	api.states[name] = domRunning // 运行中

	hold, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("测试前置：占用端口 %d 失败: %v", port, err)
	}
	defer hold.Close()

	if err := p.RestartVM(context.Background(), name); err != nil {
		t.Fatalf("运行中 restart 应走 ACPI 重启成功: %v", err)
	}
	if len(api.rebooted) != 1 || len(api.started) != 0 {
		t.Fatalf("应只调用 Reboot: rebooted=%v started=%v", api.rebooted, api.started)
	}
}

// (f) 拿不到该 VM 当前声明（裸启动，如单测未 setVMSpec）⇒ 不判、不误拒。
func TestStartVM_NoSpecSkipsTelnetPortCheck(t *testing.T) {
	const name = "fw-vm"
	api := newMockLibvirt()
	api.present[name] = true
	api.states[name] = domShutoff
	p := newTestProvider(api, newMockStorage("/vms/"+name+"/disk.qcow2"), nil)
	// 未 setVMSpec、未设画像 ⇒ 无法判定形态 ⇒ 不阻断。

	if err := p.StartVM(context.Background(), name); err != nil {
		t.Fatalf("拿不到声明时不应误拒启动: %v", err)
	}
	if len(api.started) != 1 {
		t.Fatalf("应照常启动: %v", api.started)
	}
}
