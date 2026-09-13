package network

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// ---------- M3-7（三）：SR-IOV VF 数量 ----------

func TestSRIOVSetVFCount(t *testing.T) {
	var gotPath, gotData string
	p := &SRIOVProvider{
		PathFor: func(ifname string) string { return "/sys/class/net/" + ifname + "/device/sriov_numvfs" },
		Write: func(path string, data []byte) error {
			gotPath, gotData = path, string(data)
			return nil
		},
	}
	if err := p.SetVFCount(context.Background(), "ens1f0", 4); err != nil {
		t.Fatalf("SetVFCount: %v", err)
	}
	if gotPath != "/sys/class/net/ens1f0/device/sriov_numvfs" || gotData != "4" {
		t.Fatalf("写入不符: %q %q", gotPath, gotData)
	}
	if err := p.SetVFCount(context.Background(), "ens1f0", -1); err == nil {
		t.Fatalf("负数量应报错")
	}
	if err := p.SetVFCount(context.Background(), "", 1); err == nil {
		t.Fatalf("空接口名应报错")
	}
	// PF 不支持 SR-IOV（sysfs 不存在）
	p2 := &SRIOVProvider{Write: func(string, []byte) error { return os.ErrNotExist }}
	if err := p2.SetVFCount(context.Background(), "vmx0", 2); err == nil || !strings.Contains(err.Error(), "SR-IOV") {
		t.Fatalf("不支持应给明确错误: %v", err)
	}
	// 其它写入错误
	p3 := &SRIOVProvider{Write: func(string, []byte) error { return errors.New("EPERM") }}
	if err := p3.SetVFCount(context.Background(), "ens1f0", 2); err == nil {
		t.Fatalf("写入错误应上抛")
	}
}
