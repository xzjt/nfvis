package network

// 决策 #504：接口丢弃计数如实化——`Drops` 必须真的从 stats 值映射出来，
// 且方向字段在 VPP 口径下不得被填（VPP 的 /interfaces/<if>/drops 不区分方向）。
//
// 红-绿：本用例在修复前必失败——`interfaceCountersFrom` 不读 `c.Drops`，丢弃恒 0，
// 而真机同一时刻 `vpp_get_stats /interfaces/ens192/drops=148`（round47 现场）。

import (
	"encoding/json"
	"testing"

	"go.fd.io/govpp/api"
)

// 假 stats 值 ⇒ 出口出现该数（旧实现恒 0 ⇒ 必失败）。
func TestInterfaceCountersFromMapsDrops(t *testing.T) {
	c := api.InterfaceCounters{
		InterfaceIndex: 4,
		InterfaceName:  "ens192",
		Drops:          148, // 真机 stats segment 的 /interfaces/ens192/drops
		RxErrors:       1,
		TxErrors:       2,
	}
	c.Rx.Packets, c.Rx.Bytes = 85, 12345
	c.Tx.Packets, c.Tx.Bytes = 3, 678

	got := interfaceCountersFrom(c)
	if got.Drops != 148 {
		t.Fatalf("Drops 应映射为 148（stats 值），实得 %d——丢弃没接线时恒 0", got.Drops)
	}
	if got.RxPackets != 85 || got.TxPackets != 3 || got.RxBytes != 12345 || got.TxBytes != 678 {
		t.Fatalf("packets/bytes 映射回归：%+v", got)
	}
	if got.RxErrors != 1 || got.TxErrors != 2 {
		t.Fatalf("errors 映射回归：%+v", got)
	}
	// 方向字段**必须留空**：VPP 只有总数，填进 rx/tx 就是把总数摊成方向（编造）。
	if got.RxDrops != 0 || got.TxDrops != 0 {
		t.Fatalf("VPP 口径不得填方向丢弃（RxDrops=%d TxDrops=%d）——只有一个总数", got.RxDrops, got.TxDrops)
	}
	if got.DirectionalDrops {
		t.Fatal("VPP 口径不是按方向提供的（DirectionalDrops 应为 false）")
	}
}

// VPP 下的 JSON 形状：**有 drops、没有 rx_drops/tx_drops**（宁缺不谎报，不是发 0）。
func TestInterfaceCountersFromJSONVPPShape(t *testing.T) {
	b, err := json.Marshal(interfaceCountersFrom(api.InterfaceCounters{Drops: 148}))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if v, ok := m["drops"]; !ok {
		t.Fatalf("VPP 口径必须有 drops 键：%s", b)
	} else if v.(float64) != 148 {
		t.Fatalf("drops = %v，应为 148", v)
	}
	for _, k := range []string{"rx_drops", "tx_drops"} {
		if _, ok := m[k]; ok {
			t.Fatalf("VPP 口径不得出现 %q（该口径在本数据面不适用）：%s", k, b)
		}
	}
}

// 0 是有效读数：drops=0 必须**发出去**（真没丢弃 vs 取不到，靠键在不在区分）。
func TestInterfaceCountersFromJSONVPPZeroIsReal(t *testing.T) {
	b, _ := json.Marshal(interfaceCountersFrom(api.InterfaceCounters{Drops: 0}))
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if v, ok := m["drops"]; !ok || v.(float64) != 0 {
		t.Fatalf("drops=0 应如实发出（0 是有效读数，不是「取不到」）：%s", b)
	}
}
