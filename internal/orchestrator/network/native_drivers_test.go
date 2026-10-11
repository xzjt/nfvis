package network

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 决策 #464：接管前记下原生驱动；解绑不带 to-driver 时按「显式 > 记录 > 探测」交还。
//
// 夹具刻意复现 round17 的真机行为：**内核不会自动 probe**——解绑后驱动链接消失、rescan 不恢复；
// 只有显式 `bind` 才让链接回来。这样「没有记录 ⇒ 报错要 to-driver」与「有记录 ⇒ 一次成功」
// 两条路径都能被区分开（否则假 sysfs 里链接一直在，探测会假成功、红-绿分不出来）。
func newNativeDriverFixture(t *testing.T, pci, native string) (b *DPDKBinder, nd *NativeDrivers, logs *[]string, writes *map[string][]string, root string) {
	t.Helper()
	root, _ = fakeSysfs(t, "ens224", pci, native)
	b, writes = newTestBinder(t, root)
	driverLink := filepath.Join(root, "bus", "pci", "devices", pci, "driver")
	inner := b.WriteFile
	b.WriteFile = func(path string, data []byte) error {
		if err := inner(path, data); err != nil {
			return err
		}
		slash := filepath.ToSlash(path)
		switch {
		case strings.HasSuffix(slash, "/unbind"):
			_ = os.Remove(driverLink) // 解绑后无驱动（内核不自动 probe）
		case strings.Contains(slash, "/bus/pci/drivers/") && strings.HasSuffix(slash, "/bind"):
			drv := filepath.Base(filepath.Dir(path))
			_ = os.Remove(driverLink)
			_ = os.Symlink(filepath.Join(root, "bus", "pci", "drivers", drv), driverLink) // 显式 bind 生效
		}
		return nil
	}
	logs = &[]string{}
	b.Logf = func(f string, a ...any) { *logs = append(*logs, fmt.Sprintf(f, a...)) }
	nd = NewNativeDrivers(filepath.Join(root, "var", "dpdk-native-drivers.json"))
	b.NativeDrivers = nd
	return b, nd, logs, writes, root
}

// toPostBindState 把设备改成「已交 DPDK」的样子：驱动链接指向 vfio-pci（真机 bind 之后即如此）。
func toPostBindState(t *testing.T, root, pci string) {
	t.Helper()
	driverLink := filepath.Join(root, "bus", "pci", "devices", pci, "driver")
	_ = os.Remove(driverLink)
	if err := os.Symlink(filepath.Join(root, "bus", "pci", "drivers", DefaultUioDriver), driverLink); err != nil {
		t.Fatal(err)
	}
}

// 接管前记录；解绑不带 to-driver 时按记录交还（本平台内核不会自动 probe）。
func TestNativeDriverRecordedAndUsedOnUnbind(t *testing.T) {
	const pci = "0000-13-00.0"
	b, nd, logs, writes, root := newNativeDriverFixture(t, pci, "vmxnet3")

	// ① 绑定：记下 vmxnet3
	if _, err := b.Bind(context.Background(), "ens224", ""); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if got, ok := nd.Get(pci); !ok || got != "vmxnet3" {
		t.Fatalf("接管前应记录原生驱动 vmxnet3：%q ok=%v", got, ok)
	}
	if !strings.Contains(strings.Join(*logs, "\n"), "已记录 "+pci+" 的原生驱动：vmxnet3") {
		t.Fatalf("应如实记录一条 journal：%v", *logs)
	}

	// ② 解绑（不带 to-driver）：应显式绑回 vmxnet3 —— 改前这里会报「内核未自动重新探测」
	toPostBindState(t, root, pci)
	if _, err := b.Unbind(context.Background(), "ens224", ""); err != nil {
		t.Fatalf("有记录时解绑不该失败：%v", err)
	}
	if got := (*writes)[filepath.Join(root, "bus", "pci", "drivers", "vmxnet3", "bind")]; len(got) != 1 || got[0] != pci {
		t.Fatalf("应按记录显式绑定到 vmxnet3：%v", got)
	}
	if !strings.Contains(strings.Join(*logs, "\n"), "按绑定时记录的原生驱动交还") {
		t.Fatalf("journal 应写明本次走的是「记录」这一路：%v", *logs)
	}
}

// 显式 to-driver 优先于记录。
func TestUnbindExplicitToDriverWins(t *testing.T) {
	const pci = "0000-13-00.0"
	b, nd, _, writes, root := newNativeDriverFixture(t, pci, "vmxnet3")
	if err := nd.Set(pci, "vmxnet3"); err != nil {
		t.Fatal(err)
	}
	toPostBindState(t, root, pci)

	if _, err := b.Unbind(context.Background(), "ens224", "e1000e"); err != nil {
		t.Fatalf("显式指定应照做：%v", err)
	}
	if got := (*writes)[filepath.Join(root, "bus", "pci", "drivers", "e1000e", "bind")]; len(got) != 1 {
		t.Fatalf("应绑到显式指定的 e1000e：%v", got)
	}
	if got := (*writes)[filepath.Join(root, "bus", "pci", "drivers", "vmxnet3", "bind")]; len(got) != 0 {
		t.Fatalf("显式指定时不该再动记录里的驱动：%v", got)
	}
}

// 没有记录 ⇒ 退回 rescan 探测；本夹具复现「内核不 probe」⇒ 如实报错并给照做路径（既有行为不变）。
func TestUnbindWithoutRecordFallsBackToProbe(t *testing.T) {
	const pci = "0000-13-00.0"
	b, _, _, writes, root := newNativeDriverFixture(t, pci, "vmxnet3")
	toPostBindState(t, root, pci)

	_, err := b.Unbind(context.Background(), "ens224", "")
	if err == nil {
		t.Fatal("无记录且内核不 probe 时应如实报错")
	}
	if !strings.Contains(err.Error(), "to-driver") {
		t.Fatalf("错误应给照做路径：%v", err)
	}
	// 既有的 sysfs 写序不变：先解绑、再清 override、再 rescan
	if got := (*writes)[filepath.Join(root, "bus/pci/devices", pci, "driver_override")]; len(got) != 1 || strings.TrimSpace(got[0]) != "" {
		t.Fatalf("应清空 driver_override：%v", got)
	}
	if got := (*writes)[filepath.Join(root, "bus/pci/rescan")]; len(got) != 1 {
		t.Fatalf("应触发 rescan：%v", got)
	}
}

// 记录里的驱动已不在（模块被卸/改名）⇒ 如实退回探测路径（不硬绑、不谎报）。
func TestUnbindStaleRecordFallsBack(t *testing.T) {
	const pci = "0000-13-00.0"
	b, nd, logs, writes, root := newNativeDriverFixture(t, pci, "vmxnet3")
	if err := nd.Set(pci, "gone-driver"); err != nil {
		t.Fatal(err)
	}
	toPostBindState(t, root, pci)

	if _, err := b.Unbind(context.Background(), "ens224", ""); err == nil {
		t.Fatal("记录里的驱动不可用且内核不 probe 时应如实报错")
	}
	if got := (*writes)[filepath.Join(root, "bus", "pci", "drivers", "gone-driver", "bind")]; len(got) != 0 {
		t.Fatalf("不该硬绑到不可用的驱动：%v", got)
	}
	if !strings.Contains(strings.Join(*logs, "\n"), "当前不可用") {
		t.Fatalf("应如实说明记录里的驱动不可用：%v", *logs)
	}
}

// 只记非 DPDK 用户态驱动：从 vfio-pci 换到 igb_uio 之类的**重绑**不该把它记成原生驱动。
func TestBindFromUioDriverNotRecorded(t *testing.T) {
	const pci = "0000-13-00.0"
	b, nd, _, _, root := newNativeDriverFixture(t, pci, DefaultUioDriver)
	if err := os.MkdirAll(filepath.Join(root, "bus", "pci", "drivers", "igb_uio"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := b.Bind(context.Background(), "ens224", "igb_uio"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if got, ok := nd.Get(pci); ok {
		t.Fatalf("从 DPDK 驱动重绑不该记原生驱动：%q", got)
	}
}

// IsDPDKUioDriver：三个标准 DPDK 驱动为真，内核驱动为假。
func TestIsDPDKUioDriver(t *testing.T) {
	for _, d := range []string{"vfio-pci", "igb_uio", "uio_pci_generic"} {
		if !IsDPDKUioDriver(d) {
			t.Fatalf("%s 应判为 DPDK 用户态驱动", d)
		}
	}
	for _, d := range []string{"vmxnet3", "e1000e", "i40e", "", " vfio-pci-x"} {
		if IsDPDKUioDriver(d) {
			t.Fatalf("%q 不该判为 DPDK 用户态驱动", d)
		}
	}
}
