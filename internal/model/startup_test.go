package model

import "testing"

func TestVMImageProfileJSONKeys(t *testing.T) {
	c := Config{VMImageProfiles: []VMImageProfile{{Image: "img1", Machine: "q35", ExtraArgs: []string{"-cpu host"}}}}
	if c.VMImageProfiles[0].Image != "img1" {
		t.Fatalf("image key not held")
	}
	vm := VMFunction{Name: "v1", Startup: &VMStartupOverride{Console: "vnc"}}
	if vm.Startup == nil || vm.Startup.Console != "vnc" {
		t.Fatalf("startup override not held")
	}
}

func TestEffectiveStartupPrecedence(t *testing.T) {
	profiles := []VMImageProfile{{Image: "img1", Machine: "pc", Firmware: "uefi", ExtraArgs: []string{"-global x"}}}
	vm := VMFunction{Name: "v1", Image: "img1", Startup: &VMStartupOverride{Machine: "pc-q35-6.2"}}
	got := EffectiveStartup(vm, profiles)
	if got.Machine != "pc-q35-6.2" { // 实例覆盖 > 画像
		t.Fatalf("instance override lost: %q", got.Machine)
	}
	if got.Firmware != "uefi" { // 画像 > 默认
		t.Fatalf("profile value lost: %q", got.Firmware)
	}
	if got.Accel != "kvm" || got.Console != "serial" || got.DiskIface != "virtio" || got.BootOrder != "hd" {
		t.Fatalf("defaults not filled: %+v", got)
	}
	if len(got.ExtraArgs) != 1 || got.ExtraArgs[0] != "-global x" {
		t.Fatalf("profile extra args lost: %+v", got.ExtraArgs)
	}
}

func TestEffectiveStartupNoProfile(t *testing.T) {
	got := EffectiveStartup(VMFunction{Name: "v1", Image: "none"}, nil)
	if got.Machine != "q35" || got.CPUMode != "host-passthrough" {
		t.Fatalf("defaults not applied: %+v", got)
	}
}

func TestValidateExtraArgsDenylist(t *testing.T) {
	for _, bad := range []string{"-m 8G", "-smp 4", "-object memory-backend-file", "-netdev tap", "-cpu host", "-drive file=x", "-M q35", "-serial tcp"} {
		if err := ValidateExtraArgs([]string{bad}); err == nil {
			t.Fatalf("expected reject for %q", bad)
		}
	}
	for _, ok := range []string{"-global kvm-pit.lost_tick_policy=discard", "-rtc base=utc", "-smbios type=1", "-no-reboot"} {
		if err := ValidateExtraArgs([]string{ok}); err != nil {
			t.Fatalf("expected accept for %q: %v", ok, err)
		}
	}
}

// EffectiveConsole（决策 #485 契约 §4.3）：显式设定的形态**权威**（explicit=true），
// 未显式设定时回落产品默认 serial（explicit=false）。
func TestEffectiveConsoleExplicit(t *testing.T) {
	// 画像显式 console=telnet ⇒ explicit、形态 telnet。
	p := []VMImageProfile{{Image: "img1", Console: "telnet"}}
	if form, exp := EffectiveConsole(VMFunction{Name: "v1", Image: "img1"}, p); form != "telnet" || !exp {
		t.Fatalf("画像 telnet 应 explicit: form=%q explicit=%v", form, exp)
	}
	// 实例覆盖优先且显式。
	vm := VMFunction{Name: "v1", Image: "img1", Startup: &VMStartupOverride{Console: "serial"}}
	if form, exp := EffectiveConsole(vm, p); form != "serial" || !exp {
		t.Fatalf("实例覆盖 serial 应盖过画像并 explicit: form=%q explicit=%v", form, exp)
	}
	// 无画像/无覆盖 ⇒ 默认 serial、非显式。
	if form, exp := EffectiveConsole(VMFunction{Name: "v2", Image: "none"}, nil); form != "serial" || exp {
		t.Fatalf("无画像应回落默认且非显式: form=%q explicit=%v", form, exp)
	}
}

// ConsoleTelnetPort 确定性 + 区间（唯一实现位于 model，域组装与端口冲突校验共用）。
func TestConsoleTelnetPortDeterministic(t *testing.T) {
	a := ConsoleTelnetPort("fw-vm")
	if a != ConsoleTelnetPort("fw-vm") {
		t.Fatal("同名应恒等")
	}
	if a < ConsolePortMin || a > ConsolePortMax {
		t.Fatalf("端口应在 [%d,%d]，实得 %d", ConsolePortMin, ConsolePortMax, a)
	}
}
