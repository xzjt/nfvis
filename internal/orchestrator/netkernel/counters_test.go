package netkernel

// 决策 #504：内核网络数据面的接口计数——丢弃**按方向**（sysfs 的 rx_dropped/tx_dropped
// 是两个真值），出口发 rx_drops + tx_drops、**不发** drops（两方向不相加冒充总数）。
//
// 读法用临时目录模拟 sysfs（`sysfsNetRoot` 可改，与 state_test.go 同做法）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// fakeSysfs 在临时目录里造一个网卡的 statistics/ 目录。
func fakeSysfs(t *testing.T, ifname string, files map[string]string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ifname, "statistics")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, val := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(val), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := sysfsNetRoot
	sysfsNetRoot = root
	t.Cleanup(func() { sysfsNetRoot = old })
}

func fullCounterFiles(rxPkts, txPkts, rxBytes, txBytes, rxErr, txErr, rxDrop, txDrop string) map[string]string {
	return map[string]string{
		"rx_packets": rxPkts, "tx_packets": txPkts,
		"rx_bytes": rxBytes, "tx_bytes": txBytes,
		"rx_errors": rxErr, "tx_errors": txErr,
		"rx_dropped": rxDrop, "tx_dropped": txDrop,
	}
}

// 内核口径：两个方向的丢弃都读出来，且 JSON 里只有方向键、没有 drops。
func TestKernelInterfaceCountersDirectionalDrops(t *testing.T) {
	fakeSysfs(t, "ens192", fullCounterFiles("100", "200", "3000", "4000", "5", "6", "7", "8"))

	c, ok := New(nil).InterfaceCounters(context.Background(), "ens192")
	if !ok {
		t.Fatal("内核口计数应读得到")
	}
	if c.RxDrops != 7 || c.TxDrops != 8 {
		t.Fatalf("方向丢弃应为 rx=7 tx=8（sysfs 的 rx_dropped/tx_dropped），实得 rx=%d tx=%d", c.RxDrops, c.TxDrops)
	}
	if !c.DirectionalDrops {
		t.Fatal("内核口径必须标记为按方向（DirectionalDrops=true）")
	}
	if c.RxPackets != 100 || c.TxPackets != 200 || c.RxBytes != 3000 || c.TxBytes != 4000 ||
		c.RxErrors != 5 || c.TxErrors != 6 {
		t.Fatalf("其余计数映射不符：%+v", c)
	}

	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if v, ok := m["rx_drops"]; !ok || v.(float64) != 7 {
		t.Fatalf("内核口径必须发 rx_drops=7：%s", b)
	}
	if v, ok := m["tx_drops"]; !ok || v.(float64) != 8 {
		t.Fatalf("内核口径必须发 tx_drops=8：%s", b)
	}
	// 两方向**不相加**冒充总数（决策 #504 的如实边界）。
	if _, ok := m["drops"]; ok {
		t.Fatalf("内核口径不得发 drops（不相加冒充总数）：%s", b)
	}
}

// 0 也是有效读数：方向键必须发出（真没丢弃 ≠ 取不到）。
func TestKernelInterfaceCountersZeroDropsStillEmitted(t *testing.T) {
	fakeSysfs(t, "ens224", fullCounterFiles("0", "0", "0", "0", "0", "0", "0", "0"))
	c, ok := New(nil).InterfaceCounters(context.Background(), "ens224")
	if !ok {
		t.Fatal("内核口计数应读得到")
	}
	b, _ := json.Marshal(c)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"rx_drops", "tx_drops"} {
		if v, ok := m[k]; !ok || v.(float64) != 0 {
			t.Fatalf("全零现场 %q 仍应如实发出（0 是有效读数）：%s", k, b)
		}
	}
}

// 缺一个计数文件即 ok=false：半个读数比没有更危险（会让人以为口上没流量）。
func TestKernelInterfaceCountersMissingFileIsUnavailable(t *testing.T) {
	files := fullCounterFiles("1", "2", "3", "4", "5", "6", "7", "8")
	delete(files, "tx_dropped")
	fakeSysfs(t, "ens192", files)

	if _, ok := New(nil).InterfaceCounters(context.Background(), "ens192"); ok {
		t.Fatal("缺 tx_dropped 时不得给出半个读数（ok 应为 false）")
	}
}

// 内核里没有该 netdev（口被 DPDK 接管 / 名字写错）⇒ ok=false，不编造 0。
func TestKernelInterfaceCountersUnknownIface(t *testing.T) {
	fakeSysfs(t, "ens192", fullCounterFiles("1", "2", "3", "4", "5", "6", "7", "8"))
	if _, ok := New(nil).InterfaceCounters(context.Background(), "ens999"); ok {
		t.Fatal("内核里不存在的口不得报计数")
	}
	if _, ok := New(nil).InterfaceCounters(context.Background(), ""); ok {
		t.Fatal("空口名不得报计数")
	}
}

// 非数字内容（内核异常/截断）同样 ok=false，不把 0 当读数传出去。
func TestKernelInterfaceCountersGarbageIsUnavailable(t *testing.T) {
	files := fullCounterFiles("1", "2", "3", "4", "5", "6", "7", "8")
	files["rx_bytes"] = "not-a-number\n"
	fakeSysfs(t, "ens192", files)
	if _, ok := New(nil).InterfaceCounters(context.Background(), "ens192"); ok {
		t.Fatal("解析不了的计数应报不可用，不得当成 0")
	}
}
