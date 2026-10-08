package api

// 决策 #329：大页池读视图与回收的 api 层测试。
//
// 核心纯函数（回收量/在用页不动/回读不一致）在 internal/system 的单测里逐条覆盖；
// 这里覆盖**装配与三面同源**：声明值派生、占用者证据、CLI show/request、REST 两端点、
// super-user 门禁与审计。sysfs 用临时根（Options.HugepageRoot），绝不动真机。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
	ksys "github.com/xzjt/nfvis/internal/system"
)

// writeHugepageFixture 在临时根下造 sysfs（与 internal/system 的单测同构）。
func writeHugepageFixture(t *testing.T, root, pageSize string, nr, free int) {
	t.Helper()
	kb, ok := ksys.HugepagePageKB(pageSize)
	if !ok {
		t.Fatalf("未知页尺寸 %s", pageSize)
	}
	dir := filepath.Join(root, "sys", "kernel", "mm", "hugepages", fmt.Sprintf("hugepages-%dkB", kb))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	for name, v := range map[string]int{"nr_hugepages": nr, "free_hugepages": free} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strconv.Itoa(v)), 0o644); err != nil {
			t.Fatalf("写 %s: %v", name, err)
		}
	}
}

// writeHugepageSmaps 造一个 /proc/<pid>/smaps，含一个 1G hugetlb 映射（pages1G 页）。
func writeHugepageSmaps(t *testing.T, root, pid string, pages1G int) {
	t.Helper()
	dir := filepath.Join(root, "proc", pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建 /proc/%s: %v", pid, err)
	}
	block := fmt.Sprintf("7f0000000000-7f0040000000 rw-s 00000000 00:0d 42 /dev/hugepages/1-sem-vm/pc.ram\n"+
		"Size: %d kB\nKernelPageSize: 1048576 kB\nMMUPageSize: 1048576 kB\n"+
		"VmFlags: rd wr sh mr mw me ms de ht pf io\n", pages1G*1048576)
	if err := os.WriteFile(filepath.Join(dir, "smaps"), []byte(block), 0o644); err != nil {
		t.Fatalf("写 smaps: %v", err)
	}
}

// writeHugepageComm 造 /proc/<pid>/comm（决策 #353：dataplane 归属按 comm=vpp 判定）。
func writeHugepageComm(t *testing.T, root, pid, comm string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "proc", pid, "comm"), []byte(comm+"\n"), 0o644); err != nil {
		t.Fatalf("写 /proc/%s/comm: %v", pid, err)
	}
}

func TestHugepageDeclaredFromConfig(t *testing.T) {
	cfg := model.Config{ResourcePools: &model.ResourcePool{
		Hugepages: []model.HPool{{PageSize: "1G", Count: 2}, {PageSize: "2M", Count: 768}},
	}}
	d := hugepageDeclared(cfg)
	if d["1G"] != 2 || d["2M"] != 768 {
		t.Fatalf("声明值派生不符：%+v", d)
	}
	// 只声明 2M 时，1G 不托管（-1），2M 取声明值。
	cfg2 := model.Config{ResourcePools: &model.ResourcePool{Hugepages: []model.HPool{{PageSize: "2M", Count: 64}}}}
	d2 := hugepageDeclared(cfg2)
	if d2["1G"] > 0 {
		t.Errorf("未声明 1G 时不该给出正声明值：%+v", d2)
	}
	if d2["2M"] != 64 {
		t.Errorf("2M 声明值 = %d，期望 64", d2["2M"])
	}
}

func TestHugepageOccupantsEvidence(t *testing.T) {
	cfg := model.Config{
		ResourcePools: &model.ResourcePool{Hugepages: []model.HPool{{PageSize: "1G", Count: 4}}},
		Vpp:           &model.VppConfig{Memory: &model.VppMemory{HugepagePreference: "2M"}},
	}
	cfg.VirtualMachineFunctions = []model.VMFunction{
		{Name: "vnf-a", Memory: model.VMMemory{SizeMB: 2048}},
	}
	got := strings.Join(hugepageOccupants(cfg, "1G", model.DataPlaneVPP), "\n")
	if !strings.Contains(got, "vnf-a") || !strings.Contains(got, "2 页") {
		t.Errorf("1G 占用者证据应列出 vnf-a 与其页数：%s", got)
	}
	// 2M 池：VPP 偏好 2M → 应出现 VPP；不应出现用 1G 的 vnf-a。
	got2M := strings.Join(hugepageOccupants(cfg, "2M", model.DataPlaneVPP), "\n")
	if !strings.Contains(got2M, "VPP") {
		t.Errorf("2M 占用者证据应包含 VPP：%s", got2M)
	}
	if strings.Contains(got2M, "vnf-a") {
		t.Errorf("2M 池不该把用 1G 的 VNF 算进来：%s", got2M)
	}
	// 内核数据面：VPP 不在场（也不吃大页池），占用者证据不得把它列成占用者
	// （R2-18；说明里可以出现"未使用 VPP"的字样，判据认的是「VPP 数据面（…）」这条证据）。
	gotK := strings.Join(hugepageOccupants(cfg, "2M", model.DataPlaneKernel), "\n")
	if strings.Contains(gotK, "VPP 数据面（") {
		t.Errorf("内核数据面下不得把 VPP 列为占用者：%s", gotK)
	}
	if !strings.Contains(gotK, "Linux 内核网络") {
		t.Errorf("内核数据面下应如实说明数据面：%s", gotK)
	}
}

// commitHugepagePool 经 CLI 提交一份 1G 大页池声明，并退出配置模式
// （不回操作模式的话，后续 show/request 会被当成配置模式内的语句）。
func commitHugepagePool(t *testing.T, x *cliExecutor, size string, count int) {
	t.Helper()
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure", fmt.Sprintf("set resource-pools hugepages page-size %s count %d", size, count), "commit", "exit")
}

func TestCLIShowSystemHugepagesThreeWay(t *testing.T) {
	x, _ := newCLIKit(t)
	root := t.TempDir()
	x.hugepageRoot = root
	writeHugepageFixture(t, root, "1G", 4, 2) // 声明 2、实际 4、空闲 2 → 可回收 2
	commitHugepagePool(t, x, "1G", 2)

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system hugepages").Output
	for _, want := range []string{"页尺寸", "声明", "内核实际", "在用", "实际持有", "数据面占用",
		"无主占用", "空闲（可分配）", "可回收", "request system hugepages reclaim"} {
		if !strings.Contains(out, want) {
			t.Errorf("show system hugepages 输出缺少 %q：\n%s", want, out)
		}
	}
	// 三方数字真的发得出来：1G 行应出现 声明 2 / 实际 4 / 在用 2 / 可回收 2。
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "1G") {
			line = l
			break
		}
	}
	for _, want := range []string{"2", "4"} {
		if !strings.Contains(line, want) {
			t.Errorf("1G 行缺少数字 %q：%q", want, line)
		}
	}
	if x.structured == nil {
		t.Error("show system hugepages 应给出结构化输出（display json 用）")
	}
}

// 决策 #353：1G 池「数据面固定占用」进 CLI 读视图——列有「数据面占用」，
// 说明区给出「VNF 可起页数 = 空闲页数」（数据源：/proc/<pid>/comm == vpp）。
func TestCLIShowSystemHugepagesDataplaneNote(t *testing.T) {
	x, _ := newCLIKit(t)
	root := t.TempDir()
	x.hugepageRoot = root
	writeHugepageFixture(t, root, "1G", 2, 0) // 实际 2、空闲 0
	writeHugepageSmaps(t, root, "100", 1)     // vpp 主堆持有 1 页
	writeHugepageComm(t, root, "100", "vpp")
	commitHugepagePool(t, x, "1G", 2)

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system hugepages").Output
	if strings.Contains(out, "%%") {
		t.Fatalf("show 不该失败：%s", out)
	}
	// 1G 行（表内）「数据面占用」列应为 1；2M 池无进程持有 → 0。
	if !strings.Contains(out, "数据面（VPP 主堆）固定占用 1 页（实测；不可配置释放）——VNF 可起页数 = 空闲页数（当前 0）") {
		t.Fatalf("说明区应给出数据面固定占用与可起 VNF 页数：\n%s", out)
	}
	// 结构化输出同样带新字段（供 display json 与 REST 同源消费）。
	if x.structured == nil {
		t.Fatal("应给出结构化输出")
	}
	raw, err := json.Marshal(x.structured)
	if err != nil {
		t.Fatalf("结构化输出不可序列化: %v", err)
	}
	if !strings.Contains(string(raw), `"held_by_dataplane":1`) {
		t.Fatalf("结构化输出应带 held_by_dataplane=1：%s", raw)
	}
}

func TestCLIRequestHugepagesReclaim(t *testing.T) {
	x, _ := newCLIKit(t)
	root := t.TempDir()
	x.hugepageRoot = root
	x.setHugepages(ksys.SysfsHugepageSetter{Root: root})
	writeHugepageFixture(t, root, "1G", 4, 2)
	commitHugepagePool(t, x, "1G", 2)

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request system hugepages reclaim").Output
	if strings.Contains(out, "%%") {
		t.Fatalf("回收应成功，实得：%s", out)
	}
	if !strings.Contains(out, "已回收 2 页") {
		t.Errorf("输出应说明回收了 2 页：%s", out)
	}
	// 独立事实源：回读 sysfs 确认池真的降到声明值。
	nr, _, _ := ksys.ReadHugepagePool(root, "1G")
	if nr != 2 {
		t.Fatalf("回读 1G nr = %d，期望 2（回收没落到内核）", nr)
	}
	// 审计留痕。
	trail, err := x.engine.AuditTrail(50, 0)
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	found := false
	for _, e := range trail {
		if e.Action == "system.hugepages.reclaim" && e.Result == "success" {
			found = true
		}
	}
	if !found {
		t.Error("回收动作应写审计（system.hugepages.reclaim）")
	}
}

func TestCLIRequestHugepagesReclaimBlockedNoFail(t *testing.T) {
	x, _ := newCLIKit(t)
	root := t.TempDir()
	x.hugepageRoot = root
	x.setHugepages(ksys.SysfsHugepageSetter{Root: root})
	writeHugepageFixture(t, root, "1G", 4, 0) // 多余 2 页全在用
	commitHugepagePool(t, x, "1G", 2)

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request system hugepages reclaim").Output
	if strings.Contains(out, "%%") {
		t.Fatalf("「在用页挡住」是如实结论、不是命令失败：%s", out)
	}
	if !strings.Contains(out, "无法回收") {
		t.Errorf("应如实说明无法回收：%s", out)
	}
	// 在用页一律不动。
	if nr, _, _ := ksys.ReadHugepagePool(root, "1G"); nr != 4 {
		t.Fatalf("被挡住时不该改池，实得 nr=%d", nr)
	}
}

func TestCLIRequestHugepagesReclaimUnavailable(t *testing.T) {
	x, _ := newCLIKit(t)
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request system hugepages reclaim").Output
	if !strings.Contains(out, "%%") || !strings.Contains(out, "未装配") {
		t.Fatalf("未装配写能力时应明确报不可用：%s", out)
	}
}

// ---------- REST ----------

func TestRESTHugepagesReadAndReclaim(t *testing.T) {
	root := t.TempDir()
	writeHugepageFixture(t, root, "1G", 4, 2)
	ts := newTestServerOpts(t, Options{
		Hugepages:    ksys.SysfsHugepageSetter{Root: root},
		HugepageRoot: root,
	})
	token := loginAdmin(t, ts)
	// 声明 1G=2（提交后生效）。
	status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/resource-pools", token,
		map[string]any{"hugepages": []map[string]any{{"page_size": "1G", "count": 2}}},
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("声明大页池: %d %s", status, body)
	}

	// GET：三方数字。
	status, _, body = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/hugepages", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /system/hugepages: %d %s", status, body)
	}
	var view struct {
		Pools []struct {
			PageSize    string `json:"page_size"`
			Declared    int    `json:"declared"`
			Actual      int    `json:"actual"`
			InUse       int    `json:"in_use"`
			Reclaimable int    `json:"reclaimable"`
			State       string `json:"state"`
		} `json:"pools"`
		Reclaimable int `json:"reclaimable"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("解析响应: %v %s", err, body)
	}
	if len(view.Pools) != 2 {
		t.Fatalf("pools 应恒为两个页尺寸，实得 %d", len(view.Pools))
	}
	p1 := view.Pools[0]
	if p1.PageSize != "1G" || p1.Declared != 2 || p1.Actual != 4 || p1.InUse != 2 || p1.Reclaimable != 2 || p1.State != ksys.HugepageStateSurplus {
		t.Fatalf("1G 三方数字不符：%+v", p1)
	}
	if view.Reclaimable != 2 {
		t.Fatalf("合计可回收 = %d，期望 2", view.Reclaimable)
	}

	// POST reclaim：已回收并回读确认。
	status, _, body = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/hugepages:reclaim", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("POST reclaim: %d %s", status, body)
	}
	if !strings.Contains(string(body), `"action":"reclaimed"`) || !strings.Contains(string(body), `"reclaimed":2`) {
		t.Errorf("响应应报 1G 已回收 2 页：%s", body)
	}
	if nr, _, _ := ksys.ReadHugepagePool(root, "1G"); nr != 2 {
		t.Fatalf("回读 1G nr = %d，期望 2", nr)
	}
}

func TestRESTHugepagesReclaimRequiresSuperUser(t *testing.T) {
	root := t.TempDir()
	writeHugepageFixture(t, root, "1G", 4, 2)
	ts := newTestServerOpts(t, Options{
		Hugepages:    ksys.SysfsHugepageSetter{Root: root},
		HugepageRoot: root,
	})
	vt := loginViewer(t, ts)
	// 读：read-only 可读。
	status, _, _ := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/hugepages", vt, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("read-only 应可读大页池读视图：%d", status)
	}
	// 写：read-only 被拒（class 与 request system kernel apply 同档 = super-user）。
	status, _, body := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/hugepages:reclaim", vt, nil, nil)
	if status != http.StatusForbidden {
		t.Fatalf("read-only 调回收应 403，实得 %d %s", status, body)
	}
	if nr, _, _ := ksys.ReadHugepagePool(root, "1G"); nr != 4 {
		t.Fatalf("被拒时不该改池，实得 nr=%d", nr)
	}
}

func TestRESTHugepagesReclaimUnavailable(t *testing.T) {
	ts := newTestServer(t) // 未注入 Hugepages → 503
	token := loginAdmin(t, ts)
	status, _, body := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/hugepages:reclaim", token, nil, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("未装配写能力应 503，实得 %d %s", status, body)
	}
	if !strings.Contains(string(body), "RUNTIME_UNAVAILABLE") {
		t.Fatalf("错误码应为 RUNTIME_UNAVAILABLE：%s", body)
	}
}

// 决策 #346 端到端：内核实际 == 声明、但有一页无主占用（round125 现场形态）——
// 读视图如实报 state=orphan/orphan=1（旧 #329 口径下这里四列全「一致」），
// 但 POST reclaim **不回收无主页**（真机实测：写 nr_hugepages 释放不了这类预留页）：
// 不写、如实报无可回收、池与空闲数不变，**绝不报成功**。
func TestRESTHugepagesOrphanReadNotReclaimed(t *testing.T) {
	root := t.TempDir()
	writeHugepageFixture(t, root, "1G", 2, 0) // 实际 2、空闲 0：在用 2
	writeHugepageSmaps(t, root, "555", 1)     // 进程只持有 1 页 → 1 页无主占用
	ts := newTestServerOpts(t, Options{Hugepages: ksys.SysfsHugepageSetter{Root: root}, HugepageRoot: root})
	token := loginAdmin(t, ts)

	// 声明 1G=2（== 内核实际）。
	status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/resource-pools", token,
		map[string]any{"hugepages": []map[string]any{{"page_size": "1G", "count": 2}}},
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("声明大页池: %d %s", status, body)
	}

	// GET：1G 池应如实报 held=1 / orphan=1 / state=orphan / reclaimable=0（无主页不可回收）。
	status, _, body = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/hugepages", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /system/hugepages: %d %s", status, body)
	}
	type poolView struct {
		PageSize        string `json:"page_size"`
		Held            int    `json:"held"`
		HeldByDataplane int    `json:"held_by_dataplane"`
		Orphan          int    `json:"orphan"`
		InUse           int    `json:"in_use"`
		Reclaimable     int    `json:"reclaimable"`
		State           string `json:"state"`
	}
	var view struct {
		Pools []poolView `json:"pools"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("解析响应: %v %s", err, body)
	}
	var p1 *poolView
	for i := range view.Pools {
		if view.Pools[i].PageSize == "1G" {
			p1 = &view.Pools[i]
		}
	}
	if p1 == nil {
		t.Fatalf("响应里没有 1G 池：%s", body)
	}
	if p1.InUse != 2 || p1.Held != 1 || p1.Orphan != 1 || p1.State != ksys.HugepageStateOrphan || p1.Reclaimable != 0 {
		t.Fatalf("1G 无主占用读视图不符（reclaimable 应为 0）：%+v", *p1)
	}
	// 决策 #353：该 fixture 的进程没写 comm → 按非数据面计，held_by_dataplane 如实为 0。
	if p1.HeldByDataplane != 0 {
		t.Fatalf("comm 读不到的进程应按非数据面计（held_by_dataplane=0），实得 %d：%+v", p1.HeldByDataplane, *p1)
	}

	// POST reclaim：无主页不在回收范围 → 不动作、如实报无可回收、绝不报成功。
	status, _, body = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/hugepages:reclaim", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("POST reclaim: %d %s", status, body)
	}
	if strings.Contains(string(body), `"action":"reclaimed"`) || strings.Contains(string(body), `"reclaimed":1`) {
		t.Errorf("无主页不可回收，绝不报成功：%s", body)
	}
	if !strings.Contains(string(body), `"reclaimed":0`) {
		t.Errorf("应如实报回收 0 页：%s", body)
	}
	// 独立事实源：池与空闲数完全没变（写 nr 也释放不了无主页）。
	if nr, free, _ := ksys.ReadHugepagePool(root, "1G"); nr != 2 || free != 0 {
		t.Fatalf("回读 = nr %d free %d，期望 nr 2 free 0（无变化）", nr, free)
	}
}
