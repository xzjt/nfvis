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
