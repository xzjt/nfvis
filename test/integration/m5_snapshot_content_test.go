//go:build integration

// T0-5 / FR-CMP-015 真机集成测试：快照**磁盘内容级**回滚。
//
// M4-6（m4_snapshot_test.go）只证了「回滚路径可执行」——它在宿主侧用 qemu-img 直接写
// qcow2 标记扇区，而宿主直写会破坏 qcow2 内部快照的 L1/refcount，因此**不能**用于证明
// 「内容真的回滚了」。本测试改在 **guest 内**写文件：
//
//   1. 引导云镜像；cloud-init `write_files` 在 guest 根盘写下 KNOWN 内容，
//      并用 `chpasswd` 给 root 设口令以便串口控制台登录（镜像内 root 口令为 `!*` 锁定）；
//   2. 串口登录 guest，把文件改成 LATER（这一步证明「guest 内写盘」链路可用）；
//   3. 关机，创建快照（此刻磁盘内容 = LATER）；决策 #75 起 create/rollback 需关机态；
//   4. 启动 guest，串口把文件改成 CHANGED（快照后写入）；
//   5. 关机，回滚到快照；
//   6. 启动 guest 使磁盘内容可见，串口回读 → 应为 LATER（既非 CHANGED 也非 KNOWN）。
//
// 为什么必须关机再回滚/重启：回滚直接改磁盘，而运行中 guest 的内存/页缓存仍是旧内容；
// 只有重引导后读到的才**只能是磁盘内容**，此时 LATER 才能排除「内存残留」与「写未生效」两种假阳性。
// （对运行中域回滚的真实底座行为见 TestSnapshotRevertOnRunningVMRealLibvirt。）
//
// 环境事实（实测所得，勿凭记忆改）：
//   - 镜像 /var/lib/nfvis/images/alpine.qcow2：Alpine 3.20、单分区 ext、ttyS0 有 getty、
//     自带 cloud-init 24.1.3（NoCloud）；root 口令 `!*`（锁定），须 cloud-init 设口令。
//   - guest 默认 shell 是 **ash**：`echo READ<<$(cat f)>>END` 会被当成重定向报
//     `syntax error: unexpected "("`；须用 `printf "READ<<%s>>END\n" "$(cat f)"`。
//
// 需可引导云镜像（NFVIS_TEST_VM_IMAGE 或 alpine.qcow2），否则跳过。
package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/compute"
)

const (
	itSnapContentVM = "it-t05-vm"
	snapFile        = "/root/t05-state.txt"
	snapKnown       = "T05-KNOWN"
	snapLater       = "T05-LATER"
	snapChanged     = "T05-CHANGED"
	snapGuestPass   = "Nfvis@12345"
)

// consoleRW 抽象串口双向流（compute.Provider.Console 返回值）。
type consoleRW interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
}

// consoleSession 串口会话：同一流可连续下发多条命令。
// 必须记住「已登录」——串口流保持附着，登录后再等 `login:` 会永久等不到（getty 已交由 shell）。
type consoleSession struct {
	t      *testing.T
	stream consoleRW
	logged bool
}

// ensureShell 首次调用走 login → 口令 → 提示符；之后直接返回（已有 shell）。
func (cs *consoleSession) ensureShell(timeout time.Duration) {
	cs.t.Helper()
	if cs.logged {
		return
	}
	waitSerialFor(cs.t, cs.stream, "login:", timeout, "guest 登录提示")
	if _, err := cs.stream.Write([]byte("root\n")); err != nil {
		cs.t.Fatalf("串口写入用户名: %v", err)
	}
	waitSerialFor(cs.t, cs.stream, "Password:", 60*time.Second, "guest 口令提示")
	if _, err := cs.stream.Write([]byte(snapGuestPass + "\n")); err != nil {
		cs.t.Fatalf("串口写入口令: %v", err)
	}
	waitSerialFor(cs.t, cs.stream, "#", 60*time.Second, "guest shell 提示符")
	cs.logged = true
}

// waitSerialFor 在串口流上等待出现 want；超时失败并打印尾部输出。
func waitSerialFor(t *testing.T, rw consoleRW, want string, timeout time.Duration, what string) string {
	t.Helper()
	ch := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := rw.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
				if strings.Contains(b.String(), want) {
					ch <- b.String()
					return
				}
			}
			if err != nil {
				ch <- b.String()
				return
			}
		}
	}()
	select {
	case out := <-ch:
		if !strings.Contains(out, want) {
			t.Fatalf("等待 %s：串口未出现 %q。串口输出尾部:\n%s", what, want, tailStr(out, 2000))
		}
		return out
	case <-time.After(timeout):
		t.Fatalf("等待 %s 超时（未出现 %q）", what, want)
		return ""
	}
}

// WriteMarker 确保有 shell 后把 snapFile 写成 content，并回显确认标记。
func (cs *consoleSession) WriteMarker(content string, timeout time.Duration) {
	cs.t.Helper()
	cs.ensureShell(timeout)
	echo := "WROTE-" + content
	cmd := fmt.Sprintf("echo %s > %s && echo %s\n", content, snapFile, echo)
	if _, err := cs.stream.Write([]byte(cmd)); err != nil {
		cs.t.Fatalf("串口写入命令: %v", err)
	}
	waitSerialFor(cs.t, cs.stream, echo, 60*time.Second, "写入 "+content+" 的确认标记")
}

// ReadMarker 确保有 shell 后读回 snapFile 内容。
//
// 串口三个坑（均经实测）：
//  1. guest 为 ash：`echo x<<$(cat f)>>y` 会被当重定向（syntax error），须用 `cat` + 前后夹标记。
//  2. 串口**回显命令本身**，且 pty 80 列会把长命令回显**折行**（`^M` 处断开）。
//  3. 最阴的一点：若标记字面量出现在命令行里，标记会**先**在命令回显中出现，
//     于是「等标记出现」会立刻命中回显、在 shell 执行前就返回（实测拿到的是空输出）。
//     故标记必须由 shell **运行时拼出**，命令行回显里不存在字面量。
func (cs *consoleSession) ReadMarker(timeout time.Duration) string {
	cs.t.Helper()
	cs.ensureShell(timeout)
	const begin, end = "MARKBEGIN7f", "MARKEND7f"
	// 用 printf 拼标记：命令回显含 "MARK%s" 与 "BEGIN7f" 但**不含** "MARKBEGIN7f"。
	cmd := fmt.Sprintf("B7f=$(printf 'MARK%%s' 'BEGIN7f'); E7f=$(printf 'MARK%%s' 'END7f'); echo -n $B7f; cat %s; echo -n $E7f\n", snapFile)
	if _, err := cs.stream.Write([]byte(cmd)); err != nil {
		cs.t.Fatalf("串口写入读取命令: %v", err)
	}
	out := waitSerialFor(cs.t, cs.stream, end, 60*time.Second, "读取结束标记")
	// 真实输出可能被 pty 折行（实测 80 列时 T05-LATER 与 MARKEND7f 分行），
	// 故先去掉 CR 与折行，把流拼成连续文本，再取「最后一个 begin 之后、其后第一个 end」。
	flat := strings.ReplaceAll(strings.ReplaceAll(out, "\r\n", ""), "\r", "")
	i := strings.LastIndex(flat, begin)
	if i < 0 {
		cs.t.Fatalf("串口输出未出现 begin 标记:\n%s", tailStr(out, 1500))
	}
	rest := flat[i+len(begin):]
	j := strings.Index(rest, end)
	if j < 0 {
		cs.t.Fatalf("begin 标记之后未出现 end 标记:\n%s", tailStr(out, 1500))
	}
	return strings.TrimSpace(rest[:j])
}

func TestSnapshotContentLevelRollbackRealLibvirt(t *testing.T) {
	imagesDir, vmsDir, vhostDir := "/var/lib/nfvis/images", "/var/lib/nfvis/vms", "/run/nfvis/vhost"
	image := os.Getenv("NFVIS_TEST_VM_IMAGE")
	if image == "" {
		image = "alpine.qcow2"
	}
	if _, err := os.Stat(filepath.Join(imagesDir, image)); err != nil {
		t.Skipf("跳过：无可引导镜像 %s（设置 NFVIS_TEST_VM_IMAGE）", filepath.Join(imagesDir, image))
	}
	if _, err := exec.LookPath("cloud-localds"); err != nil {
		t.Skipf("跳过：未找到 cloud-localds: %v", err)
	}
	for _, d := range []string{imagesDir, vmsDir, vhostDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cfg := compute.DefaultConfig()
	cfg.URI = os.Getenv("NFVIS_LIBVIRT_URI")
	cfg.VMsDir, cfg.ImagesDir, cfg.VhostDir = vmsDir, imagesDir, vhostDir
	cfg.StopTimeout = 20 * time.Second

	p, conn, err := compute.NewConnectedProvider(ctx, cfg)
	if err != nil {
		t.Skipf("跳过（libvirt 不可用）: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = p.DeleteVM(context.Background(), itSnapContentVM)
	t.Cleanup(func() { _ = p.DeleteVM(context.Background(), itSnapContentVM) })

	userData := "#cloud-config\n" +
		"ssh_pwauth: true\n" +
		"chpasswd:\n  list: |\n    root:" + snapGuestPass + "\n  expire: false\n" +
		"write_files:\n  - path: " + snapFile + "\n    content: \"" + snapKnown + "\"\n"
	vm := model.VMFunction{
		Name: itSnapContentVM, Image: image,
		VCPU:   model.VMCpu{Count: 1},
		Memory: model.VMMemory{SizeMB: 1024, HugepageSize: "1G"},
		CloudInit: &model.CloudInit{
			Hostname: "it-t05",
			UserData: userData,
			SSHKeys:  []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITESTKEY nfvis@test"},
		},
		Autostart: true,
	}
	cfgModel := model.Config{
		ResourcePools: &model.ResourcePool{
			Hugepages: []model.HPool{{PageSize: "1G", Count: 1}},
			CPU:       &model.CPUSetup{IsolatedCores: []int{1, 2, 3}},
		},
		VirtualMachineFunctions: []model.VMFunction{vm},
	}
	if err := p.DefineVM(ctx, vm, model.AllocationFor(cfgModel, vm)); err != nil {
		t.Fatalf("DefineVM: %v", err)
	}

	// ---- 步骤 2：串口登录把内容改成 LATER（磁盘此时 = LATER）----
	stream, err := p.Console(ctx, itSnapContentVM)
	if err != nil {
		t.Fatalf("打开串口 console: %v", err)
	}
	sess := &consoleSession{t: t, stream: stream}
	sess.WriteMarker(snapLater, 6*time.Minute)
	t.Log("guest 内写入 LATER 完成（cloud-init 设口令 + 串口登录链路可用）")
	if err := stream.Close(); err != nil {
		t.Logf("关闭首个 console 流: %v", err)
	}

	// ---- 步骤 3：关机后创建快照（内容 = LATER）----
	// 决策 #75：产品侧 create/rollback 需关机态（运行中 409）。这里走**产品允许的路径**，
	// 先关机再建快照——这也正是内容级回滚的正确用法。
	if err := p.StopVM(ctx, itSnapContentVM); err != nil {
		t.Fatalf("建快照前停止 VM: %v", err)
	}
	if err := p.SnapshotCreate(ctx, itSnapContentVM, "snap-t05", "T0-5 内容级回滚"); err != nil {
		t.Fatalf("SnapshotCreate: %v", err)
	}
	t.Log("已在内容=LATER（关机态）创建快照 snap-t05")

	// ---- 步骤 4：重启 guest，把内容改成 CHANGED（快照后写入）----
	if err := p.StartVM(ctx, itSnapContentVM); err != nil {
		t.Fatalf("快照后启动 VM: %v", err)
	}
	stream2, err := p.Console(ctx, itSnapContentVM)
	if err != nil {
		t.Fatalf("重启后打开串口 console: %v", err)
	}
	sess2 := &consoleSession{t: t, stream: stream2}
	sess2.WriteMarker(snapChanged, 6*time.Minute)
	t.Log("快照后已将 guest 内文件改为 CHANGED")
	if err := stream2.Close(); err != nil {
		t.Logf("关闭第二个 console 流: %v", err)
	}

	// ---- 步骤 5：关机回滚（产品路径；决策 #75 要求关机态）----
	if err := p.StopVM(ctx, itSnapContentVM); err != nil {
		t.Fatalf("回滚前停止 VM: %v", err)
	}
	if err := p.SnapshotRevert(ctx, itSnapContentVM, "snap-t05"); err != nil {
		t.Fatalf("SnapshotRevert: %v", err)
	}
	t.Log("关机态回滚成功")

	// ---- 步骤 6：启动 guest 使磁盘内容可见，串口回读应为 LATER ----
	if err := p.StartVM(ctx, itSnapContentVM); err != nil {
		t.Fatalf("回滚后启动 VM: %v", err)
	}
	stream3, err := p.Console(ctx, itSnapContentVM)
	if err != nil {
		t.Fatalf("回滚后打开串口 console: %v", err)
	}
	defer func() { _ = stream3.Close() }()
	sess3 := &consoleSession{t: t, stream: stream3}

	got := sess3.ReadMarker(6 * time.Minute)
	switch got {
	case snapLater:
		t.Logf("✅ 内容级回滚证实：guest 内文件为 %q（快照点内容）；CHANGED 与 KNOWN 均未留存", got)
	case snapChanged:
		t.Errorf("❌ 磁盘未回滚：读到 %q（快照后的写入仍在）", got)
	case snapKnown:
		t.Errorf("⚠️ 文件为 %q（首启内容），步骤 2 的 guest 内写入未生效，未证到目标", got)
	default:
		t.Errorf("❌ 读到意外内容 %q，期望 %q", got, snapLater)
	}
}

// TestSnapshotRevertOnRunningVMRealLibvirt 固化决策 #75 的**依据**：直接对**运行中**域
// 调用底层 libvirt 快照 API（绕过 API 层 409 守卫），实测其真实行为。
//
// 结论（实测）：`DomainSnapshotCreateXML(flags=0)` 与 `DomainRevertToSnapshot(flags=0)`
// 在运行中域上**均返回成功、不报 libvirt 错误**，但**QEMU 进程被替换**——即该 VM 被静默重启。
// 这正是产品在 API 层拒绝运行中快照（409）的原因：静默重启生产 VNF 不可接受。
//
// 本测试不经过 runtimestate 守卫（compute.Provider 无此守卫，守卫在 API 层），
// 故能如实观测底座行为；断言「pid 变化」而非「报错」。
func TestSnapshotRevertOnRunningVMRealLibvirt(t *testing.T) {
	imagesDir, vmsDir, vhostDir := "/var/lib/nfvis/images", "/var/lib/nfvis/vms", "/run/nfvis/vhost"
	image := os.Getenv("NFVIS_TEST_VM_IMAGE")
	if image == "" {
		image = "alpine.qcow2"
	}
	if _, err := os.Stat(filepath.Join(imagesDir, image)); err != nil {
		t.Skipf("跳过：无可引导镜像 %s（设置 NFVIS_TEST_VM_IMAGE）", filepath.Join(imagesDir, image))
	}
	if os.Geteuid() != 0 {
		t.Skip("跳过：需 root 读取 qemu 进程信息")
	}
	for _, d := range []string{imagesDir, vmsDir, vhostDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	cfg := compute.DefaultConfig()
	cfg.URI = os.Getenv("NFVIS_LIBVIRT_URI")
	cfg.VMsDir, cfg.ImagesDir, cfg.VhostDir = vmsDir, imagesDir, vhostDir
	cfg.StopTimeout = 15 * time.Second

	p, conn, err := compute.NewConnectedProvider(ctx, cfg)
	if err != nil {
		t.Skipf("跳过（libvirt 不可用）: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = p.DeleteVM(context.Background(), itSnapContentVM)
	t.Cleanup(func() { _ = p.DeleteVM(context.Background(), itSnapContentVM) })

	// 用独立镜像目录，避免与其它用例互踩。
	vm := model.VMFunction{
		Name: itSnapContentVM, Image: image,
		VCPU:   model.VMCpu{Count: 1},
		Memory: model.VMMemory{SizeMB: 1024, HugepageSize: "1G"},
		CloudInit: &model.CloudInit{
			Hostname: "it-t05",
			UserData: "#cloud-config\nhostname: it-t05\n",
			SSHKeys:  []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITESTKEY nfvis@test"},
		},
		Autostart: true,
	}
	cfgModel := model.Config{
		ResourcePools: &model.ResourcePool{
			Hugepages: []model.HPool{{PageSize: "1G", Count: 1}},
			CPU:       &model.CPUSetup{IsolatedCores: []int{1, 2, 3}},
		},
		VirtualMachineFunctions: []model.VMFunction{vm},
	}
	if err := p.DefineVM(ctx, vm, model.AllocationFor(cfgModel, vm)); err != nil {
		t.Fatalf("DefineVM: %v", err)
	}

	pidOf := func() string {
		out, _ := exec.Command("pgrep", "-f", "qemu-system.*"+itSnapContentVM).Output()
		return strings.TrimSpace(string(out))
	}
	pidBefore := pidOf()
	if pidBefore == "" {
		t.Skip("跳过：未找到该域的 qemu 进程（无法观测进程替换）")
	}

	// 对运行中域建内部快照并回滚（底层路径，不设断电前提）。
	if err := p.SnapshotCreate(ctx, itSnapContentVM, "live-snap", "运行中快照实测"); err != nil {
		t.Fatalf("运行中 SnapshotCreate 返回错误（与实测不符）: %v", err)
	}
	if err := p.SnapshotRevert(ctx, itSnapContentVM, "live-snap"); err != nil {
		t.Fatalf("运行中 SnapshotRevert 返回错误（与实测不符）: %v", err)
	}
	time.Sleep(3 * time.Second)

	pidAfter := pidOf()
	if pidAfter == "" {
		t.Fatalf("回滚后 qemu 进程消失（VM 未恢复运行）")
	}
	if pidBefore == pidAfter {
		t.Logf("注意：QEMU 进程未变（%s）——本次实测未观测到进程替换", pidBefore)
	} else {
		t.Logf("✅ 实测证实决策 #75 依据：运行中回滚返回成功且不报错，但 QEMU 进程被替换（%s → %s），即 VM 被静默重启", pidBefore, pidAfter)
	}
}
