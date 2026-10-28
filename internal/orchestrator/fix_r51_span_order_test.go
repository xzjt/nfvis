package orchestrator

// round51 Q1：端口镜像会话的拆除必须排在**它引用的源接口**之前（先解引用、后删被引用）。
//
// 缺陷形态（静态评审定位）：删除段里 `del-vnf-if[...]`（删 VNF vNIC 接口）排在 `del-span[...]`
// 之前，而镜像会话的源可以是 VNF vNIC（PMSource.Vnf）。「同一次提交删掉会话 + 它的源 vNIC」
// 是**合法声明**——校验只要求源 vNIC 在**新**配置里存在（checkPortMirroring 只看新配置），
// 本场景新配置里两者都没有。于是旧顺序把接口先从数据面删掉，DeleteSpan 随后拿着已失效的
// sw_if_index 去关镜像 ⇒ VPP 报错、整次提交回滚（本可一次清干净）。
// 与 R88-6（del-vrf 之后才删 vNIC）、#342/#196 同一规则。

import (
	"context"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

func TestApplySpanDeleteBeforeItsSourceVnic(t *testing.T) {
	ap, calls := newRecApplier("")
	old := model.Config{
		VirtualMachineFunctions: []model.VMFunction{{Name: "vnf-a", Image: "img",
			Interfaces: []model.VnfInterface{{Name: "eth0", Type: "vhost-user"}}}},
		PortMirroring: []model.PortMirroring{{
			Name: "sp-1", Analyzer: "ens225",
			Source: model.PMSource{Vnf: "vnf-a", VnfInterface: "eth0", Direction: "both"},
		}},
	}
	newCfg := model.Config{}
	if err := ap.Apply(context.Background(), old, newCfg); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	spanIdx, ifIdx := -1, -1
	for i, c := range *calls {
		switch c {
		case "del-span:sp-1":
			spanIdx = i
		case "del-vnf-if:vnf-a/eth0":
			ifIdx = i
		}
	}
	if spanIdx < 0 || ifIdx < 0 {
		t.Fatalf("应同时出现 del-span 与 del-vnf-if: %v", *calls)
	}
	if spanIdx > ifIdx {
		t.Fatalf("删除顺序错误：镜像会话（引用源接口）必须先于其源 vNIC 删除: %v", *calls)
	}
}
