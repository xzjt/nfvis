package network

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
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

// ---------- V1 收尾（决策 #70）：声明式 vf_count 必须落实 ----------
//
// 此前 interfaces[].sriov.vf_count 被持久化、commit 也成功，但没有任何代码执行它
// （SetVFCount 只被命令式 CLI/API 调用），属「配置静默无操作」——
// 配置层承诺 4 个 VF，实际 0 个且无提示。

func TestApplyInterfaceAppliesDeclarativeVFCount(t *testing.T) {
	var writes []string
	prov := &SRIOVProvider{
		PathFor: func(ifname string) string { return "/fake/" + ifname },
		Write: func(path string, data []byte) error {
			writes = append(writes, path+"="+string(data))
			return nil
		},
	}
	np := NewL2Network(orchestrator.NewNoopNetwork(), nil)
	np.SetSRIOV(prov)

	// 配置了 vf-count → 必须写入 sysfs
	if err := np.ApplyInterface(context.Background(), model.InterfaceConfig{
		Name: "ens1f0", Sriov: &model.InterfaceSriov{VFCount: 4},
	}); err != nil {
		t.Fatalf("ApplyInterface: %v", err)
	}
	if len(writes) != 1 || writes[0] != "/fake/ens1f0=4" {
		t.Fatalf("声明式 vf-count 未落实: %v", writes)
	}

	// vf-count 0 = 回收全部，同样必须落实（不是「未配置」）
	writes = nil
	if err := np.ApplyInterface(context.Background(), model.InterfaceConfig{
		Name: "ens1f0", Sriov: &model.InterfaceSriov{VFCount: 0},
	}); err != nil {
		t.Fatalf("ApplyInterface(0): %v", err)
	}
	if len(writes) != 1 || writes[0] != "/fake/ens1f0=0" {
		t.Fatalf("vf-count 0 应写 0（回收全部）: %v", writes)
	}

	// 未配置 sriov → 不触碰 sysfs
	writes = nil
	if err := np.ApplyInterface(context.Background(), model.InterfaceConfig{Name: "ens192"}); err != nil {
		t.Fatalf("ApplyInterface(无 sriov): %v", err)
	}
	if len(writes) != 0 {
		t.Fatalf("未配置 sriov 不应写 sysfs: %v", writes)
	}
}

// PF 不支持 SR-IOV（如 vmxnet3）时须**明确报错**，不得静默通过。
func TestApplyInterfaceVFCountErrorNotSilent(t *testing.T) {
	prov := &SRIOVProvider{
		PathFor: func(string) string { return "/fake/pf" },
		Write:   func(string, []byte) error { return errors.New("no such file (PF 不支持 SR-IOV)") },
	}
	np := NewL2Network(orchestrator.NewNoopNetwork(), nil)
	np.SetSRIOV(prov)
	err := np.ApplyInterface(context.Background(), model.InterfaceConfig{
		Name: "ens192", Sriov: &model.InterfaceSriov{VFCount: 4},
	})
	if err == nil || !strings.Contains(err.Error(), "VF 数量") {
		t.Fatalf("PF 不支持时应明确报错（不得静默）: %v", err)
	}
}

// 未装配 SR-IOV 编排器时，配置了 vf-count 也须明确报错（不静默放过）。
func TestApplyInterfaceVFCountUnwired(t *testing.T) {
	np := NewL2Network(orchestrator.NewNoopNetwork(), nil) // 未 SetSRIOV
	err := np.ApplyInterface(context.Background(), model.InterfaceConfig{
		Name: "ens1f0", Sriov: &model.InterfaceSriov{VFCount: 4},
	})
	if err == nil || !strings.Contains(err.Error(), "SR-IOV 未接入") {
		t.Fatalf("未装配应明确报错: %v", err)
	}
}
