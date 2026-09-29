# NFViS 功能差距分析（应该做但没做的功能）

> 记录日期：2026-09-30
> 分析基线：**v1.1.50**（tag `v1.1.50` → main `7cd81f9`，round88 收口；规格书附录 A 决策 #1~#201；
> 真机基线 fulltest 195/0/12、语义 12/0/1、生命周期 21/0/3、pty 10/10）
> 性质：**功能完善视角的差距分析记录，不构成已批准的路线图**。任何条目落地前均须按仓库规则
> 「契约先行」执行：先改 docs/ 下对应契约文档并在规格书附录 A 追加决策记录，再写代码。

---

## 方法与口径

本文只看功能完善，不按成本/环境/优先级过滤。由三条事实线交叉比对得出：

- **A. 账面欠款**：规格书 §12「V1 明确范围外（V2 候选清单）」30 项 + 附录 A 决策记录。
  ⚠️ 注意：§12 所列 30 项中，**Web 控制面**（决策 #115~#142 系列）、**物理业务口 link 状态告警**（#73）、
  **告警转发远程 syslog**（#69）、**openapi.json 运行时端点与分页**（#69/#70/#74）、
  **FR-SEC 强制力项**（#71/#72/#96）共 5 类后来被拉回实现，**但 §12 文本至今未回改**——
  判断「现在到底有什么」必须以附录 A + `docs/V1-验收检查表.md` 为准，而非 §12 原文。
- **B. 已登记未清偿**：`docs/V1-收尾待办.md`（待办与未完成项唯一入口）、`docs/V1-验收检查表.md`
  （109 条 FR：通过 101 / 未验 4 / 降级 2 / 移 V2 2）、`docs/M5-11-端到端与基准报告.md` §4、
  round79~88 证据文件（`docs/evidence/v1-closeout-round8*.txt`）里明确登记的未修缺陷与未验证项。
- **C. 新发现空白**：对当前实际能力面逐关键词 grep 核查——CLI 命令树（`docs/NFViS-CLI命令树完整设计.md`，
  全表 259 行/263 条）、OpenAPI（`docs/NFViS-openapi.yaml`，99 个路径）、Web 控制台（`internal/api/ui/`，
  32 条路由）、`internal/`+`cmd/` 全部 .go 源码——找出**任何文档都没登记、但同类 NFV/网络平台标配**的能力缺口。

**总体判断**：主轴（事务化配置 + L2 交换/VRF/静态路由/NAT/ACL/QoS/SPAN + VNF/容器生命周期 +
备份/升级/诊断运维工具链）已相当完整；按「一台功能完善的 NFV 一体机」标准衡量，欠账集中在四个象限：
**数据面动态能力**（动态路由/VXLAN/VRRP/STP/DHCP 全部为零）、**可观测性**（SNMP/流采样/历史时序/
逐规则计数全缺）、**AAA 外部化与凭证治理**（RADIUS/TACACS+/API key/逐 token 吊销）、
**计算面 guest 视角与热操作**（guest-agent/热迁移/热调整/容器 exec）。

---

## 一、账面欠款：规格书已承诺推迟、至今未做（27 条）

构成：§12 未做清单中的 24 项 + 决策层面 3 条；§12 另有 1 项（容器镜像目录名与 tag 一致性）
属「已登记未修缺陷」，归入第二节。

### 网络数据面（6）

1. **动态路由协议（OSPF/BGP）**——完全未做；架构已预留 Linux-CP + FRR 扩展路径
   （vpp plugins 里 linux-cp 开关已暴露），决策 #6。
2. **VXLAN overlay**——全 0 命中。
3. **VRRP 网关冗余**——全 0 命中。
4. **静态路由 ECMP（多下一跳）**——0 命中。
5. **storm control / port security / MAC 数量限制**——0 命中。
6. **tap 接口 VM 接入**——决策 #2 后置；现有 vhost-user / SR-IOV VF / memif 三种接入。

### 可观测性（3）

7. **SNMP（v2c/v3）**——网络设备基本盘，全 0 命中（决策 #17 后置）。
8. **sFlow/NetFlow/IPFIX 流量采样**——0 命中。
9. **ACL 逐规则命中计数**——决策 #68 正式降 V2 的技术债：govpp v0.13.0 调
   `acl_stats_intf_counters_enable` 会被 VPP 误执行为 `acl_del`（应答错位，可能误删 ACL），
   绑定后 stats 段亦无逐规则计数；需升级 govpp 或自解析 stats segment。

### AAA 与安全（3）

10. **RADIUS/TACACS+ 外部认证**——0 命中；现状仅本地用户 + class（外部 AAA 仅「预留」表述）。
11. **登录 banner**——0 命中。
12. **管理面主机防火墙策略配置**——0 命中（宿主 nftables 类管理）。

### 计算编排（8）

13. **VM 热迁移**——0 命中。
14. **vCPU/内存/vNIC 热调整**——现仅关机状态可改（FR-CMP-012：热调整列 V2）。
15. **GPU 及通用 PCI 设备直通**——0 命中。
16. **qemu-guest-agent 集成**——VNF 内 IP/状态上报、`show` 提供 guest 视图；现在 guest 地址
    只能靠 cloud-init 或串口人工确认。
17. **容器 exec / 交互式终端**——容器只有 `log`，没有 exec。
18. **外部镜像仓库对接（Docker Registry 协议 pull）**——现仅本地上传 + URL 拉取（FR-CMP-034 移 V2）。
19. **镜像仓库配额与版本 tag 管理**——0 命中。
20. **企业级镜像治理（签名验证/漏洞扫描）**——0 命中。

### 系统与运维（4）

21. **整机镜像 A/B 升级**——现 `request system software add` 为 deb 级升级。
22. **自动定期配置备份**——现在只有手动 `request system configuration backup`。
23. **多节点集群与集中管理**——0 命中（单机定位，但功能清单上为零）。
24. **CLI 批处理脚本 `-f <file>`**——`-c` 单串脚本模式已有，脚本文件加载没有。

### 半成品命令（决策层面另欠 3 条）

25. **`request system api token revoke <token-id>`（逐 token 吊销）**——只输出提示、不实现
    （决策 #76⑧）；V1 仅能经 `POST /logout` 吊销当前会话。
26. **`show configuration permissions <class>`**——语义从未定义，CLI 明确提示未实现（决策 #153）。
27. **`request system storage format-data`**——决策 #65 延期，等数据分区定义后开放。

---

## 二、已登记未修的缺口（17 条）

出处均为 `docs/V1-收尾待办.md`、round 证据文件或验收检查表；这些不是新发现，但同样是
「应该做没做」的欠账。

1. **R79-1（中）**：`POST /logout` 的候选清理按 `user@api` 身份键生效 ⇒ 同一用户跑任意一条
   CLI 命令会**静默丢弃控制台/脚本侧未提交候选**并释放编辑锁（待办 §2.3 有最小复现；
   决策 #151 只修了「一次性事务后交还锁」这一相关面）。根治需会话与身份键分离，契约先行。
2. **VM 启动失败的可诊断性**：vhost-user 初始化失败时 VM 停在 libvirt `paused (starting up)`，
   CLI/控制台只回「已受理」，失败原因仅 libvirt 层可见（round80 登记；已验证恢复办法
   `request vpp restart` + `start`）。
3. **operator 角色粒度**：决策 #145 只做「只读 vs 有写」两档，operator 仍会看到声明为
   super-user 的入口并得 403；需服务端下发权威 class（契约先行）。
4. **「秘密出口」全量复查**：决策 #149 只脱敏了诊断归档，同类出口未系统扫过。
5. **自助改密连带提交配置页候选**（round77 观察项）。
6. **1G 大页池「无主占用」无回收路径**（round88 #199/#201 只收掉了 vpp 包 sysctl 撑大机制，
   池回收本身仍开口）。
7. **CLI 引号内 `|` 被当管道切分、多行引号值只取首行**（多行 user-data 须走文件路径规避）。
8. **R84-16**：VNF 侧声明的端口不出现在 `show virtual-switches <vs> ports` 等配置读视图
   （物化进 `vs.Ports` 需先决策，会改配置库形状）。
9. **R84-18**：`cross-connect` 端口数不足时**静默无操作**（既有测试把「缩减到 1 端口」定义为合法）。
10. **R84-22**：恢复收敛顺序——vNIC 置入 L3 VRF 可能早于建表 ⇒ `No such FIB / VRF (-3)`
    未收敛项，需重放一次才成功。
11. **F1（P2，首装发现性）**：新装机 `show interfaces` 恒「（无接口）」、`set interfaces` 的
    Tab 候选为空——内核侧网卡（ens160/192/224）对操作者不可见（round81 §17，候选修法已写明）。
12. **COMMIT_COMPENSATION_FAILED 对非 VRF 表类残渣无自动消解路径**；且告警表进程内、
    重启丢历史（round87 §8 已知限制）。
13. **容器镜像目录名 ≠ Docker tag 时镜像不可用**：§12 第 30 项、决策 #76 §4①，至今仍是
    已知限制（README L177、CLI 命令全表 §4）。
14. **console 断开后空闲阻塞在 `Read`**，需按键才退出（FR-CMP-014 伴随缺陷，决策 #70⑤ 待修）。
15. **`request vpp restart` 返回后立即查询偶发 broken pipe**。
16. **#20（低）**：deb `Recommends` 未补 `qemu-utils, cloud-image-utils`（缺 qemu-img/cloud-localds
    时 VM 定义失败/测试跳过）。
17. **NFR-006 告警侧缺 `time_synced` 三态字段**（审计侧已实现，告警侧未加，验收检查表 NFR-006 行）。

### 未验证（功能是否真成立存疑，均环境受限）

- **SR-IOV 全链路**（FR-NET-004/021）：nfvis-vm 为 vmxnet3，无 PF/VF 硬件；
  附带缺陷：`request sriov delete-vfs` 忽略 `vf <n>` 参数，按计数递减（决策 #94 已「诚实化」）。
- **LLDP 邻居表**（FR-NET-018）：环境无 LLDP 对端（启用与命令已验）。
- **容器 memif 通流**（T0-3 / FR-NET-022 容器侧）：离线无自带 memif 客户端的容器镜像。
- **真人 `Ctrl-]`**（T0-4 / FR-CMP-014）：需运行 VM + 真实交互 TTY。
- **§10 吞吐/容量类基准**：10GbE ≥14Mpps、vhost-user ≥8Gbps、≤10 VM/≤20 容器、
  恢复 ≤30s/≤3min——需流量发生器与目标硬件，截至 v1.1.50 未测。
- **硬件健康极端场景**（FR-SYS-012）：VM 无 IPMI/温度传感器/smartctl（降级路径已验证）。

---

## 三、新发现：文档零登记、但同类产品标配的空白（13 条）

以下条目在任何规划文档（§12/附录 A/收尾待办）中均未出现，由第三条事实线（能力面 grep 核查）得出。

### 数据面

1. **DHCP server / relay**——全仓库 0 命中（仅两处注释提及 guest DHCP 场景）。
   BVI/L3 网关是产品自己的核心场景，VNF 拿地址只能靠 cloud-init 或外部 DHCP；至少 DHCP relay 该有。
2. **L2 环路防护（STP/RSTP，或至少环路检测）**——0 命中，且 CLI 全表对 cross-connect 场景
   明文警告「无环路保护」；自定位「虚拟交换机」却无任何防环手段。
3. **DNS 转发/代理**——`set dns server` 只是宿主解析器配置；无 dnsmasq 类转发服务供
   VNF/容器/管理网使用。
4. **IPv6 业务面完整性**——地址与静态路由已双栈，但 NAT / ACL 五元组 / 诊断命令
   （ping6/traceroute6）的 v6 覆盖未见证据（**待专项核实**）；「IPv6 仅管理面 + 静态路由」
   只能算半双栈。
5. **QoS 深度**——仅单速 cir/cbs 且只支持 ingress-policy 绑定；无 egress 方向、
   无层级 shaper / 优先级队列 / 丢弃策略。
6. **组播（IGMP/multicast）**——全 0 命中（POC 定位下优先级低，如实列出）。

### 可观测性

7. **历史时序数据**——`/metrics` 是即时快照、无时序存储；Web 总览 5s 轮询同样没有趋势图
   数据底座，容量规划与事后回溯无从谈起。
8. **事件外推 webhook**——告警出口只有远程 syslog 转发 + SSE 拉流；对接不了外部监控/工单系统的推送。
9. **`/metrics` 覆盖面**——`internal/metrics` 只采集宿主运行态（/proc 等）；VPP / VNF / 容器
   指标是否进 /metrics 未见证据（**待专项核实**）。

### 管理与安全

10. **独立 API 凭证（API key / 只读机器凭证）**——当前唯一凭证就是登录 Bearer token
    （REST/Web/CLI 桥共用一套），无机器对机器的独立凭证；第二节 R79-1 的根治口径
    （会话与身份键分离）也落在这里。
11. **License / 授权管理**——0 命中（商用化前置，POC 可缓）。
12. **标准北向（NETCONF/RESTCONF/YANG）**——远期备查；现阶段 OpenAPI 已够。

### 诊断细项

13. **ping / traceroute 不支持指定 VRF / 源地址 / IPv6**——命令树里 `ping` 仅 VPP 数据面
    IPv4；排障高频细项。

---

## 四、只按功能完善逻辑的补齐顺序（5 批）

- **第 0 批（账面对齐，地基）**
  §12 文本回改（5 项已实现仍写「范围外」）；3 条半成品命令收口（token revoke / permissions /
  format-data）；清偿第二节 17 条登记缺陷；CLI/REST/Web 三入口覆盖真缺口对齐
  （`docs/CLI-REST覆盖核查.md` 尚余 4 条，含 2 条 V1 登记延期）。
  理由：功能完善的前提是「承诺的都在、做了的都说清」。
- **第 1 批（网络 OS 基本盘）**
  DHCP relay/server、环路防护（STP 或检测）、ACL 逐规则命中计数（#68 技术债）、QoS egress、
  ping/traceroute 细项（VRF/源地址/v6）、F1 首装接口可见性。
- **第 2 批（可观测性基本盘）**
  SNMP v2c/v3、流采样（sFlow/IPFIX 择一）、历史时序存储、webhook 外推。
- **第 3 批（AAA 与凭证治理）**
  RADIUS/TACACS+、API key 独立凭证、逐 token 吊销、operator 粒度、秘密出口全量复查。
- **第 4 批（计算编排深化）**
  qemu-guest-agent、容器 exec、外部镜像仓库（Registry 协议）、镜像治理（配额/tag/签名）、
  vCPU/内存热调整、tap 接入。
- **第 5 批（平台化）**
  动态路由（linux-cp + FRR，架构路径已预留）、VXLAN、VRRP、ECMP、A/B 整机升级、
  多节点集群/集中管理、热迁移、GPU 直通。

---

## 附：本分析的证据来源

| 事实线 | 来源 |
|---|---|
| 账面欠款（A） | `docs/NFViS-系统产品需求与目标架构规格书.md` §1.2、§12（L478-480）、附录 A（#1~#201）；`docs/CLI-REST覆盖核查.md` §3 |
| 已登记未清偿（B） | `docs/V1-收尾待办.md`；`docs/V1-验收检查表.md` §5/§6；`docs/M5-任务清单.md`、`docs/M5-验收记录.md`、`docs/M5-11-端到端与基准报告.md` §4；`docs/evidence/v1-closeout-round79~88*.txt` |
| 能力面核查（C） | `docs/NFViS-CLI命令树完整设计.md`、`docs/NFViS-CLI命令全表.md`（259 行/263 条）、`docs/NFViS-openapi.yaml`（99 路径）、`internal/`（14 包）+`cmd/` 源码 grep、`internal/api/ui/`（5 文件/32 路由） |

> 使用说明：本文条目在立项时应逐条回到上表出处复核现状（后续版本可能已收口部分条目），
> 并按「契约先行」补规格书附录 A 决策后再实施。
