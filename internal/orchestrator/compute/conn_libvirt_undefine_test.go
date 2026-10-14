package compute

// 决策 #485（UEFI 域无法删除的修复）的守护：
//   - (*Conn).Undefine 下发的 DomainUndefineFlags 标志必须同时含「快照元数据」与「NVRAM」两位，
//     否则 firmware=uefi（带 nvram）的域在 libvirt 12 上 undefine 会被拒（真机实证
//     "cannot undefine domain with nvram"）。
//
// go-libvirt 的 *libvirt.Libvirt 方法不可 mock，故 conn_libvirt.go 用可注入的 domainUndefine
// 包装真实调用；本用例替换它以**观察实际下发的标志**（recorded calls），并另以源码扫描钉住
// 「Undefine 走 undefineDomain」这条接线（沿 TestJobTierWiring 的既有做法）。

import (
	"os"
	"strings"
	"testing"

	"github.com/digitalocean/go-libvirt"
)

// TestUndefineDomainPassesNvramFlag 断言删除域时实际下发的标志含 NVRAM 与快照元数据两位。
// 红-绿：从 undefineDomain 去掉 DomainUndefineNvram（或改回只传快照位）即失败。
func TestUndefineDomainPassesNvramFlag(t *testing.T) {
	var (
		called bool
		got    libvirt.DomainUndefineFlagsValues
	)
	old := domainUndefine
	domainUndefine = func(_ *libvirt.Libvirt, _ libvirt.Domain, flags libvirt.DomainUndefineFlagsValues) error {
		called, got = true, flags
		return nil
	}
	t.Cleanup(func() { domainUndefine = old })

	if err := undefineDomain(nil, libvirt.Domain{}); err != nil {
		t.Fatalf("undefineDomain 返回错误: %v", err)
	}
	if !called {
		t.Fatal("undefineDomain 未调用底层 DomainUndefineFlags 包装")
	}
	if got&libvirt.DomainUndefineNvram == 0 {
		t.Fatalf("undefine 标志必须含 NVRAM 位（UEFI 域否则无法 undefine），实际 flags=%d", got)
	}
	if got&libvirt.DomainUndefineSnapshotsMetadata == 0 {
		t.Fatalf("undefine 标志必须含快照元数据位（FR-CMP-013），实际 flags=%d", got)
	}
}

// TestUndefineWiringToHelper 接线守护（源码扫描）：Undefine 必须经 undefineDomain 下发（而非
// 直接调用 DomainUndefineFlags 少带标志），保证上面的标志断言覆盖真实删除路径。
func TestUndefineWiringToHelper(t *testing.T) {
	src, err := os.ReadFile("conn_libvirt.go")
	if err != nil {
		t.Fatalf("读取 conn_libvirt.go: %v", err)
	}
	text := string(src)
	marker := "func (c *Conn) Undefine("
	i := strings.Index(text, marker)
	if i < 0 {
		t.Fatal("未找到 (*Conn).Undefine")
	}
	rest := text[i+len(marker):]
	if j := strings.Index(rest, "\nfunc "); j >= 0 {
		rest = rest[:j]
	}
	if !strings.Contains(rest, "undefineDomain(l, dom)") {
		t.Fatal("Undefine 必须经 undefineDomain 下发 undefine（否则标志不受 TestUndefineDomainPassesNvramFlag 守护）")
	}
	if strings.Contains(rest, "DomainUndefineFlags(") {
		t.Fatal("Undefine 不应直接调用 DomainUndefineFlags（应经 undefineDomain 带全标志）")
	}
}
