package api

// round51 评审 L7：`delete virtual-machine-functions <n> vcpu count <c> pin <b>` 忽略 isSet
// ——apply 函数签名是 `_ bool` 并无条件写 cpu["count"]/cpu["pin"]，于是「删一条 vcpu 设定」
// 实际是**写**一条（delete 变 add 的同族），且写的是入参里的值。
//
// 语义选择（两形态一致）：该语句写的是 vcpu 的 count 与 pin **两个叶**，删除即逐叶清除
//（与 aliasStaticRouteBoth 这类复合语句的删除同口径：只清本语句写过的叶），清空后连
// `vcpu` 空壳一起回收——统计口径下它等价于裸形态 `delete … vcpu`（通用树遍历删整键），
// 也与 container-functions 的 `delete … vcpu count <n>`（`delete(ct,"vcpu")`）一致。
// 不选「只删 count 保留 pin」：count 是模型必填（validate 报「vcpu count 必填且大于 0」），
// 留下 pin 只会让提交必然被拒。

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// TestFixR51DeleteVMFVcpuPinDoesNotWrite：delete 不得写入任何值；set 分支保持不变。
func TestFixR51DeleteVMFVcpuPinDoesNotWrite(t *testing.T) {
	var cfg model.Config
	r51MustSet(t, &cfg, "set virtual-machine-functions fw-vm image base.qcow2")
	r51MustSet(t, &cfg, "set virtual-machine-functions fw-vm vcpu count 4 pin false")

	vm := cfg.VirtualMachineFunctions[0]
	if vm.VCPU.Count != 4 || vm.VCPU.Pin == nil || *vm.VCPU.Pin {
		t.Fatalf("前置：set 应写入 count=4 / pin=false，实际: %+v", vm.VCPU)
	}

	// delete 形态：语句里的取值**不得**被写进去（修复前 count/pin 被写成 2/true）
	if err := r51DelStmt(&cfg, "delete virtual-machine-functions fw-vm vcpu count 2 pin true"); err != nil {
		t.Fatalf("delete 语句应成功（清掉 vcpu 设定）: %v", err)
	}
	got := cfg.VirtualMachineFunctions[0].VCPU
	if got.Count != 0 || got.Pin != nil {
		t.Fatalf("delete 不得写入任何值（修复前会写成 count=%d pin=%v）: %+v",
			got.Count, got.Pin, got)
	}

	// 幂等：再删一次＝无变化（noChangeError，脚本模式按提示继续；不是「写入」）
	err := r51DelStmt(&cfg, "delete virtual-machine-functions fw-vm vcpu count 2 pin true")
	if err == nil || !strings.Contains(err.Error(), "未产生配置变更") {
		t.Fatalf("重复删除应报「未产生配置变更」（而非写入或报错）: %v", err)
	}
}

// TestFixR51DeleteVMFVcpuPinKeepsOtherVMFields：删 vcpu 设定不得牵连其它字段/其它 VM。
func TestFixR51DeleteVMFVcpuPinKeepsOtherVMFields(t *testing.T) {
	var cfg model.Config
	r51MustSet(t, &cfg, "set virtual-machine-functions fw-vm image base.qcow2")
	r51MustSet(t, &cfg, "set virtual-machine-functions fw-vm memory size-mb 1024")
	r51MustSet(t, &cfg, "set virtual-machine-functions fw-vm vcpu count 2 pin true")
	r51MustSet(t, &cfg, "set virtual-machine-functions db-vm image base.qcow2")
	r51MustSet(t, &cfg, "set virtual-machine-functions db-vm vcpu count 8 pin false")

	if err := r51DelStmt(&cfg, "delete virtual-machine-functions fw-vm vcpu count 2 pin true"); err != nil {
		t.Fatalf("delete 应成功: %v", err)
	}
	fw := cfg.VirtualMachineFunctions[0]
	if fw.VCPU.Count != 0 || fw.VCPU.Pin != nil || fw.Memory.SizeMB != 1024 || fw.Image != "base.qcow2" {
		t.Fatalf("删除应只清 fw-vm 的 vcpu 设定: %+v", fw)
	}
	db := cfg.VirtualMachineFunctions[1]
	if db.VCPU.Count != 8 || db.VCPU.Pin == nil || *db.VCPU.Pin {
		t.Fatalf("不得牵连其它 VM 的 vcpu 设定: %+v", db.VCPU)
	}
}
