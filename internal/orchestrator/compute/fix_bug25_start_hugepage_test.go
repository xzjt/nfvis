package compute

// BUG-25 第二部分守护：VM **启动前**的大页可用量预检。
//
// 缺陷现场（真机）：`resource-pools hugepages page-size 1G count 2` 提交后重启，池建好了，
// 但 VM 起不来，操作者拿到的是 libvirt 英文原文
//   Unable to find any usable hugetlbfs mount for 2048 KiB
// ——不说哪个池、不说差多少页、不给照做路径。本预检把这条失败提前到 api.Start 之前，
// 用中文给出「需要 N 页 / 空闲 M 页 / 照做命令」。
//
// 判据：
//   - 空闲不足 ⇒ 立即失败，错误含页尺寸、需要-可用页数、照做命令（set resource-pools …）；
//   - 空闲充足 ⇒ 不报（正常启动语义不变）；
//   - 探针读不到（ok=false）⇒ **不判**（没有事实就不拒，与 checkConsolePortForStart 同口径）；
//   - 未注入探针 ⇒ 不判（单测/未装配环境与修复前逐字一致）；
//   - backing=normal（普通内存）⇒ 不判（不占大页池）；
//   - 未持有该 VM 的声明（裸启动）⇒ 不判。
//
// ⚠️ 与既有行为**不重复**：`checkDataPlaneForStart` 判数据面进程在不在、
// `checkConsolePortForStart` 判 telnet 派生端口被谁占——此前**没有任何一层**判「内核当下
// 空闲大页够不够」（model 的 PoolLedger 只按**配置声明**做配额演练，声明 2 页而内核只分配到
// 1 页时账本照样通过）。三条落点相同（api.Start 之前）、互不替代。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// stubHugepageProbe 注入假探针（按页尺寸返回固定空闲页数）。
func stubHugepageProbe(p *Provider, freeBySize map[string]int, known map[string]bool) {
	p.cfg.HugepageProbe = func(pageSize string, _ int) (int, bool) {
		if known != nil && !known[pageSize] {
			return 0, false
		}
		free, ok := freeBySize[pageSize]
		if !ok {
			return 0, false
		}
		return free, true
	}
}

// startProbeFixture 建一个「已持有声明、域不存在」的 provider（启动路径最短）。
func startProbeFixture(t *testing.T, vm model.VMFunction, pageSize string) (*Provider, *mockLibvirt) {
	t.Helper()
	api := newMockLibvirt()
	p := newTestProvider(api, newMockStorage("/images/img.qcow2"), nil)
	p.setVMSpec(vm, model.AllocatedResources{HugepageSize: pageSize})
	return p, api
}

// 空闲页不足：必须在**碰 libvirt 之前**失败，且文案含页尺寸 / 需要-可用 / 照做命令。
func TestBug25StartFailsWhenHugepagesShort(t *testing.T) {
	vm := vmFixture("fw-vm") // 1024MB / 1G 页 ⇒ 需要 1 页
	p, api := startProbeFixture(t, vm, "1G")
	stubHugepageProbe(p, map[string]int{"1G": 0}, nil) // 1G 池一页空闲都没有

	err := p.StartVM(context.Background(), "fw-vm")
	if err == nil {
		t.Fatal("空闲大页不足时启动必须失败")
	}
	msg := err.Error()
	for _, want := range []string{"大页不足", "1G", "需要 1 页", "空闲 0 页", "1024MB", "set resource-pools hugepages page-size 1G", "reboot"} {
		if !strings.Contains(msg, want) {
			t.Errorf("失败文案应含 %q：%s", want, msg)
		}
	}
	// 关键：不得把 libvirt 的英文原文原样抛给操作者（那是缺陷现象本身）。
	if strings.Contains(strings.ToLower(msg), "unable to find any usable hugetlbfs mount") {
		t.Errorf("不应透传 libvirt 英文原文：%s", msg)
	}
	// 也在 api.Start 之前失败：域未被定义、也未被启动（不留半成品现场）。
	if len(api.started) != 0 {
		t.Fatalf("预检命中时不得进入 api.Start（否则会留半成品域）：%v", api.started)
	}
}

// 需求页数按页尺寸换算且**向上取整**（2048MB / 1G = 2 页；2049MB 不存在——校验要求整数倍，
// 这里用 2M 页验证「1024MB / 2M = 512 页」的量差文案）。
func TestBug25StartShortfallShowsExactPagesNeeded(t *testing.T) {
	vm := vmFixture("fw-vm")
	vm.Memory = model.VMMemory{SizeMB: 1024, HugepageSize: "2M"}
	p, _ := startProbeFixture(t, vm, "2M")
	stubHugepageProbe(p, map[string]int{"2M": 500}, nil) // 差 12 页

	err := p.StartVM(context.Background(), "fw-vm")
	if err == nil {
		t.Fatal("2M 池 500 页 < 需要 512 页，启动必须失败")
	}
	msg := err.Error()
	for _, want := range []string{"2M", "需要 512 页", "空闲 500 页", "每页 2MB", "1024MB"} {
		if !strings.Contains(msg, want) {
			t.Errorf("文案应含 %q：%s", want, msg)
		}
	}
}

// 空闲充足：不报（正常路径语义不变）。
func TestBug25StartPassesWhenHugepagesEnough(t *testing.T) {
	vm := vmFixture("fw-vm")
	p, api := startProbeFixture(t, vm, "1G")
	stubHugepageProbe(p, map[string]int{"1G": 4}, nil)

	if err := p.StartVM(context.Background(), "fw-vm"); err != nil {
		t.Fatalf("空闲 4 页 ≥ 需要 1 页，启动不应被拦：%v", err)
	}
	if len(api.started) != 1 || api.started[0] != "fw-vm" {
		t.Fatalf("充足时应正常进入 api.Start：%v", api.started)
	}
}

// 探针读不到（内核没提供该池/sysfs 不可读）：**不判**——拿不到事实就不拒合法启动。
func TestBug25StartNotJudgedWhenProbeUnavailable(t *testing.T) {
	vm := vmFixture("fw-vm")
	p, api := startProbeFixture(t, vm, "1G")
	stubHugepageProbe(p, map[string]int{"1G": 0}, map[string]bool{"1G": false}) // 明确「读不到」

	if err := p.StartVM(context.Background(), "fw-vm"); err != nil {
		t.Fatalf("探针读不到时不得拒绝启动：%v", err)
	}
	if len(api.started) != 1 {
		t.Fatalf("未判定时应正常启动：%v", api.started)
	}
}

// 未注入探针：与修复前逐字一致（不判）。
func TestBug25StartNotJudgedWithoutProbe(t *testing.T) {
	vm := vmFixture("fw-vm")
	p, api := startProbeFixture(t, vm, "1G")
	// 显式置 nil：默认就是 nil（NewProvider 不注入），这条把「不注入 = 不判」钉住。
	p.cfg.HugepageProbe = nil

	if err := p.StartVM(context.Background(), "fw-vm"); err != nil {
		t.Fatalf("未注入探针时不得拒绝启动：%v", err)
	}
	if len(api.started) != 1 {
		t.Fatalf("未注入时应正常启动：%v", api.started)
	}
}

// backing=normal（普通内存）：不占大页池，即使池里一页都没有也不判。
func TestBug25StartSkipsNormalMemory(t *testing.T) {
	vm := vmFixture("fw-vm")
	vm.Memory = model.VMMemory{SizeMB: 1024, Backing: "normal"}
	p, api := startProbeFixture(t, vm, "1G") // 账本对 normal 不给页尺寸，这里故意留着也照样跳过
	stubHugepageProbe(p, map[string]int{"1G": 0}, nil)

	if err := p.StartVM(context.Background(), "fw-vm"); err != nil {
		t.Fatalf("普通内存不占大页池，不应被大页预检拦：%v", err)
	}
	if len(api.started) != 1 {
		t.Fatalf("应正常启动：%v", api.started)
	}
}

// 未持有该 VM 的声明（裸启动）：不判（与 checkConsolePortForStart 同口径）。
func TestBug25StartSkipsWithoutSpec(t *testing.T) {
	api := newMockLibvirt()
	api.present["ghost"] = true
	api.states["ghost"] = domShutoff
	p := newTestProvider(api, newMockStorage("/images/img.qcow2"), nil)
	stubHugepageProbe(p, map[string]int{"1G": 0}, nil)

	if err := p.StartVM(context.Background(), "ghost"); err != nil {
		t.Fatalf("拿不到声明时不得启用大页预检：%v", err)
	}
}

// restart 的 off→start 分支与 start 同口径（同一份「空闲页不够就别碰 libvirt」）。
func TestBug25RestartOffStartAlsoPrechecked(t *testing.T) {
	vm := vmFixture("fw-vm")
	p, api := startProbeFixture(t, vm, "1G")
	api.present["fw-vm"] = true
	api.states["fw-vm"] = domShutoff
	stubHugepageProbe(p, map[string]int{"1G": 0}, nil)

	err := p.RestartVM(context.Background(), "fw-vm")
	if err == nil || !strings.Contains(err.Error(), "大页不足") {
		t.Fatalf("restart 的 off→start 分支应同样预检：%v", err)
	}
	if len(api.started) != 0 {
		t.Fatalf("预检命中时不得启动：%v", api.started)
	}
}

// 陈旧 paused 域**不判**：它的大页已被自己持有（销毁后回到池里），按此刻的空闲页判
// 会把「自己的占用」算成「不够」，从而误拒决策 #455 的恢复路径（销毁→重定义→启动）。
// 这条是自审查抓到的边界，必须钉住。
func TestBug25StartSkipsStalePausedDomain(t *testing.T) {
	vm := vmFixture("fw-vm") // 1024MB / 1G ⇒ 需要 1 页
	p, api := startProbeFixture(t, vm, "1G")
	api.present["fw-vm"] = true
	api.states["fw-vm"] = domPaused
	api.reasons["fw-vm"] = pausedReasonStartingUp
	stubHugepageProbe(p, map[string]int{"1G": 0}, nil) // 空闲 0 页（那 1 页正被这个 paused 域持有）

	if err := p.StartVM(context.Background(), "fw-vm"); err != nil {
		t.Fatalf("陈旧 paused 域不得被大页预检拦住（它的页销毁后会回到池里）：%v", err)
	}
	if len(api.destroyed) != 1 || len(api.started) != 1 {
		t.Fatalf("应沿用决策 #455 的恢复路径（先销毁再启动）：destroyed=%v started=%v", api.destroyed, api.started)
	}
}

// 但**非 paused** 的关机域仍要判（上面那条例外不能放宽成「只要 exists 就不判」）。
func TestBug25StartStillJudgesShutoffDomain(t *testing.T) {
	vm := vmFixture("fw-vm")
	p, api := startProbeFixture(t, vm, "1G")
	api.present["fw-vm"] = true
	api.states["fw-vm"] = domShutoff
	stubHugepageProbe(p, map[string]int{"1G": 0}, nil)

	err := p.StartVM(context.Background(), "fw-vm")
	if err == nil || !strings.Contains(err.Error(), "大页不足") {
		t.Fatalf("已关机（shutoff）的域不持有大页，空闲不够时必须拦：%v", err)
	}
	if len(api.started) != 0 {
		t.Fatalf("命中时不得启动：%v", api.started)
	}
}

// 纯函数：判定逻辑与 IO 解耦（表驱动）。
func TestBug25ShortfallPureCases(t *testing.T) {
	cases := []struct {
		name    string
		page    string
		need    int
		free    int
		wantErr bool
	}{
		{"充足", "1G", 2, 2, false},
		{"多出来", "1G", 2, 3, false},
		{"缺一页", "1G", 2, 1, true},
		{"零空闲", "2M", 512, 0, true},
		{"未知页尺寸不判", "4K", 4, 0, false},
		{"需求为零不判", "1G", 0, 0, false},
		{"空闲未知（负数）不判", "1G", 1, -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := hugepageShortfall("fw-vm", tc.page, tc.need, tc.free)
			if (err != nil) != tc.wantErr {
				t.Fatalf("hugepageShortfall(%s, need=%d, free=%d) = %v，期望 wantErr=%v",
					tc.page, tc.need, tc.free, err, tc.wantErr)
			}
		})
	}
}

// SysfsHugepageProbe：按页尺寸读 sysfs，读不到回退 /proc/meminfo（仅当 meminfo 的默认页尺寸
// 与请求一致——否则是用别的池的数字冒充）。三条都不能靠宿主现场。
func TestBug25SysfsProbeReadsPerSizeAndFallsBack(t *testing.T) {
	root := t.TempDir()
	dir1G := filepath.Join(root, "hugepages-1048576kB")
	dir2M := filepath.Join(root, "hugepages-2048kB")
	for _, d := range []string{dir1G, dir2M} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("建夹具目录: %v", err)
		}
	}
	// sysfs 有 1G 池；2M 池目录不存在（模拟内核未提供该尺寸）。
	if err := os.WriteFile(filepath.Join(dir1G, "free_hugepages"), []byte("3\n"), 0o644); err != nil {
		t.Fatalf("写夹具: %v", err)
	}
	meminfo := filepath.Join(root, "meminfo")
	if err := os.WriteFile(meminfo, []byte("HugePages_Free:    700\nHugepagesize:       2048 kB\n"), 0o644); err != nil {
		t.Fatalf("写 meminfo 夹具: %v", err)
	}
	oldRoot, oldMem := hugepageSysfsRoot, hugepageMeminfoPath
	hugepageSysfsRoot, hugepageMeminfoPath = root, meminfo
	t.Cleanup(func() { hugepageSysfsRoot, hugepageMeminfoPath = oldRoot, oldMem })

	if n, ok := SysfsHugepageProbe("1G", 1); !ok || n != 3 {
		t.Fatalf("1G 应读到 sysfs 的 3 页：n=%d ok=%v", n, ok)
	}
	// 2M 无 sysfs ⇒ 回退 meminfo（默认页尺寸恰是 2048kB ⇒ 它是 2M 池的空闲数，可用）。
	if n, ok := SysfsHugepageProbe("2M", 512); !ok || n != 700 {
		t.Fatalf("2M 应回退 meminfo 的 700 页：n=%d ok=%v", n, ok)
	}
	// 未知页尺寸：不猜。
	if _, ok := SysfsHugepageProbe("4K", 1); ok {
		t.Fatal("未知页尺寸不应返回 ok")
	}
}

// meminfo 的默认页尺寸与请求不一致时**不得**拿它的 HugePages_Free 冒充（那是别的池）。
func TestBug25SysfsProbeRefusesMismatchedMeminfo(t *testing.T) {
	root := t.TempDir()
	meminfo := filepath.Join(root, "meminfo")
	if err := os.WriteFile(meminfo, []byte("HugePages_Free:    700\nHugepagesize:       2048 kB\n"), 0o644); err != nil {
		t.Fatalf("写 meminfo 夹具: %v", err)
	}
	oldRoot, oldMem := hugepageSysfsRoot, hugepageMeminfoPath
	hugepageSysfsRoot, hugepageMeminfoPath = root, meminfo // 无任何 sysfs 池目录
	t.Cleanup(func() { hugepageSysfsRoot, hugepageMeminfoPath = oldRoot, oldMem })

	if _, ok := SysfsHugepageProbe("1G", 1); ok {
		t.Fatal("meminfo 只反映默认尺寸（2M）池，不得用它回答 1G 的空闲页数")
	}
}
