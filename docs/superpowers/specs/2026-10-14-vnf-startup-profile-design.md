# 镜像启动画像（VNF 自定义启动参数）v1 · 设计（决策 #485 草案）

> 立项来源：用户会话（2026-10-14）——「有些 VNF（VM 或容器）需要使用特定参数（qemu 或 docker）
> 才能启动，现有 CLI 无法自定义 VNF 启动参数」。参考实现：EVE-NG `eve-ng-dev/templates`
> （per-image YAML 模板，含 `qemu_options`/`dock_args` 自由串 + `config_script`）与
> GNS3 `gns3-server/appliances`（per-appliance `.gns3a` JSON，结构化字段为主 + 可选 `qemu.options` 自由串）。
>
> 本文件是**设计口径**；契约（附录 A #485 / 命令树设计 / 命令全表 / openapi）与实现见后续
> 「契约先行」步骤。**首版仅 VM（QEMU）**，容器留后续。

## 1. 背景与缺口

现状（代码事实）：

- VM 的 domain XML 由 `internal/orchestrator/compute/domain_xml.go` 生成，**机器类型恒 `q35`、
  CPU 模式恒 `host-passthrough`、emulator 恒 `/usr/bin/qemu-system-x86_64`**（`domain_xml.go:31-34`）；
  `compute.Config` 虽有 `Machine`/`CPUMode` 字段，但 `cmd/nfvisd/main.go:402` 只覆盖 `URI`/`DataPlane`，
  **无任何用户配置入口**。全仓库 grep 不到 `qemu:commandline` 或额外 qemu 参数透传。
- VM 可配字段（`internal/model/model.go:632` 的 `VMFunction` + 命令树 `tree_config.go:500`）只有：
  image、vcpu count/pin、memory（size-mb/hugepage-size/numa/backing）、disks、interfaces、pci-device、
  cloud-init、serial console 开关、autostart、description。
- 容器可配字段（`ContainerFunction`）：image、vcpu、memory、interfaces(memif)、env、command、args、
  restart-policy、autostart——`BuildCreateSpec`（`docker.go:862`）里 `Privileged`/`CapAdd`/`Binds`
  按「有无 memif 口」自动推导，**不可配**。

⇒ 结果：**需要特定启动参数才能起来的镜像（老机器类型、UEFI-only、只认 IDE 启动、需要 `-cpu host`
/ 特定 SMBIOS、只输出 VNC 等）在当前产品上无法启动**。此缺口既未实现，也**未登记**在
`docs/NFViS-功能差距分析.md` / `docs/v2待做.md` / `docs/v3待做.md`（相邻登记项只有 qemu-guest-agent、
热调整、GPU 直通）。本设计即为登记与收口。

## 2. 参考实现（两者共同模式）

行业事实标准：**把「某个镜像需要什么启动参数」固化成 per-image 的模板/画像文件**，平台启动实例时并进去。

| | EVE-NG | GNS3 |
|---|---|---|
| 形态 | 每镜像一个 YAML（分 `amd/`、`intel/`） | 每 appliance 一个 `.gns3a`（JSON） |
| 结构化字段 | cpu、ram、ethernet、console、qemu_nic、arch、qemu_version | adapter_type、adapters、ram、cpus、hda/hdb_disk_interface、arch、console_type、kvm、boot_priority |
| 自由参数串 | `qemu_options`/`dock_args`——**整串** | `qemu.options`——**只放额外项**（如 `-cpu host`） |
| 运行时脚本 | 有 `config_script`（Python 改写命令行） | 无 |
| 镜像目录 | 不绑 | **绑进 appliance**（filename/version/md5sum/filesize/download_url） |

**结论**：采纳 GNS3 的分层——**结构化白名单字段为主体，自由参数串只作窄逃生口**；不抄 EVE-NG 的
`config_script`（产品里等于任意代码执行）。与两参考最大的不同：NFViS 有**全局资源账本**
（大页/NUMA/绑核，见 `AGENTS.md` 口径红线 #481），模板里的 `ram`/`cpus` 不能直接覆盖——故资源类
字段**不进画像**。

## 3. 已定口径（本次会话锁定）

1. **首版范围**：只做 VM（QEMU）；容器（docker 参数）留后续增量。
2. **挂载点**：画像挂**镜像级**（键＝镜像名），**VNF 实例可覆盖**个别项。生效优先级
   **实例 startup > 镜像画像 > 产品默认**。
3. **表达形式**：结构化白名单字段为主 + 一个**窄自由口**（`extra-args`，super-user、强校验、
   显式声明绕过账本）。
4. **console 形态纳入**：`serial`(默认) | `telnet` | `vnc`。
5. **结构选择＝方案 A**：画像落在**配置库**（新增顶层节），镜像文件仓库（`/var/lib/nfvis/images`
   + index.json）不动。

## 4. 设计

### 4.1 配置模型（`internal/model/model.go`）

```go
// VMImageProfile 镜像级启动画像（首版仅 VM/QEMU，决策 #485）。
type VMImageProfile struct {
    Image      string   `json:"image"`                    // 键＝vm-image 名
    Machine    string   `json:"machine,omitempty"`        // q35(默认)|pc|pc-i440fx-*|pc-q35-*
    CPUMode    string   `json:"cpu_mode,omitempty"`       // host-passthrough(默认)|host-model|custom
    CPUModel   string   `json:"cpu_model,omitempty"`      // cpu_mode=custom 时必填
    Firmware   string   `json:"firmware,omitempty"`       // bios(默认)|uefi
    DiskIface  string   `json:"disk_interface,omitempty"` // virtio(默认)|sata|ide|scsi
    BootOrder  string   `json:"boot_order,omitempty"`     // hd(默认)|cd|dc
    Accel      string   `json:"accel,omitempty"`          // kvm(默认)|tcg
    Console    string   `json:"console,omitempty"`        // serial(默认)|telnet|vnc
    ExtraArgs  []string `json:"extra_args,omitempty"`     // 窄自由口（S 档）
}
```

`Config` 增一行：`VMImageProfiles []VMImageProfile \`json:"vm_image_profiles,omitempty"\``。

VNF 实例覆盖块（`VMFunction` 增字段）：

```go
// Startup 启动画像覆盖（仅含画像里允许覆盖的子集；空＝全用画像/默认）。
Startup *VMStartupOverride `json:"startup,omitempty"`
```

`VMStartupOverride` 字段集＝`VMImageProfile` 去掉 `Image` 的同名子集（machine/cpu-mode/cpu-model/
firmware/disk-interface/boot-order/accel/console/extra-args）。

**口径红线**：画像与覆盖块**都不含** memory/vcpu/hugepage-size/numa/绑核——这些归资源账本单一真源
（`AGENTS.md` #481）。命令树里根本没有这些叶子，故「配了再拒」升级为「**无从配置**」。

### 4.2 命令树（`internal/schema/tree_config.go`）

```
set vm-image-profiles <image-name>            # 键=镜像名（? / Tab 从镜像仓库枚举 vm-image）
    machine <type>            q35|pc|pc-i440fx-*|pc-q35-*
    cpu mode <mode>           host-passthrough|host-model|custom
    cpu model <model>         （cpu mode custom 时）
    firmware <type>           bios|uefi
    disk-interface <iface>    virtio|sata|ide|scsi
    boot-order <order>        hd|cd|dc
    accel <type>              kvm|tcg
    console <type>            serial|telnet|vnc
    extra-args <arg>          （可多条；S 档，见 4.4）
```

`virtual-machine-functions <name>` 下新增 `startup` 子节，叶子同名（实例覆盖）。删除语句
`delete vm-image-profiles <image-name>`（整节删）/ `delete … <image> <leaf>`（单叶子）。

### 4.3 console 形态

| console | domain XML | 访问路径 |
|---|---|---|
| `serial`（默认） | `<serial type='pty'>` + `<console type='pty'>`（**现状不变**） | 既有 console 管线（ticket + WS + CLI raw + Ctrl-]） |
| `telnet` | `<serial type='tcp'><source mode='bind' host='127.0.0.1' service='<port>'/><protocol type='telnet'/></serial>` | 同一 console 管线，底层由 pty 换 tcp |
| `vnc` | `<graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1'/>`（+ `<video>` 模型） | **仅 Web**（noVNC 渲染，新增前端依赖）；CLI `console` 对 vnc **明确拒绝**并指向 Web（如实，不假装能显示） |

- **绑定地址一律 `127.0.0.1`**（与 #478 回环哲学一致，不扩大对外暴露面）。
- 与既有 `serial console enable/disable`（`SerialConsole *bool`）的关系：`console` 字段**存在时权威**；
  未设时回落既有语义（`serial_console==false` ⇒ 无 console，否则 serial）。**向后兼容、不破坏既有契约**。

### 4.4 apply / 编排 / 校验（红线）

**domain XML 合并**（`domain_xml.go`）：`DomainSpec` 已有 `Machine`/`CPUMode`；新增 `Firmware`/
`DiskIface`/`BootOrder`/`Accel`/`Console`/`ExtraArgs` 字段。编排层按 VM 传入**有效画像**（实例 >
画像 > 默认），覆盖全局 `compute.Config` 的固定值：

- `machine` → `<os><type machine=…>`；`cpu mode/model` → `<cpu mode=…>`（+`<model>`）
- `firmware=uefi` → `<os firmware='efi'>`（libvirt 走 OVMF）
- `disk-interface` → 主盘/数据盘 target bus；`boot-order` → `<os><boot dev=…>`
- `accel=tcg` → domain type 由 `kvm` 降为 `qemu`
- `extra-args` → `<qemu:commandline><qemu:arg value=…/></qemu:commandline>`

**窄自由口校验（denylist）**：`extra-args` 命中以下前缀**提交期拒绝**（名单定死在契约里 + 守护测试；
理由＝覆盖资源账本 / 抢走产品的网络·磁盘·console 模型）：

```
-m / -mem* / -smp / -object / -numa            （资源账本）
-kernel / -initrd / -append                     （启动镜像绕过产品磁盘模型）
-drive / -blockdev                              （磁盘模型）
-netdev / -nic / -net                           （网络模型）
-chardev / -monitor / -qmp / -mon / -serial     （console/监视器）
-cpu / -machine / -boot / -device               （有结构化字段，强制走字段）
-uuid / -daemonize / -pidfile / -S              （身份/进程生命周期）
```

**权限档 S**；每次使用进 audit；读视图与告警**显式标注「已绕过资源账本」**。

**apply 计划**（`internal/orchestrator/apply.go`）：新增节的 create/update/delete 进 `plan()`，
**撤销（undo）路径必须写全**（决策 #178/#179 的教训：apply 的 undo 缺口是历史高发区）；VM 重定义
必须带有效画像，否则 `request vpp restart` / nfvis 重启后重放的 domain 丢参数。

**校验**（`internal/model/validate.go` 新增 `checkVMImageProfiles`）：键去重、`cpu model` 与
`cpu mode=custom` 一致性、枚举合法；提交期要求画像指向的**镜像在仓库存在**。镜像被删后画像成孤儿
→ 对账告警 `VM_IMAGE_PROFILE_ORPHAN`（同 `ACL_LEFTOVER` 族，随事实消解，跨 nfvisd 重启可见）。

### 4.5 读视图 / Web

| 面 | 入口 | 内容 |
|---|---|---|
| CLI | `show vm-image-profiles [<image>]` | 画像逐项（含来源＝配置） |
| CLI | `show virtual-machine-functions <name> detail` | 新增「有效启动画像」块，**逐项标来源**（镜像画像/实例覆盖/产品默认）——照 #134「三源对照」经验，避免「看着有其实没生效」 |
| REST | `GET /vm-image-profiles`（openapi 新增 schema） | 同上 |
| Web | 镜像详情页「启动画像」分栏 | 走既有 `wbtn`/`data-write` 门禁；S 级入口按 class 隐藏（#324/#327 机制） |

### 4.6 守护测试

1. 命令树 ↔ 命令全表 ↔ openapi 三处一致（既有 archtest 机制）。
2. `show configuration | display set` 能反推画像等价 set 语句（round81 硬要求）。
3. apply undo 缺口测试（画像节增删改的撤销）。
4. `extra-args` denylist 拒绝清单测试（红-绿）。
5. 画像 → domain XML 的**字段级**断言（machine/cpu/firmware/disk/boot/accel/console/extra-args）。
6. 孤儿画像对账告警建/消测试。

## 5. 契约面草案

- **附录 A #485**（`docs/NFViS-系统产品需求与目标架构规格书.md`，v3 线第 82 条）：标题
  「镜像启动画像——VNF 自定义启动参数（首版 VM/QEMU）」；由来/口径/不改/落点/契约面/测试/真机验证
  按第 4 节填；`AGENTS.md` 决策条数同步 **+1**（取仓库当前实际值，勿凭记忆写死）。
- **`docs/NFViS-CLI命令树完整设计.md`**：新增 `vm-image-profiles` 节 + `virtual-machine-functions … startup` 子节。
- **`docs/NFViS-CLI命令全表.md`**：`set/delete vm-image-profiles …`、`set/delete virtual-machine-functions <n> startup …`、`show vm-image-profiles …` 各行（含权限档、REST 映射、状态）。
- **`docs/NFViS-openapi.yaml`**：新增 `VMImageProfile` schema + `VMStartupOverride` + `GET /vm-image-profiles`；
  `VMFunction` 增 `startup`；`openapi.json` 以 `--write` 同步。
- **`docs/NFViS-用户手册.md`**：新章节「镜像启动画像」+ `extra-args` 红线与 console 形态说明。

## 6. 非目标与边界（如实登记）

- **首版不做容器**：docker 的 privileged/cap-add/device/挂载/shm 等留后续。
- **不做运行时脚本**（EVE-NG `config_script`）：产品里等于任意代码执行，明确拒绝。
- **资源类一律不碰账本**：memory/vcpu/hugepage/numa/绑核不进画像，`extra-args` denylist 兜底。
- **不做热修改**：画像改动需停机重定义 VM（与 FR-CMP-012 一致）。
- **VNC 访问仅 Web**：CLI `console` 对 vnc 拒绝并指向 Web；noVNC 是本次新增的前端依赖。
- **`extra-args` 是窄口**：`-device`/`-netdev` 等一律拒；确有需求时补**结构化字段**，不放宽自由串。

## 7. 实现清单（供 writing-plans 排期）

1. 契约先行：附录 A #485 + 命令树设计 + 命令全表 + openapi（`--write`）+ AGENTS 条数。
2. 模型：`VMImageProfile`/`VMStartupOverride` + `Config.VMImageProfiles` + `Validate` 校验。
3. 命令树 + 执行器（`set/delete/show vm-image-profiles`、`startup` 覆盖、display set 反推）。
4. 编排：`DomainSpec` 扩字段 + `BuildDomain` 合并 + 有效画像计算（实例>画像>默认）。
5. `extra-args` denylist 校验 + 审计 + 读视图标注。
6. console：telnet/vnc 的 domain 映射 + 访问路径（telnet 走既有管线；vnc 走 Web noVNC + CLI 拒绝）。
7. 对账：孤儿画像告警 `VM_IMAGE_PROFILE_ORPHAN`。
8. 守护测试 + 红-绿；`make check` 全绿。
9. 真机验证：老机器类型 / UEFI / IDE 启动 / `-cpu host` / telnet / vnc 各造一个可启动现场；
   `request vpp restart` 后重放不丢参数；四套件复跑。
