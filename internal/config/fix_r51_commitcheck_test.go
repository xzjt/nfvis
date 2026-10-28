package config

// round51 G2/G3 的守护：
//
//   - G3：`commit check` 与 `commit` 必须给出**同一结论**，两条路径共用 validationErrors。
//     此前 check 只跑 e.validate，漏掉镜像存在性 ⇒「check 报 OK、commit 被拒」。
//   - G2：FR-CFG-011⑤ 必须覆盖 `vm.Disks[].Image`（数据盘引用的镜像），此前完全绕过。
//
// 一条**有意保留**的差异也要钉住（见下 TestFixR51CommitCheckExcludesCommitOnlyGuards）：
// 自锁兜底与 confirm 守卫是「这次提交动作」的判据（分别读 opts.AllowNoSuperUser 与
// opts.ConfirmedMinutes / 相对 committed 的变更），不是「配置本身合不合法」，故只在提交路径判。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// G3 边界：自锁兜底（决策 #152）是**提交期**判据——commit check 不报（它拿不到调用方的
// opts），commit 报；且 AllowNoSuperUser 的豁免必须仍然生效（该开关只有恢复出厂会置位，
// 且由 internal/archtest/superuser_guard_test.go 钉住「恰好一处赋值 + 引擎确实读它」）。
//
// 这条边界此前被 G3 的第一次实现改成了「check 也报」，代价是恢复出厂路径的语义被伪装成
// 配置期判据、且 PUT candidate 一份不含用户的文档连预检都过不去（合法与否该由提交判定）。
func TestFixR51CommitCheckExcludesCommitOnlyGuards(t *testing.T) {
	k := newSuperUserKit(t) // 基线自带一个 super-user
	sess := Session{User: "admin", Source: "ssh"}
	k.edit(t, "admin", "ssh")

	// 整文档提交一份空配置 = 把本机提交成「无人可登录」（决策 #152）。
	if err := k.engine.UpdateCandidate(sess, model.Config{}); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}

	// ① commit check：配置本身就是合法的（结构/语义/镜像都没有问题），故为空。
	verrs, err := k.engine.CommitCheck(sess)
	if err != nil {
		t.Fatalf("CommitCheck 不该上抛: %v", err)
	}
	if len(verrs) != 0 {
		t.Fatalf("自锁兜底是提交期判据，commit check 不该报: %+v", verrs)
	}

	// ② commit（无豁免）：被拒，且报的正是自锁兜底。
	_, cerr := k.engine.Commit(context.Background(), sess, CommitOpts{})
	var ve *ValidationError
	if !errors.As(cerr, &ve) {
		t.Fatalf("commit 应被拒并返回 ValidationError: %v", cerr)
	}
	if !strings.Contains(fmt.Sprint(ve.Errors), "super-user") {
		t.Fatalf("commit 应报自锁兜底（至少要保留一个 super-user 账号）: %+v", ve.Errors)
	}
	// 既有语义不变：被拒后候选与编辑锁都保留，操作者改完可直接重提。
	if _, dirty, err := k.engine.Candidate(); err != nil || !dirty {
		t.Fatalf("被拒后候选应保留: err=%v dirty=%v", err, dirty)
	}

	// ③ 豁免仍然生效（恢复出厂路径）：AllowNoSuperUser 置位后不得再因自锁被拒
	//    （这一步会真的提交掉这份空配置，故 ④ 的「候选保留」断言放在它之前）。
	_, cerr = k.engine.Commit(context.Background(), sess, CommitOpts{AllowNoSuperUser: true})
	if errors.As(cerr, &ve) && strings.Contains(fmt.Sprint(ve.Errors), "super-user") {
		t.Fatalf("AllowNoSuperUser 的豁免失效（恢复出厂会被自己的兜底拦住）: %+v", ve.Errors)
	}
}

// G2 + G3②：数据盘引用的镜像要过规则⑤，且 commit check 与 commit 一致（含类型匹配）。
func TestFixR51CommitCheckSeesDataDiskImage(t *testing.T) {
	images := mockImages{
		"ubuntu22-vm": {Name: "ubuntu22-vm", Type: "vm-image"},
		"alpine-ct":   {Name: "alpine-ct", Type: "container-image"},
	}
	e, _, _ := newEngineWithExternals(t, images, nil)
	sess := Session{User: "admin", Source: "ssh"}
	if err := e.Edit(sess); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	// 与 TestEngineNumaWarning 同形的**合法** VM（正控证明它不是被别的原因拒掉）。
	vm := model.VMFunction{
		Name: "fw-vm", Image: "ubuntu22-vm",
		VCPU:       model.VMCpu{Count: 2},
		Memory:     model.VMMemory{SizeMB: 8192, HugepageSize: "1G"},
		Interfaces: []model.VnfInterface{{Name: "eth0", Type: "vhost-user", VirtualSwitch: "vs-app"}},
		Disks:      []model.VMDisk{{Name: "data0", Image: "ghost.qcow2"}},
	}
	cfg := baseCommitted()
	cfg.ResourcePools = poolsFor()
	cfg.VirtualMachineFunctions = []model.VMFunction{vm}
	if err := e.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}

	verrs, err := e.CommitCheck(sess)
	if err != nil {
		t.Fatalf("CommitCheck: %v", err)
	}
	if len(verrs) != 1 || verrs[0].Path != "virtual-machine-functions[fw-vm].disks[data0].image" {
		t.Fatalf("commit check 应报数据盘镜像（路径与主盘同形状）: %+v", verrs)
	}
	if !strings.Contains(verrs[0].Message, "不存在") || !strings.Contains(verrs[0].Message, "ubuntu22-vm") {
		t.Fatalf("数据盘镜像报错应可照着排查（列出现有名）: %+v", verrs[0])
	}
	_, cerr := e.Commit(context.Background(), sess, CommitOpts{})
	var ve *ValidationError
	if !errors.As(cerr, &ve) {
		t.Fatalf("commit 应被拒: %v", cerr)
	}
	if got, want := fmt.Sprint(ve.Errors), fmt.Sprint(verrs); got != want {
		t.Fatalf("commit check 与 commit 的结论不一致:\ncheck =%s\ncommit=%s", want, got)
	}

	// 类型也要判（容器镜像不能当 VM 数据盘）——与主盘同一套规则。
	cfg.VirtualMachineFunctions[0].Disks[0].Image = "alpine-ct"
	_ = e.UpdateCandidate(sess, cfg)
	verrs, err = e.CommitCheck(sess)
	if err != nil || len(verrs) != 1 || !strings.Contains(verrs[0].Message, "镜像类型不匹配") {
		t.Fatalf("数据盘镜像类型不匹配应被报出: err=%v verrs=%+v", err, verrs)
	}

	// 正控：指向仓库里真实存在的 vm-image ⇒ check 无错、commit 成功。
	cfg.VirtualMachineFunctions[0].Disks[0].Image = "ubuntu22-vm"
	if err := e.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	if verrs, err := e.CommitCheck(sess); err != nil || len(verrs) != 0 {
		t.Fatalf("合法数据盘镜像不该报错: err=%v verrs=%+v", err, verrs)
	}
	if _, err := e.Commit(context.Background(), sess, CommitOpts{}); err != nil {
		t.Fatalf("正控 commit 应成功: %v", err)
	}
}

// G3 边界（有意保留的差异）：confirm 守卫不进 validationErrors——它依赖 CommitOpts 与
// 「相对 committed 的变更」，是**提交动作**的判定，不是「配置本身合不合法」。故管理口变更时
// commit check 照样报 OK、而 commit 报 ErrConfirmRequired（照做路径是 `commit confirmed <n>`）。
func TestFixR51CommitCheckExcludesConfirmGuard(t *testing.T) {
	k := newEngineKit(t) // 基线带管理口 192.168.1.10/24
	sess := Session{User: "admin", Source: "ssh"}
	k.edit(t, "admin", "ssh")

	cfg := baseCommitted()
	cfg.System.Management = &model.MgmtConfig{Address: "192.168.2.10/24", Gateway: "192.168.2.1"}
	if err := k.engine.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	if verrs, err := k.engine.CommitCheck(sess); err != nil || len(verrs) != 0 {
		t.Fatalf("配置本身合法 ⇒ commit check 应为空: err=%v verrs=%+v", err, verrs)
	}
	if _, err := k.engine.Commit(context.Background(), sess, CommitOpts{}); !errors.Is(err, ErrConfirmRequired) {
		t.Fatalf("管理口变更须 commit confirmed: %v", err)
	}
}
