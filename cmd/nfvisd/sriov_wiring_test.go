package main

// v3 round2 体检 R2-7：SR-IOV 编排的装配顺序回归。
//
// 旧装配：`l2net.SetSRIOV(sriovProvider)` 写在 `sriovProvider = network.NewSRIOVProvider()`
// **之前**（VPP 分支内先注入、if/else 之后才构造）⇒ VPP（缺省）数据面拿到的是 nil，
// 声明式 `interfaces … sriov vf-count N` 从「可下发」变成硬失败（v2 回归；内核侧不受影响，
// 因为内核 Provider 自建 provider，套件又把该语句列为「环境受限必被正确拒绝」，所以抓不到）。
//
// 修法：构造与注入收敛到**同一个函数** attachSRIOV（构造先于注入在结构上不可能写反），
// 装配处只调用它一次。本文件钉住该结构（行为 + 源码级两条）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// 行为：attachSRIOV 注入的必须是**同一个** provider 实例（重定向 sysfs 写入路径后
// `interfaces … sriov vf-count` 走得到它；未注入时 ApplyInterface 会报「SR-IOV 未接入」）。
func TestAttachSRIOVInjectsProviderIntoVPPNetwork(t *testing.T) {
	l2 := network.NewL2Network(nil, nil)
	p := attachSRIOV(l2)
	if p == nil {
		t.Fatal("attachSRIOV 必须返回可用 provider")
	}
	dir := t.TempDir()
	p.PathFor = func(string) string { return filepath.Join(dir, "sriov_numvfs") }
	err := l2.ApplyInterface(context.Background(), model.InterfaceConfig{
		Name: "ens192", Sriov: &model.InterfaceSriov{VFCount: 2},
	})
	if err != nil {
		t.Fatalf("注入后 sriov vf-count 应可下发（修复前这里报「SR-IOV 未接入」）：%v", err)
	}
	b, rerr := os.ReadFile(filepath.Join(dir, "sriov_numvfs"))
	if rerr != nil || string(b) != "2" {
		t.Fatalf("VF 数量应写到注入实例的路径：content=%q err=%v", b, rerr)
	}

	// 内核数据面（l2net=nil）同样必须拿到 provider——API 层的命令式 VF 路径用同一个实例。
	if attachSRIOV(nil) == nil {
		t.Fatal("l2net 为 nil（内核数据面）时也要返回 provider")
	}
}

// 源码级守护：`SetSRIOV` 只能出现在 attachSRIOV 体内，且必须**在构造之后**。
// main.go 里再出现裸调用 = 顺序回归的形态（R2-7 就是那样写出来的）。
func TestSRIOVWiringKeepsConstructorBeforeInjection(t *testing.T) {
	mainSrc, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mainSrc), "SetSRIOV(") {
		t.Fatalf("main.go 不得直接调用 SetSRIOV——构造与注入必须一体（见 dataplane.go 的 attachSRIOV）")
	}
	dpSrc, err := os.ReadFile("dataplane.go")
	if err != nil {
		t.Fatal(err)
	}
	body := funcBody(string(dpSrc), "func attachSRIOV(")
	if body == "" {
		t.Fatalf("dataplane.go 里找不到 attachSRIOV（装配顺序的唯一收敛点）")
	}
	iNew := strings.Index(body, "NewSRIOVProvider()")
	iSet := strings.Index(body, "SetSRIOV(")
	if iNew < 0 || iSet < 0 || iNew > iSet {
		t.Fatalf("attachSRIOV 必须先构造（NewSRIOVProvider）后注入（SetSRIOV）：\n%s", body)
	}
}

// funcBody 取源码里某个函数（以签名前缀定位）到下一个顶层 func 之前的正文。
func funcBody(src, sig string) string {
	i := strings.Index(src, sig)
	if i < 0 {
		return ""
	}
	rest := src[i:]
	if j := strings.Index(rest[1:], "\nfunc "); j >= 0 {
		return rest[:j+1]
	}
	return rest
}
