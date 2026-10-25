package config

// BUG-25（v3.0.29 外部测试报告）守护：`set resource-pools hugepages page-size 2M count N` 提交后，
// 产品只提示「需 reboot 生效」——**不提示本机当下没有 2M 的 hugetlbfs 挂载点**。后果是真机
// 现场：重启后池建好了，但 libvirt 找不到 2M 挂载点，VM 起不来，报的是 libvirt 英文原文
// `Unable to find any usable hugetlbfs mount for 2048 KiB`。
//
// 判据（四条）：
//   - 声明 2M 而挂载表只有 1G → 告警必须**点名 2M**，并给出「重启后按 fstab 重建」与
//     「在此之前该池的 VNF 起不来（libvirt 报 …）」两段可行动信息；
//   - 反向（声明 1G、只有 2M）同样点名；
//   - 两尺寸都有挂载点 → **不产生该告警**（不制造噪声）；
//   - 挂载表读不到 → 如实说「无法核对」，不谎报通过。
//
// 另按决策 #459 的实测口径：内核在 /proc/mounts 里把 pagesize 规范化成「最大整数单位」
// （1G ⇒ `1024M`、2M ⇒ `2M`），故判定必须按解析出的 kB 值比较，不能按字面串比。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// stubHugepageHost 把挂载表/内核默认页尺寸换成夹具（返回 restore）。
// mounts 为空串表示「文件不存在/读不到」。
func stubHugepageHost(t *testing.T, mounts, meminfo string) {
	t.Helper()
	dir := t.TempDir()
	mountsPath := filepath.Join(dir, "mounts")
	meminfoPath := filepath.Join(dir, "meminfo")
	if mounts != "" {
		if err := os.WriteFile(mountsPath, []byte(mounts), 0o644); err != nil {
			t.Fatalf("写 mounts 夹具: %v", err)
		}
	}
	if meminfo != "" {
		if err := os.WriteFile(meminfoPath, []byte(meminfo), 0o644); err != nil {
			t.Fatalf("写 meminfo 夹具: %v", err)
		}
	}
	oldMounts, oldMeminfo := hugepageMountsPath, hugepageMeminfoPath
	hugepageMountsPath, hugepageMeminfoPath = mountsPath, meminfoPath
	t.Cleanup(func() { hugepageMountsPath, hugepageMeminfoPath = oldMounts, oldMeminfo })
}

const meminfoDefault2M = "MemTotal:       16000000 kB\nHugepagesize:       2048 kB\n"

// mountsOnly1G 内核规范化形式：1G 写作 1024M（决策 #459 真机实测值）。
const mountsOnly1G = "proc /proc proc rw 0 0\n" +
	"nodev /dev/hugepages hugetlbfs rw,relatime,pagesize=1024M 0 0\n"

const mountsOnly2M = "nodev /dev/hugepages-2M hugetlbfs rw,relatime,pagesize=2M 0 0\n"

const mountsBoth = mountsOnly1G + mountsOnly2M

// commitWithPools 在 engineKit 上提交一次「声明这些大页池」的候选配置。
func commitWithPools(t *testing.T, pools []model.HPool) []string {
	t.Helper()
	k := newEngineKit(t)
	k.edit(t, "admin", "console")
	cfg := baseCommitted()
	cfg.ResourcePools = &model.ResourcePool{Hugepages: pools}
	if err := k.engine.UpdateCandidate(Session{User: "admin", Source: "console"}, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	res, err := k.engine.Commit(context.Background(), Session{User: "admin", Source: "console"}, CommitOpts{})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return res.Warnings
}

// warningsWith 返回包含全部 sub 的告警行（用于定位本缺陷的那一条，不受其它告警干扰）。
func warningsWith(ws []string, subs ...string) []string {
	var out []string
	for _, w := range ws {
		ok := true
		for _, s := range subs {
			if !strings.Contains(w, s) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, w)
		}
	}
	return out
}

// 声明 2M、本机只挂了 1G：必须点名 2M，并给出「重启后按 fstab 重建」与
// 「在此之前该池的 VNF 起不来（libvirt 原文）」——只报「需 reboot 生效」等于没说。
func TestBug25CommitWarnsMissing2MMount(t *testing.T) {
	stubHugepageHost(t, mountsOnly1G, meminfoDefault2M)
	ws := commitWithPools(t, []model.HPool{{PageSize: "2M", Count: 512}})

	hits := warningsWith(ws, "2M", "hugetlbfs")
	if len(hits) != 1 {
		t.Fatalf("声明 2M 而无 2M 挂载点必须给一条点名 2M 的告警；实得 %+v", ws)
	}
	msg := hits[0]
	for _, want := range []string{"fstab", "重启", "VNF", "unable to find any usable hugetlbfs mount"} {
		if !strings.Contains(msg, want) {
			t.Errorf("告警应含 %q：%s", want, msg)
		}
	}
	// 1G 有挂载点：不得连带报 1G 缺挂载点。
	if len(warningsWith(ws, "1G", "hugetlbfs")) != 0 {
		t.Fatalf("1G 已挂载，不得报缺挂载点：%+v", ws)
	}
}

// 反向：声明 1G、本机只挂了 2M。
func TestBug25CommitWarnsMissing1GMount(t *testing.T) {
	stubHugepageHost(t, mountsOnly2M, "Hugepagesize:       2048 kB\n")
	ws := commitWithPools(t, []model.HPool{{PageSize: "1G", Count: 2}})

	hits := warningsWith(ws, "1G", "hugetlbfs")
	if len(hits) != 1 {
		t.Fatalf("声明 1G 而无 1G 挂载点必须给一条点名 1G 的告警；实得 %+v", ws)
	}
	if len(warningsWith(ws, "2M", "hugetlbfs")) != 0 {
		t.Fatalf("2M 已挂载，不得报缺挂载点：%+v", ws)
	}
}

// 两个页尺寸都有挂载点：**不产生**该告警（不制造噪声）。
func TestBug25CommitSilentWhenAllMountsPresent(t *testing.T) {
	stubHugepageHost(t, mountsBoth, meminfoDefault2M)
	ws := commitWithPools(t, []model.HPool{{PageSize: "1G", Count: 2}, {PageSize: "2M", Count: 512}})

	if hits := warningsWith(ws, "hugetlbfs"); len(hits) != 0 {
		t.Fatalf("挂载点齐备时不得提示缺挂载点：%+v", hits)
	}
	// 「需 reboot 生效」仍然在（那是另一条既有提示，本修复不取代它）。
	if !warningsContain(ws, "需 reboot 生效") {
		t.Fatalf("resource-pools 变更的既有 reboot 提示不得丢失：%+v", ws)
	}
}

// 挂载表读不到：如实说「无法核对」，不能当作「已覆盖」静默通过。
func TestBug25CommitSaysUnverifiableWhenMountsUnreadable(t *testing.T) {
	stubHugepageHost(t, "", meminfoDefault2M) // 文件不存在
	ws := commitWithPools(t, []model.HPool{{PageSize: "2M", Count: 512}})

	hits := warningsWith(ws, "无法核对")
	if len(hits) != 1 {
		t.Fatalf("读不到挂载表必须如实说「无法核对」；实得 %+v", ws)
	}
	if !strings.Contains(hits[0], "2M") {
		t.Errorf("「无法核对」应点名涉及的页尺寸：%s", hits[0])
	}
}

// 未标注 pagesize 的挂载点按**内核默认页尺寸**归属（/proc/meminfo 的 Hugepagesize）。
// 这是 /dev/hugepages 的常见形态：不写 pagesize= 即内核默认尺寸池。
func TestBug25UnlabeledMountUsesKernelDefaultSize(t *testing.T) {
	const mountsUnlabeled = "nodev /dev/hugepages hugetlbfs rw,relatime 0 0\n"
	stubHugepageHost(t, mountsUnlabeled, meminfoDefault2M)
	if ws := commitWithPools(t, []model.HPool{{PageSize: "2M", Count: 512}}); len(warningsWith(ws, "hugetlbfs")) != 0 {
		t.Fatalf("未标注 pagesize 的挂载点应按内核默认 2M 归属（不得报 2M 缺挂载点）：%+v", ws)
	}
}

// 内核规范化写法（1G ⇒ pagesize=1024M）必须按 kB 比较，不能按字面串比。
func TestBug25NormalizedPagesizeCounts(t *testing.T) {
	stubHugepageHost(t, mountsOnly1G, meminfoDefault2M)
	if ws := commitWithPools(t, []model.HPool{{PageSize: "1G", Count: 2}}); len(warningsWith(ws, "hugetlbfs")) != 0 {
		t.Fatalf("pagesize=1024M 应算 1G 已覆盖：%+v", ws)
	}
}

// 纯函数口径：解析与缺项判定可独立测（提交路径只是把它接上去）。
func TestBug25MountParsingPure(t *testing.T) {
	st := parseHugepageMounts(mountsOnly1G, 2048)
	if len(st.mountedKB) != 1 || st.mountedKB[0] != 1048576 {
		t.Fatalf("pagesize=1024M 应解析成 1048576kB：%+v", st)
	}
	if got := missingHugepageMounts([]string{"1G", "2M"}, st.mountedKB); len(got) != 1 || got[0] != "2M" {
		t.Fatalf("缺项应只有 2M：%+v", got)
	}
	// 挂载点为空：两个尺寸都缺。
	if got := missingHugepageMounts([]string{"1G", "2M"}, nil); len(got) != 2 {
		t.Fatalf("无任何挂载点时应报两个都缺：%+v", got)
	}
	// 未知页尺寸不参与判定（模型校验已拦；这里防御空/异常值不制造噪声）。
	if got := missingHugepageMounts([]string{"", "4K"}, nil); len(got) != 0 {
		t.Fatalf("未知页尺寸不应产生缺项：%+v", got)
	}
}
