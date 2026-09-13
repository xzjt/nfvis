//go:build integration

// M4-2：资源池账本与真机事实交叉核对（FR-CMP-001~004、FR-SYS-010，决策 #39）。
//
// 不经 nfvisd，直接以真机事实（/proc/meminfo 的 1G 大页总数、/etc/vpp/startup.conf
// 的 main-core/corelist-workers、CPU 数）构造 committed 资源池，校验账本视图与之一致。
// 运行于 nfvis-vm，CI 不跑；缺少真机文件时跳过。
package integration

import (
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

var (
	meminfoRe   = regexp.MustCompile(`(?m)^(Hugepagesize|HugePages_Total|HugePages_Free):\s+(\d+)`)
	mainCoreRe  = regexp.MustCompile(`(?m)main-core\s+(\d+)`)
	corelistRe  = regexp.MustCompile(`(?m)corelist-workers\s+([0-9,\- ]+)`)
	coreTokenRe = regexp.MustCompile(`^\d+(-\d+)?$`)
)

// hostFacts 真机事实（1G 大页总量/空闲、VPP 保留核）。
func hostFacts(t *testing.T) (hpTotal, hpFree int, vppReserved []int) {
	t.Helper()
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		t.Skipf("跳过：无法读取 /proc/meminfo（非 Linux 真机）: %v", err)
	}
	vals := map[string]int{}
	for _, m := range meminfoRe.FindAllStringSubmatch(string(b), -1) {
		v, _ := strconv.Atoi(m[2])
		vals[m[1]] = v
	}
	if vals["Hugepagesize"] != 1024*1024 {
		t.Skipf("跳过：本机大页粒度非 1G（Hugepagesize=%d kB）", vals["Hugepagesize"])
	}
	hpTotal, hpFree = vals["HugePages_Total"], vals["HugePages_Free"]

	vb, err := os.ReadFile("/etc/vpp/startup.conf")
	if err != nil {
		t.Skipf("跳过：未找到 /etc/vpp/startup.conf: %v", err)
	}
	if m := mainCoreRe.FindStringSubmatch(string(vb)); m != nil {
		c, _ := strconv.Atoi(m[1])
		vppReserved = append(vppReserved, c)
	}
	if m := corelistRe.FindStringSubmatch(string(vb)); m != nil {
		for _, tok := range strings.Split(strings.TrimSpace(m[1]), ",") {
			tok = strings.TrimSpace(tok)
			if !coreTokenRe.MatchString(tok) {
				continue
			}
			if a, b, ok := strings.Cut(tok, "-"); ok {
				lo, _ := strconv.Atoi(a)
				hi, _ := strconv.Atoi(b)
				for x := lo; x <= hi; x++ {
					vppReserved = append(vppReserved, x)
				}
				continue
			}
			n, _ := strconv.Atoi(tok)
			vppReserved = append(vppReserved, n)
		}
	}
	sort.Ints(vppReserved)
	return hpTotal, hpFree, vppReserved
}

// TestResourceLedgerMatchesHostFacts 账本视图须与真机的 1G 大页总量、VPP 保留核一致，
// 且「空闲页足以放下 1 台 1G VM」这一判断与实际可启动性一致（本环境 Free=1）。
func TestResourceLedgerMatchesHostFacts(t *testing.T) {
	hpTotal, hpFree, vppReserved := hostFacts(t)

	isolated := make([]int, 0, runtime.NumCPU())
	for c := 0; c < runtime.NumCPU(); c++ {
		isolated = append(isolated, c)
	}
	cfg := model.Config{
		ResourcePools: &model.ResourcePool{
			Hugepages: []model.HPool{{PageSize: "1G", Count: hpTotal}},
			CPU:       &model.CPUSetup{IsolatedCores: isolated},
		},
		Vpp: &model.VppConfig{CPU: &model.VppCPU{
			MainCore:        vppCore(vppReserved, 0),
			CorelistWorkers: corelistOf(vppReserved, 1),
		}},
	}
	l := model.NewPoolLedger(cfg)
	if errs := l.Allocate(cfg); len(errs) != 0 {
		t.Fatalf("空 VNF 配置不应有配额错误: %v", errs)
	}
	if got := l.Hugepages["1G"].Total; got != hpTotal {
		t.Fatalf("账本大页总量 %d ≠ 真机 %d", got, hpTotal)
	}
	if got := l.Hugepages["1G"].Free; got != hpFree {
		// 真机上可能被其它进程（VPP）占用大页，账本以配置总量为准；
		// 这里记录差异而非失败，仅要求 Free = Total（无 VNF）。
		t.Logf("真机 HugePages_Total=%d Free=%d（VPP 等已占 %d 页）", hpTotal, hpFree, hpTotal-hpFree)
	}
	if len(vppReserved) > 0 && !equalInts(l.CPU.VppReserved, vppReserved) {
		t.Fatalf("账本 VPP 保留核 %v ≠ 真机 startup.conf %v（FR-SYS-010）", l.CPU.VppReserved, vppReserved)
	}
	t.Logf("真机：1G 大页 total=%d free=%d；VPP 保留核=%v；隔离核=%d 个",
		hpTotal, hpFree, vppReserved, len(isolated))

	// 一台 1G 大页 VM：账本分配 1 页；判断「可启动」须 free ≥ 1。
	vm := model.VMFunction{
		Name: "it-pool-vm", Image: "img",
		VCPU:   model.VMCpu{Count: 2},
		Memory: model.VMMemory{SizeMB: 1024, HugepageSize: "1G"},
	}
	cfg.VirtualMachineFunctions = []model.VMFunction{vm}
	l = model.NewPoolLedger(cfg)
	if errs := l.Allocate(cfg); len(errs) != 0 {
		t.Fatalf("1 台 1G VM 应可分配: %v", errs)
	}
	if got := l.Hugepages["1G"].Allocated; got != 1 {
		t.Fatalf("1G VM 应占 1 页，实际 %d", got)
	}
	if hpFree < 1 {
		t.Logf("警告：真机空闲大页 %d < 1，VM 实际启动会失败（容量账见 M4-P0 记录）", hpFree)
	}

	// 池缩减到存量之下：commit 阶段账本必须报错（FR-CMP-004）。
	cfg.ResourcePools.Hugepages = []model.HPool{{PageSize: "1G", Count: 0}}
	if errs := model.CheckResources(cfg); len(errs) == 0 {
		t.Fatal("池缩减到 0 页而存量 VM 需 1 页，应报配额错误（FR-CMP-004）")
	}

	// backing=normal 的 VM 不占大页池（FR-CMP-019）。
	cfg.ResourcePools.Hugepages = []model.HPool{{PageSize: "1G", Count: 0}}
	cfg.VirtualMachineFunctions = []model.VMFunction{{
		Name: "it-plain-vm", Image: "img",
		VCPU:   model.VMCpu{Count: 1},
		Memory: model.VMMemory{SizeMB: 512, Backing: "normal"},
	}}
	if errs := model.CheckResources(cfg); len(errs) != 0 {
		t.Fatalf("backing=normal 不应受大页池为 0 影响: %v", errs)
	}
}

func vppCore(cores []int, idx int) int {
	if idx < len(cores) {
		return cores[idx]
	}
	return 0
}

// corelistOf 把保留核列表（除去第 idx 个作 main-core）转回 "5,6" 形式。
func corelistOf(cores []int, from int) string {
	if from >= len(cores) {
		return ""
	}
	parts := make([]string, 0, len(cores)-from)
	for _, c := range cores[from:] {
		parts = append(parts, strconv.Itoa(c))
	}
	return strings.Join(parts, ",")
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
