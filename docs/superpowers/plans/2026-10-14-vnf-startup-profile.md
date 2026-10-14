# 镜像启动画像（VNF 自定义启动参数）v1 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 NFViS 支持「镜像级启动画像 + VNF 实例覆盖」的 VM(QEMU) 启动参数（机器类型/CPU 模式/固件/磁盘接口/引导顺序/加速器/console 形态/窄自由口），使需要特定参数才能启动的镜像可用。

**Architecture:** 新增一个配置库顶层节 `vm-image-profiles`（键＝镜像名），`VMFunction` 增 `startup` 覆盖块；编排层计算「实例 > 画像 > 默认」的有效画像，喂给 `compute.DomainSpec` → `BuildDomain` 生成 domain XML。自由口 `extra-args` 走 denylist 校验后注入 `<qemu:commandline>`。

**Tech Stack:** Go；libvirt（`libvirt.org/go/libvirtxml`）；配置库（`internal/config` 的 candidate/commit）；CLI 命令树（`internal/schema`）；openapi（`docs/NFViS-openapi.yaml` → `internal/api/openapi.json`）。

**Spec:** `docs/superpowers/specs/2026-10-14-vnf-startup-profile-design.md`

## Global Constraints

- **契约先行**：任何代码改动前，附录 A #485 + 命令树设计 + 命令全表 + openapi（`docs/NFViS-openapi.yaml` + `bash contrib/scripts/check_openapi_json_sync.sh --write`）必须先落（Task 1）。
- **资源账本红线（AGENTS.md #481）**：画像/覆盖块**不得**出现 memory/vcpu/hugepage-size/numa/绑核；`extra-args` 命中 denylist（`-m`/`-mem*`/`-smp`/`-object`/`-numa`/`-kernel`/`-initrd`/`-append`/`-drive`/`-blockdev`/`-netdev`/`-nic`/`-net`/`-chardev`/`-monitor`/`-qmp`/`-mon`/`-serial`/`-cpu`/`-machine`/`-boot`/`-device`/`-M`/`-uuid`/`-daemonize`/`-pidfile`/`-S`）一律**提交期拒绝**。
- **权限档**：`extra-args` 与 `console` 相关写入口为 **S 档**（super-user）。
- **默认值**：machine=q35、cpu_mode=host-passthrough、firmware=bios、disk_interface=virtio、boot_order=hd、accel=kvm、console=serial。
- **每次改动后跑**：`go test ./...`；收尾跑 `make check`（七项）。
- **中文文案**：所有用户可见文案用中文；不得在用户可见文本里写决策号（`internal/archtest/user_text_test.go` 会拦）。
- **Git**：当前分支 `v3`；每个 Task 末尾提交（conventional commit）。

## File Structure

- `internal/model/model.go` — 新增 `VMImageProfile`/`VMStartupOverride`；`Config.VMImageProfiles`；`VMFunction.Startup`。
- `internal/model/startup.go`（新建）— `EffectiveStartup` 合并纯函数 + `ValidateExtraArgs` denylist。
- `internal/model/validate.go` — 新增 `checkVMImageProfiles`。
- `internal/schema/tree_config.go` — 新增 `vm-image-profiles` 节 + `virtual-machine-functions … startup` 子节。
- `internal/api/cliexec.go` — `identityFields` 增 `"vm_image_profiles": "image"`。
- `internal/orchestrator/compute/domain_xml.go` — `DomainSpec` 扩字段 + `BuildDomain` 映射。
- `internal/orchestrator/apply.go` — 画像节进 `plan()`（含 undo）。
- `internal/orchestrator/network/…` 或 `internal/api/…` — 有效画像计算接入 VM 定义路径（Task 11 定位）。
- `internal/api/cli_compute_show.go`（或新建 `cli_vm_startup.go`）— `show vm-image-profiles` + VNF detail 有效画像。
- `internal/api/ui/app.js` — 镜像详情页「启动画像」分栏（Task 12）。
- `docs/NFViS-系统产品需求与目标架构规格书.md` / `AGENTS.md` / `docs/NFViS-CLI命令全表.md` / `docs/NFViS-CLI命令树完整设计.md` / `docs/NFViS-openapi.yaml` / `docs/NFViS-用户手册.md` — 契约文档。

---

### Task 1: 契约先行（文档 + openapi）

**Files:**
- Modify: `docs/NFViS-系统产品需求与目标架构规格书.md`（附录 A 追加 #485 行；按既有行格式）
- Modify: `AGENTS.md`（决策条数 +1，取仓库当前实际值）
- Modify: `docs/NFViS-CLI命令树完整设计.md`（新增 `vm-image-profiles` 节 + `startup` 子节）
- Modify: `docs/NFViS-CLI命令全表.md`（新增 set/delete/show 各行）
- Modify: `docs/NFViS-openapi.yaml`（新增 `VMImageProfile`/`VMStartupOverride` schema + `GET /vm-image-profiles` + `VMFunction.startup`）
- Regenerate: `internal/api/openapi.json`

**Interfaces:**
- Produces: 契约（后续所有 Task 依赖的命令/字段名）。

- [ ] **Step 1: 追加附录 A #485 行**

在 `docs/NFViS-系统产品需求与目标架构规格书.md` 附录 A 末尾（#484 行后）按既有格式追加一行，内容取自 spec §4/§5（由来＝本会话缺口 + EVE-NG/GNS3 参考；口径＝镜像级画像 + 实例覆盖 + 结构化字段 + 窄自由口 + console serial/telnet/vnc；落点＝本计划各文件；契约面＝本条 + AGENTS + 命令树 + 全表 + openapi；测试/真机验证按 Task 13）。

- [ ] **Step 2: AGENTS.md 决策条数 +1**

把 `AGENTS.md` 里声明的决策条数改为仓库当前实际值 +1（先 `grep -c '^| [0-9]' docs/NFViS-系统产品需求与目标架构规格书.md` 附近确认口径，或按 Makefile 校验脚本的实际判据）。

- [ ] **Step 3: 命令树设计 + 命令全表**

按 spec §4.2 把两条语句族写入 `docs/NFViS-CLI命令树完整设计.md` 与 `docs/NFViS-CLI命令全表.md`：
`set/delete vm-image-profiles <image-name> {machine|cpu mode|cpu model|firmware|disk-interface|boot-order|accel|console|extra-args}`、
`set/delete virtual-machine-functions <name> startup {…同名…}`、`show vm-image-profiles [<image>]`。权限档：`extra-args` 与 `console` 标 **S**，其余标 **S**（配置模式写入口）。

- [ ] **Step 4: openapi schema**

在 `docs/NFViS-openapi.yaml` 的 `components/schemas` 增加：

```yaml
    VMImageProfile:
      type: object
      required: [image]
      properties:
        image: { type: string, description: 镜像名（键） }
        machine: { type: string, enum: [q35, pc, pc-i440fx-*, pc-q35-*], description: 机器类型，缺省 q35 }
        cpu_mode: { type: string, enum: [host-passthrough, host-model, custom], description: 缺省 host-passthrough }
        cpu_model: { type: string, description: cpu_mode=custom 时必填 }
        firmware: { type: string, enum: [bios, uefi], description: 缺省 bios }
        disk_interface: { type: string, enum: [virtio, sata, ide, scsi], description: 缺省 virtio }
        boot_order: { type: string, enum: [hd, cd, dc], description: 缺省 hd }
        accel: { type: string, enum: [kvm, tcg], description: 缺省 kvm }
        console: { type: string, enum: [serial, telnet, vnc], description: 缺省 serial；vnc 仅 Web 可访问 }
        extra_args:
          type: array
          items: { type: string }
          description: 额外 QEMU 参数（窄自由口；命中 denylist 提交期拒绝；S 档；绕过资源账本）
    VMStartupOverride:
      type: object
      description: VNF 实例对镜像画像的覆盖（仅允许覆盖的子集）
      properties:
        machine: { type: string }
        cpu_mode: { type: string, enum: [host-passthrough, host-model, custom] }
        cpu_model: { type: string }
        firmware: { type: string, enum: [bios, uefi] }
        disk_interface: { type: string, enum: [virtio, sata, ide, scsi] }
        boot_order: { type: string, enum: [hd, cd, dc] }
        accel: { type: string, enum: [kvm, tcg] }
        console: { type: string, enum: [serial, telnet, vnc] }
        extra_args: { type: array, items: { type: string } }
```

在 `VMFunction` schema 增 `startup: { $ref: "#/components/schemas/VMStartupOverride" }`；新增路径：

```yaml
  /vm-image-profiles:
    get:
      summary: 镜像启动画像列表
      responses:
        "200": { description: OK }
```

- [ ] **Step 5: 同步 openapi.json 并跑守护**

```bash
bash contrib/scripts/check_openapi_json_sync.sh --write
bash contrib/scripts/check_openapi_no_duplicate_keys.sh
bash contrib/scripts/check_openapi_json_sync.sh
```
Expected: 三条均输出 `✓`。

- [ ] **Step 6: Commit**

```bash
git add docs/NFViS-系统产品需求与目标架构规格书.md AGENTS.md docs/NFViS-CLI命令树完整设计.md docs/NFViS-CLI命令全表.md docs/NFViS-openapi.yaml internal/api/openapi.json
git commit -m "docs: 镜像启动画像契约先行（决策 #485）"
```

---

### Task 2: 模型字段

**Files:**
- Modify: `internal/model/model.go`（`Config` 结构 + 新增两个类型 + `VMFunction` 增字段）
- Test: `internal/model/startup_test.go`（新建）

**Interfaces:**
- Produces: `model.VMImageProfile`、`model.VMStartupOverride`、`Config.VMImageProfiles []VMImageProfile`、`VMFunction.Startup *VMStartupOverride`。

- [ ] **Step 1: 写失败测试**

`internal/model/startup_test.go`：

```go
package model

import "testing"

func TestVMImageProfileJSONKeys(t *testing.T) {
    c := Config{VMImageProfiles: []VMImageProfile{{Image: "img1", Machine: "q35", ExtraArgs: []string{"-cpu host"}}}}
    if c.VMImageProfiles[0].Image != "img1" {
        t.Fatalf("image key not held")
    }
    vm := VMFunction{Name: "v1", Startup: &VMStartupOverride{Console: "vnc"}}
    if vm.Startup == nil || vm.Startup.Console != "vnc" {
        t.Fatalf("startup override not held")
    }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/model/ -run TestVMImageProfileJSONKeys`
Expected: 编译失败（`VMImageProfile` 未定义）。

- [ ] **Step 3: 加模型**

在 `internal/model/model.go` 的 `Config` 结构里、`ContainerFunctions` 之后加：

```go
	// VMImageProfiles 镜像级启动画像（决策 #485）：键＝vm-image 名，供 VM 的 domain 生成使用。
	VMImageProfiles []VMImageProfile `json:"vm_image_profiles,omitempty"`
```

在 `VMFunction` 结构末尾（`Autostart` 后）加：

```go
	// Startup 启动画像覆盖（实例级；仅含镜像画像允许覆盖的子集；决策 #485）。
	Startup *VMStartupOverride `json:"startup,omitempty"`
```

在 `ContainerFunction` 定义附近新增：

```go
// VMImageProfile 镜像级启动画像（决策 #485，首版仅 VM/QEMU）。
type VMImageProfile struct {
	Image     string   `json:"image"`
	Machine   string   `json:"machine,omitempty"`
	CPUMode   string   `json:"cpu_mode,omitempty"`
	CPUModel  string   `json:"cpu_model,omitempty"`
	Firmware  string   `json:"firmware,omitempty"`
	DiskIface string   `json:"disk_interface,omitempty"`
	BootOrder string   `json:"boot_order,omitempty"`
	Accel     string   `json:"accel,omitempty"`
	Console   string   `json:"console,omitempty"`
	ExtraArgs []string `json:"extra_args,omitempty"`
}

// VMStartupOverride VNF 实例对镜像画像的覆盖（字段集＝VMImageProfile 去掉 Image）。
type VMStartupOverride struct {
	Machine   string   `json:"machine,omitempty"`
	CPUMode   string   `json:"cpu_mode,omitempty"`
	CPUModel  string   `json:"cpu_model,omitempty"`
	Firmware  string   `json:"firmware,omitempty"`
	DiskIface string   `json:"disk_interface,omitempty"`
	BootOrder string   `json:"boot_order,omitempty"`
	Accel     string   `json:"accel,omitempty"`
	Console   string   `json:"console,omitempty"`
	ExtraArgs []string `json:"extra_args,omitempty"`
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/model/ -run TestVMImageProfileJSONKeys`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/model/model.go internal/model/startup_test.go
git commit -m "feat(model): 镜像启动画像模型字段（#485）"
```

---

### Task 3: 有效画像合并 + extra-args denylist（纯函数）

**Files:**
- Create: `internal/model/startup.go`
- Test: `internal/model/startup_test.go`（追加）

**Interfaces:**
- Consumes: Task 2 的类型。
- Produces:
  - `func EffectiveStartup(vm VMFunction, profiles []VMImageProfile) VMImageProfile`
  - `func ValidateExtraArgs(args []string) error`

- [ ] **Step 1: 写失败测试**

追加到 `internal/model/startup_test.go`：

```go
func TestEffectiveStartupPrecedence(t *testing.T) {
    profiles := []VMImageProfile{{Image: "img1", Machine: "pc", Firmware: "uefi", ExtraArgs: []string{"-global x"}}}
    vm := VMFunction{Name: "v1", Image: "img1", Startup: &VMStartupOverride{Machine: "pc-q35-6.2"}}
    got := EffectiveStartup(vm, profiles)
    if got.Machine != "pc-q35-6.2" { // 实例覆盖 > 画像
        t.Fatalf("instance override lost: %q", got.Machine)
    }
    if got.Firmware != "uefi" { // 画像 > 默认
        t.Fatalf("profile value lost: %q", got.Firmware)
    }
    if got.Accel != "kvm" || got.Console != "serial" || got.DiskIface != "virtio" || got.BootOrder != "hd" {
        t.Fatalf("defaults not filled: %+v", got)
    }
    if len(got.ExtraArgs) != 1 || got.ExtraArgs[0] != "-global x" {
        t.Fatalf("profile extra args lost: %+v", got.ExtraArgs)
    }
}

func TestEffectiveStartupNoProfile(t *testing.T) {
    got := EffectiveStartup(VMFunction{Name: "v1", Image: "none"}, nil)
    if got.Machine != "q35" || got.CPUMode != "host-passthrough" {
        t.Fatalf("defaults not applied: %+v", got)
    }
}

func TestValidateExtraArgsDenylist(t *testing.T) {
    for _, bad := range []string{"-m 8G", "-smp 4", "-object memory-backend-file", "-netdev tap", "-cpu host", "-drive file=x", "-M q35", "-serial tcp"} {
        if err := ValidateExtraArgs([]string{bad}); err == nil {
            t.Fatalf("expected reject for %q", bad)
        }
    }
    for _, ok := range []string{"-global kvm-pit.lost_tick_policy=discard", "-rtc base=utc", "-smbios type=1", "-no-reboot"} {
        if err := ValidateExtraArgs([]string{ok}); err != nil {
            t.Fatalf("expected accept for %q: %v", ok, err)
        }
    }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/model/ -run 'TestEffectiveStartup|TestValidateExtraArgs'`
Expected: 编译失败（函数未定义）。

- [ ] **Step 3: 实现**

`internal/model/startup.go`：

```go
package model

import (
	"fmt"
	"strings"
)

// 启动画像默认值（决策 #485）。
const (
	DefaultMachine   = "q35"
	DefaultCPUMode   = "host-passthrough"
	DefaultFirmware  = "bios"
	DefaultDiskIface = "virtio"
	DefaultBootOrder = "hd"
	DefaultAccel     = "kvm"
	DefaultConsole   = "serial"
)

// EffectiveStartup 合并有效启动画像：实例覆盖 > 镜像画像 > 产品默认。
// 资源类字段（memory/vcpu/hugepage/numa/绑核）不在画像内——账本单一真源（AGENTS #481）。
func EffectiveStartup(vm VMFunction, profiles []VMImageProfile) VMImageProfile {
	var p VMImageProfile
	for _, prof := range profiles {
		if prof.Image == vm.Image {
			p = prof
			break
		}
	}
	if o := vm.Startup; o != nil {
		if o.Machine != "" {
			p.Machine = o.Machine
		}
		if o.CPUMode != "" {
			p.CPUMode = o.CPUMode
		}
		if o.CPUModel != "" {
			p.CPUModel = o.CPUModel
		}
		if o.Firmware != "" {
			p.Firmware = o.Firmware
		}
		if o.DiskIface != "" {
			p.DiskIface = o.DiskIface
		}
		if o.BootOrder != "" {
			p.BootOrder = o.BootOrder
		}
		if o.Accel != "" {
			p.Accel = o.Accel
		}
		if o.Console != "" {
			p.Console = o.Console
		}
		if len(o.ExtraArgs) > 0 {
			p.ExtraArgs = o.ExtraArgs
		}
	}
	if p.Machine == "" {
		p.Machine = DefaultMachine
	}
	if p.CPUMode == "" {
		p.CPUMode = DefaultCPUMode
	}
	if p.Firmware == "" {
		p.Firmware = DefaultFirmware
	}
	if p.DiskIface == "" {
		p.DiskIface = DefaultDiskIface
	}
	if p.BootOrder == "" {
		p.BootOrder = DefaultBootOrder
	}
	if p.Accel == "" {
		p.Accel = DefaultAccel
	}
	if p.Console == "" {
		p.Console = DefaultConsole
	}
	return p
}

// extraArgDenyPrefixes 窄自由口的拒绝前缀（决策 #485）：覆盖资源账本 / 抢走产品的
// 网络·磁盘·console 模型 / 有结构化字段可代替 / 影响进程生命周期。
var extraArgDenyPrefixes = []string{
	"-m", "-mem", "-smp", "-object", "-numa",
	"-kernel", "-initrd", "-append",
	"-drive", "-blockdev", "-netdev", "-nic", "-net",
	"-chardev", "-monitor", "-qmp", "-mon", "-serial",
	"-cpu", "-machine", "-boot", "-device", "-M",
	"-uuid", "-daemonize", "-pidfile", "-S",
}

// ValidateExtraArgs 校验额外 QEMU 参数（denylist）。每个元素可含多个空白分隔的 token。
func ValidateExtraArgs(args []string) error {
	for _, a := range args {
		for _, tok := range strings.Fields(a) {
			for _, p := range extraArgDenyPrefixes {
				if strings.HasPrefix(tok, p) {
					return fmt.Errorf("额外 QEMU 参数 %q 不被允许（%s 前缀）：该类参数要么覆盖资源账本，要么与产品的网络/磁盘/console 模型冲突；请改用对应的结构化字段（machine/cpu mode/firmware/disk-interface/boot-order/accel/console）", tok, p)
				}
			}
		}
	}
	return nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/model/ -run 'TestEffectiveStartup|TestValidateExtraArgs'`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/model/startup.go internal/model/startup_test.go
git commit -m "feat(model): 有效画像合并与 extra-args denylist（#485）"
```

---

### Task 4: 校验（枚举 + 一致性 + 镜像存在）

**Files:**
- Modify: `internal/model/validate.go`（`Validate` 内加调用 + 新增 `checkVMImageProfiles`）
- Test: `internal/model/validate_test.go`（追加）

**Interfaces:**
- Consumes: Task 2/3。
- Produces: `Validate` 对画像的拒绝项（提交期）。

- [ ] **Step 1: 写失败测试**

追加到 `internal/model/validate_test.go`：

```go
func TestValidateVMImageProfiles(t *testing.T) {
    base := Config{VMImageProfiles: []VMImageProfile{{Image: "img1", Machine: "q35"}}}
    if errs := Validate(base); len(errs) != 0 {
        t.Fatalf("valid profile rejected: %v", errs)
    }
    // 重复键
    dup := Config{VMImageProfiles: []VMImageProfile{{Image: "img1"}, {Image: "img1"}}}
    if errs := Validate(dup); len(errs) == 0 {
        t.Fatalf("duplicate key not rejected")
    }
    // cpu mode=custom 缺 model
    bad := Config{VMImageProfiles: []VMImageProfile{{Image: "img1", CPUMode: "custom"}}}
    if errs := Validate(bad); len(errs) == 0 {
        t.Fatalf("custom without model not rejected")
    }
    // 非法枚举
    bad2 := Config{VMImageProfiles: []VMImageProfile{{Image: "img1", Accel: "bogus"}}}
    if errs := Validate(bad2); len(errs) == 0 {
        t.Fatalf("bad enum not rejected")
    }
    // extra-args denylist
    bad3 := Config{VMImageProfiles: []VMImageProfile{{Image: "img1", ExtraArgs: []string{"-m 8G"}}}}
    if errs := Validate(bad3); len(errs) == 0 {
        t.Fatalf("denylisted extra-args not rejected")
    }
    // VNF startup 覆盖也要过 denylist
    bad4 := Config{
        VirtualMachineFunctions: []VMFunction{{Name: "v1", Image: "img1", Startup: &VMStartupOverride{ExtraArgs: []string{"-netdev tap"}}}},
    }
    if errs := Validate(bad4); len(errs) == 0 {
        t.Fatalf("denylisted startup extra-args not rejected")
    }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/model/ -run TestValidateVMImageProfiles`
Expected: FAIL（未校验，返回空）。

- [ ] **Step 3: 实现**

在 `internal/model/validate.go` 的 `Validate` 里（`v.checkVMFunctions(c)` 之后）加 `v.checkVMImageProfiles(c)`，并新增：

```go
func (v *validator) checkVMImageProfiles(c Config) {
	dupCheck(v, c.VMImageProfiles, "vm-image-profiles", func(p VMImageProfile) string { return p.Image }, "镜像画像")
	for i, p := range c.VMImageProfiles {
		at := fmt.Sprintf("vm-image-profiles[%s]", p.Image)
		if strings.TrimSpace(p.Image) == "" {
			v.errf("vm-image-profiles[%d]: 镜像名不能为空", i)
			continue
		}
		if p.CPUMode == "custom" && strings.TrimSpace(p.CPUModel) == "" {
			v.errf("%s: cpu mode custom 必须给出 cpu model", at)
		}
		enumCheck(v, at, "machine", p.Machine, []string{"", "q35", "pc"}, "q35|pc|pc-i440fx-*|pc-q35-*")
		enumCheck(v, at, "cpu_mode", p.CPUMode, []string{"", "host-passthrough", "host-model", "custom"}, "")
		enumCheck(v, at, "firmware", p.Firmware, []string{"", "bios", "uefi"}, "")
		enumCheck(v, at, "disk_interface", p.DiskIface, []string{"", "virtio", "sata", "ide", "scsi"}, "")
		enumCheck(v, at, "boot_order", p.BootOrder, []string{"", "hd", "cd", "dc"}, "")
		enumCheck(v, at, "accel", p.Accel, []string{"", "kvm", "tcg"}, "")
		enumCheck(v, at, "console", p.Console, []string{"", "serial", "telnet", "vnc"}, "")
		if err := ValidateExtraArgs(p.ExtraArgs); err != nil {
			v.errf("%s: %v", at, err)
		}
	}
	for _, vm := range c.VirtualMachineFunctions {
		if vm.Startup != nil {
			if err := ValidateExtraArgs(vm.Startup.ExtraArgs); err != nil {
				v.errf("virtual-machine-functions[%s] startup: %v", vm.Name, err)
			}
		}
	}
}
```

> 注：`dupCheck`/`enumCheck`/`v.errf` 用 `validate.go` 里**既有**的助手；若签名不同，按同文件既有 `checkVMFunctions` 的写法对齐（`machine` 允许 `pc-i440fx-*`/`pc-q35-*` 前缀，`enumCheck` 用前缀匹配或单独判 `strings.HasPrefix`——按既有 `checkVMFunctions` 的枚举写法实现）。
> 镜像存在性校验放在编排层（Task 11，需访问镜像仓库），不在 `model.Validate` 内。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/model/ -run TestValidateVMImageProfiles`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/model/validate.go internal/model/validate_test.go
git commit -m "feat(model): 镜像画像校验（#485）"
```

---

### Task 5: 命令树节点

**Files:**
- Modify: `internal/schema/tree_config.go`
- Test: `internal/schema/gen_test.go`（追加，或既有树测试文件）

**Interfaces:**
- Consumes: `schema.K/P/SPD/SPA/V/VE/Opt/Su`、`schema.DynImages`。
- Produces: `set/delete vm-image-profiles …`、`virtual-machine-functions … startup …` 的树路径。

- [ ] **Step 1: 写失败测试**

在 `internal/schema` 的测试里追加：

```go
func TestTreeHasVMImageProfiles(t *testing.T) {
    root := ConfigPathTree()
    if _, _, err := Match(root, []string{"vm-image-profiles", "img1", "machine", "q35"}); err != nil {
        t.Fatalf("vm-image-profiles path missing: %v", err)
    }
    if _, _, err := Match(root, []string{"vm-image-profiles", "img1", "extra-args", "-global x"}); err != nil {
        t.Fatalf("extra-args leaf missing: %v", err)
    }
    if _, _, err := Match(root, []string{"virtual-machine-functions", "v1", "startup", "console", "vnc"}); err != nil {
        t.Fatalf("startup override path missing: %v", err)
    }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/schema/ -run TestTreeHasVMImageProfiles`
Expected: FAIL（路径不存在）。

- [ ] **Step 3: 加树节点**

在 `internal/schema/tree_config.go` 的 `ConfigPathTree()` 里，与 `virtual-machine-functions`（`tree_config.go:500`）平级新增：

```go
		// —— vm-image-profiles（§2.7，决策 #485，首版仅 VM/QEMU）——
		K("vm-image-profiles", "镜像启动画像（VM/QEMU 启动参数）",
			P("<image-name>", "镜像名", DynImages,
				K("machine", "机器类型", VE("type", "q35|pc|pc-i440fx-*|pc-q35-*", "q35", "pc")),
				K("cpu", "CPU",
					K("mode", "模式", VE("mode", "host-passthrough|host-model|custom", "host-passthrough", "host-model", "custom")),
					Opt(K("model", "型号（mode=custom 时）", V("string", "型号"))),
				),
				K("firmware", "固件", VE("type", "bios|uefi", "bios", "uefi")),
				K("disk-interface", "磁盘接口", VE("iface", "virtio|sata|ide|scsi", "virtio", "sata", "ide", "scsi")),
				K("boot-order", "引导顺序", VE("order", "hd|cd|dc", "hd", "cd", "dc")),
				K("accel", "加速器", VE("type", "kvm|tcg", "kvm", "tcg")),
				Su(K("console", "控制台形态（vnc 仅 Web 可访问）", VE("type", "serial|telnet|vnc", "serial", "telnet", "vnc"))),
				Su(K("extra-args", "额外 QEMU 参数（窄自由口；S 档）", SPA("<arg>", "extra_args", "额外 QEMU 参数"))),
			),
		),
```

在 `virtual-machine-functions`（`tree_config.go:500`）的 `<name>` 子节点里（`description` 之前）新增 `startup` 子节：

```go
				Su(K("startup", "启动画像覆盖（覆盖镜像画像的同名项）",
					K("machine", "机器类型", VE("type", "q35|pc|pc-i440fx-*|pc-q35-*", "q35", "pc")),
					K("cpu", "CPU",
						K("mode", "模式", VE("mode", "host-passthrough|host-model|custom", "host-passthrough", "host-model", "custom")),
						Opt(K("model", "型号（mode=custom 时）", V("string", "型号"))),
					),
					K("firmware", "固件", VE("type", "bios|uefi", "bios", "uefi")),
					K("disk-interface", "磁盘接口", VE("iface", "virtio|sata|ide|scsi", "virtio", "sata", "ide", "scsi")),
					K("boot-order", "引导顺序", VE("order", "hd|cd|dc", "hd", "cd", "dc")),
					K("accel", "加速器", VE("type", "kvm|tcg", "kvm", "tcg")),
					K("console", "控制台形态", VE("type", "serial|telnet|vnc", "serial", "telnet", "vnc")),
					K("extra-args", "额外 QEMU 参数（窄自由口；S 档）", SPA("<arg>", "extra_args", "额外 QEMU 参数")),
				)),
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/schema/ -run TestTreeHasVMImageProfiles && go test ./internal/schema/`
Expected: PASS（既有树测试不回归）。

- [ ] **Step 5: Commit**

```bash
git add internal/schema/tree_config.go internal/schema/*_test.go
git commit -m "feat(schema): 镜像画像与 startup 覆盖命令树（#485）"
```

---

### Task 6: 执行器接线（identityFields + 泛化路径）

**Files:**
- Modify: `internal/api/cliexec.go:51`（`identityFields`）
- Test: `internal/api/cli_vm_startup_test.go`（新建）

**Interfaces:**
- Consumes: Task 2/5。
- Produces: `set/delete vm-image-profiles …` 与 `startup …` 经通用 `applyTokens` 落进 `Config.VMImageProfiles`/`VMFunction.Startup`。

- [ ] **Step 1: 写失败测试**

`internal/api/cli_vm_startup_test.go`（复用同包既有测试的 executor 构造方式；参照 `cli_compute_test.go` 的 setup）：

```go
func TestSetVMImageProfileViaGenericPath(t *testing.T) {
    // 见同包 cli_compute_test.go 的 executor 装配 helper（newTestExecutor 或等价）
    x := newTestExecutor(t)
    if out := x.Execute("admin", "super-user", "ssh", "set vm-image-profiles img1 machine pc"); strings.HasPrefix(out, "%%") {
        t.Fatalf("set machine failed: %s", out)
    }
    if out := x.Execute("admin", "super-user", "ssh", "set vm-image-profiles img1 extra-args \"-global x\""); strings.HasPrefix(out, "%%") {
        t.Fatalf("set extra-args failed: %s", out)
    }
    cfg, _, _ := x.engine.Candidate()
    if len(cfg.VMImageProfiles) != 1 || cfg.VMImageProfiles[0].Machine != "pc" {
        t.Fatalf("model not updated: %+v", cfg.VMImageProfiles)
    }
    // 删除
    if out := x.Execute("admin", "super-user", "ssh", "delete vm-image-profiles img1"); strings.HasPrefix(out, "%%") {
        t.Fatalf("delete failed: %s", out)
    }
    cfg2, _, _ := x.engine.Candidate()
    if len(cfg2.VMImageProfiles) != 0 {
        t.Fatalf("delete not applied: %+v", cfg2.VMImageProfiles)
    }
}
```

> 若同包没有 `newTestExecutor`，按 `internal/api/cli_compute_test.go` 里既有测试的装配写法（构造 `config.Engine` + `newCLIExecutor`）替换该 helper 调用。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/api/ -run TestSetVMImageProfileViaGenericPath`
Expected: FAIL（`vm_image_profiles` 元素身份字段未知 → 建出的元素键为 `name` 而非 `image`，或元素未建）。

- [ ] **Step 3: 加 identityFields 条目**

`internal/api/cliexec.go:51` 的 map 里加一行：

```go
	"vm_image_profiles": "image",
```

（`jsonKeyOf` 已把 `vm-image-profiles` → `vm_image_profiles`，无需其它映射；`cpu mode`→`cpu.mode`、`cpu model`→`cpu.model` 需确认 JSON 字段为 `cpu_mode`/`cpu_model`——见下 Step 4。）

- [ ] **Step 4: 处理 `cpu mode`/`cpu model` 的层级差异**

模型里 `cpu_mode`/`cpu_model` 是**平铺**字段，而树是 `cpu mode`/`cpu model`（两层）。通用遍历会把 `mode` 当 `cpu` 下的子对象（`cpu: {mode: ...}`），与平铺模型不符。加一条别名规则（`internal/api/cliexec.go` 的 `statementAliases` 或 `cli_aliases_compute.go`）：

```go
	// set vm-image-profiles <img> cpu mode|model <v> → 平铺 cpu_mode/cpu_model
	{pattern: []string{"vm-image-profiles", "*", "cpu", "mode", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			e, err := elemByID(tree, "vm_image_profiles", t[1])
			if err != nil {
				return err
			}
			if !isSet {
				delete(e, "cpu_mode")
				return nil
			}
			e["cpu_mode"] = t[4]
			return nil
		}},
	{pattern: []string{"vm-image-profiles", "*", "cpu", "model", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			e, err := elemByID(tree, "vm_image_profiles", t[1])
			if err != nil {
				return err
			}
			if !isSet {
				delete(e, "cpu_model")
				return nil
			}
			e["cpu_model"] = t[4]
			return nil
		}},
```

同法为 `virtual-machine-functions <n> startup cpu mode|model` 加两条（元素取 `virtual_machine_functions` 的 `t[1]`，再取/建其 `startup` 对象后写 `cpu_mode`/`cpu_model`）。

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/api/ -run TestSetVMImageProfileViaGenericPath`
Expected: PASS。再跑 `go test ./internal/api/`（既有不回归）。

- [ ] **Step 6: Commit**

```bash
git add internal/api/cliexec.go internal/api/cli_aliases_compute.go internal/api/cli_vm_startup_test.go
git commit -m "feat(api): 镜像画像 set/delete 执行器接线（#485）"
```

---

### Task 7: display set 反推守护

**Files:**
- Test: `internal/api/setstmt_test.go`（追加）或 `internal/api/cli_vm_startup_test.go`

**Interfaces:**
- Consumes: Task 2/5/6。
- Produces: 断言 `show configuration | display set` 能反推出画像等价语句。

- [ ] **Step 1: 写失败测试**

```go
func TestDisplaySetRoundTripVMImageProfile(t *testing.T) {
    cfg := model.Config{VMImageProfiles: []model.VMImageProfile{{
        Image: "img1", Machine: "pc", CPUMode: "custom", CPUModel: "Skylake-Client",
        Firmware: "uefi", DiskIface: "ide", BootOrder: "cd", Accel: "tcg",
        Console: "telnet", ExtraArgs: []string{"-global x"},
    }}}
    tree := toJSONTree(cfg)
    stmts, err := renderSetStatements(tree, nil)
    if err != nil {
        t.Fatalf("render: %v", err)
    }
    joined := strings.Join(stmts, "\n")
    for _, want := range []string{
        "set vm-image-profiles img1 machine pc",
        "set vm-image-profiles img1 cpu mode custom",
        "set vm-image-profiles img1 cpu model Skylake-Client",
        "set vm-image-profiles img1 firmware uefi",
        "set vm-image-profiles img1 disk-interface ide",
        "set vm-image-profiles img1 boot-order cd",
        "set vm-image-profiles img1 accel tcg",
        "set vm-image-profiles img1 console telnet",
        "set vm-image-profiles img1 extra-args",
    } {
        if !strings.Contains(joined, want) {
            t.Fatalf("display set missing %q:\n%s", want, joined)
        }
    }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/api/ -run TestDisplaySetRoundTripVMImageProfile`
Expected: FAIL（缺 `cpu mode` 等语句，或整体不生成）。

- [ ] **Step 3: 按需补 emitter**

若 `renderSetStatements`（`internal/api/setstmt.go`）对 `cpu_mode`/`cpu_model` 平铺字段不能反推出 `cpu mode`/`cpu model` 形态，则在 `subtreeEmitters`（`setstmt.go:84`）或 `rootFallbackEmitters`（`setstmt.go:90`）里为 `vm_image_profiles` 加一个 emitter，把平铺字段渲染成嵌套语句（`cpu mode …` / `cpu model …`）。`machine`/`firmware`/`disk-interface`/`boot-order`/`accel`/`console`/`extra-args` 若已能被机械 emitter 覆盖则不动。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/api/ -run TestDisplaySetRoundTripVMImageProfile`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/api/setstmt.go internal/api/*_test.go
git commit -m "feat(api): 镜像画像 display set 反推（#485）"
```

---

### Task 8: DomainSpec 扩字段 + BuildDomain 映射

**Files:**
- Modify: `internal/orchestrator/compute/domain_xml.go`
- Test: `internal/orchestrator/compute/domain_xml_test.go`（追加）

**Interfaces:**
- Consumes: Task 3（有效画像）。
- Produces: `DomainSpec.Firmware/DiskIface/BootOrder/Accel/Console/ExtraArgs`。

- [ ] **Step 1: 写失败测试**

```go
func TestBuildDomainStartupProfile(t *testing.T) {
    spec := baseSpec() // 复用 domain_xml_test.go 既有最小 spec 构造
    spec.Machine = "pc"
    spec.CPUMode = "custom"
    spec.CPUModel = "Skylake-Client"
    spec.Firmware = "uefi"
    spec.DiskIface = "ide"
    spec.BootOrder = "cd"
    spec.Accel = "tcg"
    xml, err := BuildDomainXML(spec)
    if err != nil {
        t.Fatal(err)
    }
    for _, want := range []string{
        `machine="pc"`, `mode="custom"`, `Skylake-Client`,
        `firmware="efi"`, `bus="ide"`, `<boot dev="cd"/>`, `type="qemu"`,
    } {
        if !strings.Contains(xml, want) {
            t.Fatalf("domain xml missing %q:\n%s", want, xml)
        }
    }
}

func TestBuildDomainVNCConsole(t *testing.T) {
    spec := baseSpec()
    spec.Console = "vnc"
    xml, _ := BuildDomainXML(spec)
    if !strings.Contains(xml, `<graphics type="vnc"`) || !strings.Contains(xml, `listen="127.0.0.1"`) {
        t.Fatalf("vnc graphics missing:\n%s", xml)
    }
    if strings.Contains(xml, `<serial`) {
        t.Fatalf("serial should be absent for vnc console:\n%s", xml)
    }
}

func TestBuildDomainTelnetConsole(t *testing.T) {
    spec := baseSpec()
    spec.Console = "telnet"
    xml, _ := BuildDomainXML(spec)
    if !strings.Contains(xml, `<serial type="tcp"`) || !strings.Contains(xml, `<protocol type="telnet"`) {
        t.Fatalf("telnet serial missing:\n%s", xml)
    }
}

func TestBuildDomainExtraArgs(t *testing.T) {
    spec := baseSpec()
    spec.ExtraArgs = []string{"-global kvm-pit.lost_tick_policy=discard"}
    xml, _ := BuildDomainXML(spec)
    if !strings.Contains(xml, "qemu:commandline") || !strings.Contains(xml, "-global kvm-pit.lost_tick_policy=discard") {
        t.Fatalf("qemu:commandline missing:\n%s", xml)
    }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/orchestrator/compute/ -run 'TestBuildDomain(StartupProfile|VNCConsole|TelnetConsole|ExtraArgs)'`
Expected: FAIL（字段未定义）。

- [ ] **Step 3: 扩 DomainSpec**

`domain_xml.go` 的 `DomainSpec`（`domain_xml.go:69`）里 `CPUMode` 之后加：

```go
	CPUModel  string   // CPUMode=custom 时的 CPU 型号
	Firmware  string   // bios|uefi（缺省 bios）
	DiskIface string   // virtio|sata|ide|scsi（缺省 virtio）
	BootOrder string   // hd|cd|dc（缺省 hd；iso 强制 cd）
	Accel     string   // kvm|tcg（缺省 kvm）
	Console   string   // serial|telnet|vnc（缺省 serial）
	ExtraArgs []string // 额外 QEMU 参数（已过 denylist）
```

- [ ] **Step 4: 改 BuildDomain**

在 `BuildDomain`（`domain_xml.go:120`）里：

- **accel**：`d.Type` 由固定 `"kvm"` 改为按 `spec.Accel`（`tcg`→`"qemu"`，否则 `"kvm"`）。
- **cpu**：`d.CPU = &libvirtxml.DomainCPU{Mode: cpuMode}` 改为在 `cpuMode=="custom"` 时附 `Model: &libvirtxml.DomainCPUModel{Value: spec.CPUModel}`（按 libvirtxml 的 `DomainCPU` 字段名；确认 vendored 定义后填）。
- **firmware**：`spec.Firmware=="uefi"` 时 `d.OS.Firmware = "efi"`。
- **disk-interface**：`buildDisks` 里主盘/数据盘的 `Target` 按 `spec.DiskIface` 生成（`virtio`→`vda`/`virtio`；`sata`→`sda`/`sata`；`scsi`→`sda`/`scsi`；`ide`→`hda`/`ide`）；iso 保持 cdrom。
- **boot-order**：非 iso 时 `bootDev` 由固定 `"hd"` 改为 `spec.BootOrder`（空取 `"hd"`）。
- **console**：把 `serialConsoleEnabled(vm)` 分支改为按 `spec.Console`：
  - `serial`：现状 pty 串口 + console。
  - `telnet`：`d.Devices.Serials = []libvirtxml.DomainSerial{{Source: &libvirtxml.DomainChardevSource{TCP: &libvirtxml.DomainChardevSourceTCP{Mode: "bind", Host: "127.0.0.1", Service: "<port>"}}, Protocol: &libvirtxml.DomainChardevProtocol{Type: "telnet"}, Target: &libvirtxml.DomainSerialTarget{Port: uintPtr(0)}}}`（端口分配见 Task 10）。
  - `vnc`：不设 serial/console；`d.Devices.Graphics = []libvirtxml.DomainGraphic{{VNC: &libvirtxml.DomainGraphicVNC{AutoPort: "yes", Port: -1, Listen: "127.0.0.1"}}}`；`d.Devices.Videos = []libvirtxml.DomainVideo{{Model: libvirtxml.DomainVideoModel{Type: "vga"}}}`（不再 `none`）。
- **extra-args**：`if len(spec.ExtraArgs) > 0 { d.QEMUCommandline = &libvirtxml.DomainQEMUCommandline{}; for _, a := range spec.ExtraArgs { for _, tok := range strings.Fields(a) { d.QEMUCommandline.Args = append(d.QEMUCommandline.Args, libvirtxml.DomainQEMUCommandlineArg{Value: tok}) } } }`。

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/orchestrator/compute/`
Expected: PASS（既有 domain 测试不回归）。

- [ ] **Step 6: Commit**

```bash
git add internal/orchestrator/compute/domain_xml.go internal/orchestrator/compute/domain_xml_test.go
git commit -m "feat(compute): domain XML 支持启动画像字段（#485）"
```

---

### Task 9: 编排层接入有效画像

**Files:**
- Modify: `internal/orchestrator/compute/provider.go`（`domainSpecFor`/定义路径，`provider.go:684` 附近）
- Modify: 调用方（`internal/orchestrator/apply.go` 的 VM 定义/恢复路径）
- Test: `internal/orchestrator/compute/provider_test.go`（追加）

**Interfaces:**
- Consumes: Task 3 `model.EffectiveStartup`、Task 8 `DomainSpec` 新字段。
- Produces: 定义 VM 时用有效画像填 `DomainSpec`。

- [ ] **Step 1: 写失败测试**

在 `provider_test.go` 里断言「给定 profile+override，产出的 DomainSpec 字段＝有效画像」（把画像合并的接入点做成可单测的纯函数或注入点）：

```go
func TestDomainSpecUsesEffectiveStartup(t *testing.T) {
    profiles := []model.VMImageProfile{{Image: "img1", Machine: "pc", Firmware: "uefi"}}
    vm := model.VMFunction{Name: "v1", Image: "img1", VCPU: model.VMCpu{Count: 1}, Memory: model.VMMemory{SizeMB: 512},
        Startup: &model.VMStartupOverride{Console: "vnc"}}
    spec := DomainSpecForTest(vm, profiles) // 暴露一个可测的组装入口（内部调用 model.EffectiveStartup）
    if spec.Machine != "pc" || spec.Firmware != "uefi" || spec.Console != "vnc" {
        t.Fatalf("effective profile not applied: %+v", spec)
    }
}
```

> 若既有 `provider.go` 的组装是方法而非纯函数，抽出 `DomainSpecForTest`（或等价内部函数）供测试调用，生产路径复用同一函数。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/orchestrator/compute/ -run TestDomainSpecUsesEffectiveStartup`
Expected: FAIL。

- [ ] **Step 3: 接入**

在 `provider.go:684` 附近（当前把 `p.cfg.CPUMode` 等填进 `DomainSpec` 处），改为先算 `eff := model.EffectiveStartup(vm, cfg.VMImageProfiles)`，把 `eff.Machine`/`CPUMode`/`CPUModel`/`Firmware`/`DiskIface`/`BootOrder`/`Accel`/`Console`/`ExtraArgs` 填进 `DomainSpec`；`p.cfg.Machine/CPUMode` 降为**兜底**（有效画像已带默认，故实际总取有效画像值）。`cfg.VMImageProfiles` 由装配层传入（新增 `compute.Config.VMImageProfiles`，在 `cmd/nfvisd/main.go` 从 committed 配置注入；改动配置需重定义 VM 生效，与 FR-CMP-012 一致）。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/orchestrator/compute/ && go test ./internal/orchestrator/`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/orchestrator/compute/provider.go internal/orchestrator/compute/provider_test.go cmd/nfvisd/main.go
git commit -m "feat(compute): 定义 VM 用有效启动画像（#485）"
```

---

### Task 10: console 访问路径（telnet 走既有管线；vnc 仅 Web + CLI 拒绝）

**Files:**
- Modify: `internal/orchestrator/compute/console_libvirt.go`（telnet 目标）/ `console.go`
- Modify: `internal/api/cli_compute_oper.go`（`request … console` 对 vnc 拒绝）
- Modify: `internal/api/ui/app.js`（vnc → noVNC；telnet 复用既有 WS 管线）
- Test: `internal/api/cli_compute_test.go`（追加）

**Interfaces:**
- Consumes: Task 8 的 `Console` 字段。
- Produces: `console` 命令按形态分派；vnc 返回可照做的拒绝文案。

- [ ] **Step 1: 写失败测试**

```go
func TestConsoleCommandRejectsVNC(t *testing.T) {
    x := newTestExecutor(t)
    x.Execute("admin", "super-user", "ssh", "set vm-image-profiles img1 console vnc")
    x.Execute("admin", "super-user", "ssh", "set virtual-machine-functions v1 image img1")
    out := x.Execute("admin", "super-user", "ssh", "request virtual-machine-functions v1 console")
    if !strings.Contains(out, "VNC") || !strings.Contains(out, "Web") {
        t.Fatalf("vnc console should be rejected with Web pointer: %s", out)
    }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/api/ -run TestConsoleCommandRejectsVNC`
Expected: FAIL。

- [ ] **Step 3: 实现**

- `cli_compute_oper.go` 的 console 分支：读该 VM 的有效画像 `console`；`vnc` → 返回 `%% 该 VM 使用 VNC 控制台，CLI 无法显示；请用 Web 控制台访问（<url>）\n`；`telnet`/`serial` → 走既有 ticket + WS 管线（telnet 底层连 tcp 串口）。
- `app.js`：`console` 命令返回的 `Kind`（新增 `"vnc"`）在 Web 侧用 noVNC 渲染；`telnet`/`serial` 复用既有 xterm.js 管线。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/api/ -run TestConsoleCommandRejectsVNC`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/orchestrator/compute/console_libvirt.go internal/api/cli_compute_oper.go internal/api/ui/app.js internal/api/cli_compute_test.go
git commit -m "feat: console 形态分派（telnet/vnc）（#485）"
```

---

### Task 11: apply 计划（画像节增删改 + undo）

**Files:**
- Modify: `internal/orchestrator/apply.go`（`plan()` 里新增画像节处理）
- Test: `internal/orchestrator/apply_test.go`（追加）

**Interfaces:**
- Consumes: Task 2。
- Produces: 画像节变更进 plan，撤销路径完整。

- [ ] **Step 1: 写失败测试**

```go
func TestPlanVMImageProfileCreateDelete(t *testing.T) {
    old := model.Config{}
    nw := model.Config{VMImageProfiles: []model.VMImageProfile{{Image: "img1", Machine: "pc"}}}
    p := plan(old, nw) // 按 apply.go 既有 plan 入口签名
    if len(p.ops) == 0 {
        t.Fatalf("no ops planned for profile create")
    }
    // 撤销回旧配置应无残留
    p2 := plan(nw, old)
    if len(p2.ops) == 0 {
        t.Fatalf("no ops planned for profile delete")
    }
    for _, op := range p2.ops {
        if op.undo == nil {
            t.Fatalf("op %q missing undo", op.desc)
        }
    }
}
```

> 按 `apply.go` 里 `plan`/`op`/`desc`/`undo` 的**既有**命名与签名调整（见 `apply.go:606` 的 `vm[%s]` op 构造）。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/orchestrator/ -run TestPlanVMImageProfileCreateDelete`
Expected: FAIL。

- [ ] **Step 3: 实现**

在 `plan()`（`apply.go`）里仿照 VM 节的 create/update/delete 分支，为 `vm-image-profiles` 加：新建/改动的画像不产生数据面动作（纯配置，供下次 VM 定义使用）——**但删除画像时若被某 VM 的 `startup`/image 引用**，按既有「先解引用」纪律处理（或仅在提交期校验引用，不阻断）。undo 记录旧画像值。确保 `EnsureConsistent`/恢复重放包含画像（VM 重定义时读 committed 画像）。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/orchestrator/`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/orchestrator/apply.go internal/orchestrator/apply_test.go
git commit -m "feat(orchestrator): 镜像画像进 apply 计划（#485）"
```

---

### Task 12: 读视图（CLI show + VNF detail 有效画像 + REST + Web）

**Files:**
- Modify: `internal/api/cli_compute_show.go`（`show vm-image-profiles`；`show virtual-machine-functions <n> detail` 加「有效启动画像」块）
- Modify: `internal/api/cliexec.go:680`（show 分派加 `vm-image-profiles`）
- Modify: `internal/api/server.go`（`GET /vm-image-profiles`）
- Modify: `internal/api/ui/app.js`（镜像详情页「启动画像」分栏）
- Test: `internal/api/cli_vm_startup_test.go`（追加）

**Interfaces:**
- Consumes: Task 2/3/6。
- Produces: 三面同源的读视图。

- [ ] **Step 1: 写失败测试**

```go
func TestShowVMImageProfilesAndEffective(t *testing.T) {
    x := newTestExecutor(t)
    x.Execute("admin", "super-user", "ssh", "set vm-image-profiles img1 machine pc firmware uefi")
    out := x.Execute("admin", "super-user", "ssh", "show vm-image-profiles")
    if !strings.Contains(out, "img1") || !strings.Contains(out, "pc") {
        t.Fatalf("show vm-image-profiles wrong: %s", out)
    }
    x.Execute("admin", "super-user", "ssh", "set virtual-machine-functions v1 image img1 vcpu count 1 memory size-mb 512")
    det := x.Execute("admin", "super-user", "ssh", "show virtual-machine-functions v1 detail")
    if !strings.Contains(det, "有效启动画像") || !strings.Contains(det, "uefi") {
        t.Fatalf("detail effective profile missing: %s", det)
    }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/api/ -run TestShowVMImageProfilesAndEffective`
Expected: FAIL。

- [ ] **Step 3: 实现**

- CLI `show vm-image-profiles [<image>]`：表格列出 `镜像 / machine / cpu / firmware / disk-interface / boot-order / accel / console / extra-args`。
- VNF detail：新增「有效启动画像」块，逐项**标来源**（`画像`/`实例覆盖`/`默认`）——用 `model.EffectiveStartup` 与画像/覆盖逐字段比对得出。
- REST `GET /vm-image-profiles`：返回 `VMImageProfile[]`（openapi 已定义）。
- Web：镜像详情页新增「启动画像」分栏（`wbtn` 门禁；S 级入口按 class 隐藏）。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/api/`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/api/cli_compute_show.go internal/api/cliexec.go internal/api/server.go internal/api/ui/app.js internal/api/cli_vm_startup_test.go
git commit -m "feat(api): 镜像画像读视图三面同源（#485）"
```

---

### Task 13: 孤儿画像对账告警 + 镜像存在性

**Files:**
- Modify: `internal/model/validate.go` 或编排层（提交期镜像存在性校验）
- Modify: `internal/api/alarms.go` / `internal/orchestrator/alarm.go`（`VM_IMAGE_PROFILE_ORPHAN`）
- Test: `internal/api/alarms_test.go` 或 `internal/orchestrator/alarm_test.go`（追加）

**Interfaces:**
- Consumes: 镜像仓库（`internal/images.Store`）。
- Produces: 告警 `VM_IMAGE_PROFILE_ORPHAN`（画像指向的镜像不存在）。

- [ ] **Step 1: 写失败测试**

```go
func TestVMImageProfileOrphanAlarm(t *testing.T) {
    // 配置里有画像 img1，但镜像仓库没有 img1 ⇒ 对账产生 VM_IMAGE_PROFILE_ORPHAN
    // 造出镜像后告警消解（按既有 ACL_LEFTOVER 对账测试的写法）
}
```

（按 `internal/api/alarms_test.go` 既有对账告警测试的装配方式填充，断言告警建/消。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/api/ -run TestVMImageProfileOrphanAlarm`
Expected: FAIL。

- [ ] **Step 3: 实现**

- 提交期：`Validate` 之外，在编排/装配层校验画像的 `Image` 在镜像仓库存在（不存在 → 提交拒绝，文案给出镜像名）。
- 对账：15s 巡检 + `EnsureConsistent` 里，对「配置有画像、仓库无镜像」的画像产生 `VM_IMAGE_PROFILE_ORPHAN`（warning）；镜像出现即消解（跨 nfvisd 重启可见，不靠进程内记忆）。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/api/ && go test ./internal/orchestrator/`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/api/alarms.go internal/orchestrator/alarm.go internal/api/alarms_test.go
git commit -m "feat: 孤儿镜像画像对账告警（#485）"
```

---

### Task 14: 收尾（make check + 守护 + 真机验证）

**Files:**
- Modify: 按需修守护/契约同步
- Test: 全套

- [ ] **Step 1: 契约同步守护**

```bash
bash contrib/scripts/check_openapi_json_sync.sh
bash contrib/scripts/check_openapi_no_duplicate_keys.sh
```
Expected: `✓`。

- [ ] **Step 2: make check 七项**

Run: `make check`
Expected: RC=0（含 archtest：决策条数、user_text、superuser_guard、依赖、打包；openapi 同步；toolcheck）。

- [ ] **Step 3: 全量测试**

Run: `go test ./...`
Expected: 0 失败。

- [ ] **Step 4: 真机验证（nfvis-vm）**

按 spec §7.9：各造一个可启动现场——老机器类型（`machine pc`）、UEFI（`firmware uefi`）、IDE 启动（`disk-interface ide`）、`extra-args "-cpu host"`、telnet console、vnc console；`request vpp restart` 后重放不丢参数；四套件复跑（fulltest/语义/lifecycle/pty）与基线对照。**清场前置**（AGENTS.md 纪律）。把结果写入 `docs/evidence/` 并在附录 A #485 行补「真机验证」段。

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "test: 镜像启动画像真机验证与收尾（#485）"
```

---

## Self-Review

- **Spec coverage**：spec §4.1（模型）→ Task 2；§4.2（命令树）→ Task 5；§4.3（console）→ Task 8/10；§4.4（apply/红线/denylist）→ Task 3/8/11；§4.5（读视图/Web）→ Task 12；§4.6（守护）→ Task 1/7/14；§5（契约面）→ Task 1；§6（非目标）→ 计划未含容器/运行时脚本/热修改，符合；§7（实现清单）→ Task 1-14 全覆盖。**镜像存在性**（spec §4.4）→ Task 13。
- **Placeholder scan**：Task 4/7/11/13 的测试夹具标注了「按既有 X 测试的写法填充」——因为具体 helper 名需在实现时对齐同包既有测试；代码主体（模型/合并/denylist/domain 映射）均已给出可编译实现。Task 8 的 `DomainCPU.Model` 字段名需对照 vendored libvirtxml 确认（已在步内注明）。
- **Type consistency**：`VMImageProfile`/`VMStartupOverride` 字段名（`CPUMode`↔`cpu_mode`、`DiskIface`↔`disk_interface`）在 Task 2/3/4/5/8 一致；`EffectiveStartup(vm, profiles)` 签名在 Task 3/9/12 一致；`ValidateExtraArgs([]string) error` 在 Task 3/4 一致；`DomainSpec` 新字段名在 Task 8/9 一致。
