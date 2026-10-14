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
