package main

// 真机 3.0.5~dev1 回归（R2-15① 的读视图改造）：内核 Provider 必须接上**当前 committed 配置**
// 的来源——按配置声明枚举读视图（`show virtual-switches` / 端口清单 / 产品自持设备判定）时，
// 进程内快照只在装配与恢复收敛时写入，提交路径不经过 Provider（Apply* 只拿到单个对象），
// 于是提交后的读视图滞留旧快照：新建交换机提交成功、读视图恒空，15s 巡检也不刷新。
//
// 本文件钉住装配：① `*config.Engine` 确实满足来源接口；② 来源在**引擎构造之后、API 装配之前**
// 接线；③ 接线只能经 attachConfigSource（内核模式外为零操作，不许 panic）。
// 读视图随来源即时变化的行为断言在 netkernel 包（TestReadViewsFollowConfigSourceNotStaleSnapshot）。

import (
	"os"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/netkernel"
)

// 构造级：引擎即来源（接口实现不匹配会在这里编译失败——接线点取的就是它的 Committed）。
var _ configSource = (*config.Engine)(nil)

// 构造级：内核模式外（VPP 数据面 kernelNet=nil）或来源为 nil 时，接线必须是无害空操作。
func TestAttachConfigSourceToleratesNil(t *testing.T) {
	kp := netkernel.New(nil)
	attachConfigSource(nil, fakeCommittedSource{}) // VPP 模式：kernelNet 为 nil
	attachConfigSource(kp, nil)                    // 未给来源
	// 只要求两条空路径都不 panic；读视图本身在无 ip 命令的环境会如实报错，不计入断言。
	if _, err := kp.BridgeDomains(); err == nil {
		t.Log("BridgeDomains 可调用（正常环境会返回内核实况）")
	}
}

type fakeCommittedSource struct{ cfg model.Config }

func (f fakeCommittedSource) Committed() (model.Config, error) { return f.cfg, nil }

// 源码级：来源接线必须存在、且在引擎构造之后、API 装配之前（Provider 先于引擎构造，
// 放早了拿不到 committed；放晚了读视图有一段陈旧窗口）。
func TestConfigSourceWiringOrderInMain(t *testing.T) {
	mainSrc, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(mainSrc)
	const want = "attachConfigSource(kernelNet, engine)"
	iWire := strings.Index(src, want)
	if iWire < 0 {
		t.Fatalf("main.go 缺少 %q（内核读视图的 committed 来源接线）", want)
	}
	iEngine := strings.Index(src, "engine, err := config.NewEngine(")
	iAPI := strings.Index(src, "apiServer := api.New(")
	if iEngine < 0 || iAPI < 0 {
		t.Fatalf("main.go 结构变了（找不到引擎构造或 API 装配点），请人工核对来源接线位置")
	}
	if !(iEngine < iWire && iWire < iAPI) {
		t.Fatalf("来源接线位置不对：引擎构造@%d 接线@%d API 装配@%d（应在引擎之后、API 之前）",
			iEngine, iWire, iAPI)
	}

	// 接线必须经 attachConfigSource（构造与注入一体，nil 安全有单一落点）。
	dpSrc, err := os.ReadFile("dataplane.go")
	if err != nil {
		t.Fatal(err)
	}
	body := funcBodyForTest(string(dpSrc), "func attachConfigSource(")
	if body == "" {
		t.Fatalf("dataplane.go 里找不到 attachConfigSource")
	}
	if !strings.Contains(body, "kp.SetConfigSource(src.Committed)") {
		t.Fatalf("attachConfigSource 应把来源接到 Provider 上：\n%s", body)
	}
}

// funcBodyForTest 取源码里某个函数（以签名前缀定位）到下一个顶层 func 之前的正文。
func funcBodyForTest(src, sig string) string {
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
