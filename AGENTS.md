# NFViS 开发约定（对本工作区内的所有开发会话生效）

任何在本仓库内工作的 AI 助手 / 开发会话，开始工作前先读本文件与 README.md。

## 项目状态与基线

- 当前阶段：**M1~M4 已完成并合并**；**M5 主体已完成**（T0 遗留 + M5-1~M5-11，见 `docs/M5-验收记录.md`、
  `docs/M5-11-端到端与基准报告.md`、`docs/M5-任务清单.md`）。M5 新增决策 **#52~#64**。
  **已完成并有真机证据**：T0-1（NAT 跨 VRF + D4 端到端）、T0-2（容器镜像类目 docker load）、T0-6（真实 cloud image URL + sha256）、
  T0-7（SPAN 抓包实证；LLDP 无对端受限）、M5-1（事件总线 + `/events` SSE）、M5-2（`/metrics`）、M5-3（VPP pcap trace 抓包导出/下载）、
  M5-4（tech-support + core dump）、M5-5（硬件健康 + 阈值告警）、M5-6（备份/恢复/zeroize）、M5-7（software add/rollback + reboot/shutdown + ntp）、
  M5-8（TLS 热换证 + SSH host key + 日志保留）、M5-10（deb 打包 + systemd 自守护 + sd_notify）、M5-11（`test/e2e` 主链路 + §10 响应类基准）。
  **仍未完成（如实登记）**：T0-3（容器 memif 通流，离线无自带 memif 的镜像）、T0-4（console 真人 `Ctrl-]`，需运行 VM + 真实 TTY）、
  以及 §10 吞吐/容量类基准（需流量发生器与规模压测）。详见 `docs/M5-11-端到端与基准报告.md` §4。
  **M5-9（CLI 剩余契约命令接线）已全部关闭**（2026-09-22 复核）：`show system hardware`（M5-5）、
  `request system api tls`（M5-8）、`monitor vnf`（**决策 #92 把「只执行一次快照」改成真跟踪**）均已接；
  仅余两条**明确延期 V2**：`request system storage format-data`、`request system password change`（原因见决策 #65）。
  **T0-5（快照磁盘内容级回滚）已于 V1 收尾第七轮完成**（决策 #75）。
- **round85（离线一键安装包）**：新增交付形态 `nfvis-vX.Y.Z.run`——自解压单文件（头部 shell + gzip 载荷），
  内含产品 deb + VPP 26.06 的 9 个 deb + apt 递归闭包（实测 238 个，171 MB），在**无外网**的干净 Ubuntu 26.04 上
  一步装齐并起服务，收尾做 18 项带独立事实源的自检（失败即非零退出）。构建端：`contrib/scripts/offline-closure.sh`
  （抓闭包 + 离线自洽校验）、`build-offline-run.sh`（可复现打包，两打逐字节一致）、`offline-installer-selftest.sh`
  （产物结构校验 + 两处工具自校准）；包内安装器 `deploy/offline/install.sh`。本轮修的两条缺陷：
  **#181** #159 的 AppArmor 放行块写在 `exit 0` 之后（死代码，装机报成功而 VNF 起不来）、
  **#182** 放行改由 nfvisd 单源保证（`internal/system/apparmor.go` + `nfvisd -ensure-libvirt-apparmor` +
  启动/60s 巡检复核）——因为 libvirt 可能晚于 nfvis 装载或在同一次 apt 事务里被后配置。
  ⚠️ **干净快照基线更正**：快照本身**没有 make/go**（只有 git、dpkg-deb、curl、chrony、tcpdump），
  故 `.run`/deb 的构建必须在有工具链的机器上做；`.run` 安装端**不需要**任何构建工具。
- **round88（干净快照走发布件真人路径 + 六条缺陷收口，2026-09-29）**：在**恢复为「干净」快照**的 nfvis-vm 上走
  **发布件真人路径**——GitHub Release 的 `nfvis-v1.1.49.run`（sha256 `559f5b84…`，上机后复核一致）
  `sh nfvis-v1.1.49.run -y` 一步装齐（**18/18 自检全过**、退出码 0）→ 按安装器提示重启 → wizard（真 pty）→
  数据面 → 接口/交换机/NAT/VNF 生命周期/容器/快照/抓包/诊断/Web 控制台黑盒全面走查（每条都有独立事实源：
  `vppctl`/`virsh`/`docker`/`tcpdump` 抓包/串口实况）。收口**六条**缺陷（决策 **#194~#199**，均真机复验）：
  **#194（R88-2）VRF 内静态路由的下一跳被拿到默认表解析**——路由 DPO 恒 `dpo-drop`（不转发），而
  `show vrfs <n> routes` 照常读得到（控制面回读正常），典型「命令成功、数据面全丢」；修法是 `FibPath` 填 `TableID`。
  **#195（R88-3）L2 的 access/trunk 子接口创建不幂等**——给已建好的交换机**再加一个端口**就撞 VPP
  `-56 already exists` 并整次提交回滚；删交换机后 VPP 里还长期残留 `ens224.100`（产品无清理路径）。
  **#196（R88-6）删除编排违反依赖倒序**——同一提交里删 VNF 又删「把它 vNIC 当 L3 接口的交换机」必然失败
  （先删了 vNIC，清地址时接口已不在），只能三次提交绕行；删顺序改为「先解引用、后删被引用」并对
  「接口已不存在」容错。**#197（R88-4）`/system/status.hostname` 只取配置值**——全新安装恒空，控制台总览
  「主机名」永远「—」；改为配置优先、否则回退 `os.Hostname()`。**#198（R88-5）控制台网络对象页 VRF 的
  「路由数」列渲染成 `[object Object]`**（取了对象数组未取长度）。**#199（R88-1）VPP 包自带的
  `vm.nr_hugepages=1024` 把 1G 池撑过内核基线声明值**——根因是产品设了 `default_hugepagesz=1G` 时该 sysctl
  落到**默认尺寸（1G）**池、开机按可用内存尽量分配（首装现场：声明 1、实际 4；此前只记作「1G 池无主占用」）；
  修法是产品按声明值写 90 号 sysctl 落点钉回（**对照实验**：无钉值 3、有钉值 1）。
  **两条观察项（如实登记）**：① 套件跑大文件上传叠加负载尖峰时系统出现 **soft lockup**（内核日志
  `BUG: soft lockup - CPU#0 stuck for 318s!`，与决策 #79 记录的同族现象），导致 systemd 看门狗
  （`WatchdogSec=30`）杀掉 nfvisd —— 非代码崩溃、非本轮修复引入，idle 时复跑全绿；② `show version` 的
  DPDK 恒「（未探测到）」（决策 #118 的既定取舍，CLI 与 Web 一致）。
  **四套件（修复版 1.1.50~dev1）与历史基线逐项一致**：fulltest **194/1/12**（唯一失败＝已登记的
  `show vpp runtime`）、语义 **12/0/1**（在有业务流量与对象的现场跑）、生命周期 **21/0/3**、pty **10/10**；
  服务零额外重启。⚠️ **清场是硬性前置**：本轮**两次**因保留自己的走查对象（`vs-wan` 占着 ens192）让
  fulltest 级联出 12~18 条假红（撞「同一网口角色互斥」的正确校验），清场后立刻回到 194/1/12。
  **同轮追加（决策 #200/#201，用户点名两问）**：① **`show vpp runtime` 落地**——从「未接入」占位改为
  **线程级**运行态（每线程向量率/主循环速率 + 整机向量率 + 工作线程数 + 数据面运行时长），数据源走 stats
  segment（与 buffer/接口计数同一条 `vpp_get_stats` 通道）；VPP 26.06 的**按节点**明细既无二进制 API、
  也不在 stats segment（全量 dump 18090 条无 `/sys/node/*`），只有 vppctl 读得到——产品不解析 vppctl 文本，
  输出里如实说明。**fulltest 因此从 194/1/12 变为 195/0/12**（round80 以来那条唯一 ✗ 归零）。
  ② **大页池 sysctl 改由 nfvis 单一事实源**：安装期 `dpkg-divert` 把 vpp 包的
  `/etc/sysctl.d/80-vpp.conf` 挪到 `.vpp-disabled`（prerm 还原、postrm(purge) 清理 90 号文件），
  90 号文件同时接管 `vm.hugetlb_shm_group=0`；真机覆盖装/幂等/卸/重装/重启，**接管后同一对照条件
  池 nr=1（接管前 3）**⇒ 撑大机制彻底消失。修复版四套件：fulltest **195/0/12**、语义 **12/0/1**、
  生命周期 **21/0/3**、pty **10/10**。
  **已发布 v1.1.50**（tag `v1.1.50` → `main 7cd81f9`；PR #232 + #233 均 squash 合并、CI 双跑全绿；
  `SOURCE_DATE_EPOCH=1790692519`）：deb sha256 `03b5721f06b53bc7321b56c39d01a08348934e85a92a9d5d5abf4ca8931e2777`（9,200,286 字节）、
  `.run` sha256 `5aa1ab49b0f8fe3584bcec93644b18b41fa1a699efba5c7f2725da898454558e`（178,505,119 字节）——两件均连打两次
  逐字节一致；回下载侧已有：deb 真实回下载逐字节一致、`.run` 与 GitHub 服务端资产摘要逐字相同
  （⚠️ `.run` 的完整 178MB 回下载本轮未完成——两条链路到 GitHub 都约 7~8KB/s，刻意不阻塞发版，已如实登记）；
  发布件上 fulltest **195/0/12**（首次零失败）、
  `.run` 自检与 `--verify` 各 **12/12**。
  证据 `docs/evidence/v1-closeout-round88-fresh-walkthrough-and-6-fixes.txt`（§7 为本轮追加）。
- **round87（清 round86 登记的两条未修 + 本轮新抓一条，2026-09-29）**：先在**未修版 1.1.48**（已装实例）
  上按同一脚本复现三条缺陷，再按修复版 `1.1.49~dev1~dev3` 逐条复验、三件套 + 集成套件复跑、Web 黑盒复核。
  **已发布 v1.1.49**（tag `v1.1.49` → `main e4ce946`，PR #231 squash 合并；`SOURCE_DATE_EPOCH=1790667231`；
  deb sha256 `97a6e5f3…`（9,184,698 字节）、`.run` sha256 `559f5b84…`（178,489,951 字节）——两件均连打两次
  逐字节一致、回下载一致）。收口**三条**（决策 **#191~#193**，均真机复验）：
  **#191（R86-9）提交失败后的补偿残渣进告警**：此前残渣（新建的表滞留、被摘掉的地址/特性没恢复）只出现在
  当次提交输出里，`show alarms active`/Web 总览页/诊断包事后全查不到，而数据面已与配置不同。修：提交编排
  注入提交期告警落点（`WithCommitAlarms`），补偿失败逐条报 `COMMIT_COMPENSATION_FAILED`（error，source =
  计划操作描述），文案带复原路径；同一对象下次成功执行即自动消警；另给 `vrf[X]`/`del-vrf[X]` 两个**复合
  操作**加 `compensateOnFail`（**失败时也执行 undo**）——真机实测删表读回失败时它已把该交换机的接口踢出
  VRF、清掉地址，而回滚不还原（配置说在 VRF 里，数据面在默认表且无地址，零报错）。
  **#192（R86-10）删表延后收敛**：NAT44 用过的表带 VPP `nat44-ei-hi` 引用，只有重启数据面才释放，于是同一
  candidate 内「改 NAT 出接口 + 删旧 L3 交换机」必然整体失败并回滚（`del-vrf` 读回报未收敛）。修：按既有
  「延后收敛」口径（同 #100/#186）——`del-vrf` 读到 `ErrVrfNotRemoved` 时**不阻断提交**，提交输出给
  预测式警告，另报 `COMMIT_VRF_DELETE_DEFERRED`（warning）+ `L3Provider` 待清理登记（恢复收敛/15s 巡检
  复核消警）；新增**残留表对账**：`EnsureConsistent` 用 `ip_table_dump` 找「配置未声明却在 VPP 里」的表 ⇒
  未收敛项 + `VRF_TABLE_LEFTOVER` 告警（**跨 nfvisd 重启仍可见**，不靠进程内记忆——这正是 R86-9「事后
  不可见」的根治口径）。手册 §8.8 改为「一次提交即可，`request vpp restart` 后自动清理」。
  **#193（R87-1，本轮新发现）接口换表后旧表登记残留**：同一提交内「把 ens192 改挂 vs-keep + 删旧交换机
  vs-old」提交**成功**，但 `vppctl show interface address` 里 ens192 无地址、不在 vs-keep 表（旧表成员清单
  还留着它，删旧表的解绑"照章"清地址并踢回默认表）。修：新增 `reassignLocked`（`ApplyVRF` /
  `RegisterL3Interfaces` / `SetVnfTable` 三处调用），写入某表前先把这些 sw_if_index 从其它表摘掉。
  **四套真机工具跑数（1.1.49~dev3，与 round36/86 基线逐项一致）**：`cli-fulltest` **194/1/12**
  （唯一失败 = 已登记的 `show vpp runtime`）、`cli-semantic-check` **12/0/1**、`cli-lifecycle-check`
  **21/0/3**、`cli-pty-smoke` **10/10**。
  ⚠️ **复跑套件前务必清上轮对象**（本轮实测：不清场时 lifecycle 只跑出 3/0/6——两个业务口被前一套件的
  vs-l3/bond0 占满，「自由口」类检查全跳过；清场后 21/0/3）。
  **Web 控制面黑盒**：总览页告警卡显示新码（截图 `docs/images/round87-web-console-alarm-overview.png`），
  与 CLI `show alarms` 及 `vppctl` 事实三方一致。证据
  `docs/evidence/v1-closeout-round87-r86-9-r86-10-residue-and-deferred-table-delete.txt`。
- **round86（干净快照从零走查 + 五条缺陷收口，2026-09-28）**（**首批**）：在恢复为「干净」快照的 nfvis-vm 上从零装工具链与
  VPP、源码构建 `1.1.48~dev1` 实装、wizard（真 pty）→ 重启 → 数据面 → VNF/容器/快照/诊断全面拟人化走查，
  收口**五条**缺陷（决策 **#183~#187**，均为真机复验过）：
  **#183（P0）** console 打开的 VM 串口 pty 曾是本进程的**控制终端**（打开时未带 `O_NOCTTY`）——此后
  **停那台 VM** 会让 nfvisd 收到 SIGHUP 而退出（调用方只看到 `Post …: EOF`、systemd 按 Restart=always 拉起，
  看不出根因）；修法是 `O_NOCTTY` + `internal/systemd.WatchHangup`（SIGHUP 免疫）+ 单元删掉并不存在的
  `ExecReload`（旧行为下 `systemctl reload nfvis` 实为"杀掉再拉起"）。
  **#184** `show log system` 与诊断包 logs.txt 在已装实例上恒空（查的是单元 `nfvisd` 而实际是 `nfvis.service`；
  又把 journalctl 的 `-- No entries --` 提示当成日志，回退分支永不触发）。
  **#185** 镜像 URL 拉取曾用 `http.Client.Timeout`（整体上限）——慢而持续的下载被掐断且归咎服务端；
  现改为「连接/响应头 60s + **空闲** 5 分钟」。
  **#186** 从零首次声明物理口时 VPP 未运行 → commit 整体失败并回滚（与手册 §7.3 的「延后收敛」承诺相反）；
  现对**已声明 DPDK 端口**同时延后 `ErrL2Unavailable`。
  **#187** NAT 用过的表带 VPP `nat44-ei-hi` 锁 → 该 L3 交换机删不掉（删规则/关插件都不释放，只有
  `request vpp restart`）；现把可照做指引写进错误文案并写进手册 §8.8。
  **第二批（同日，决策 #188~#190，PR #230 合并 `08f4f6a`；已发布 v1.1.48）**：清掉 round86 登记的三条未修——
  **#188** 告警随对象消失/状态恢复自动消解（对账式清警）+ 主动 `stop` 容器不再报 critical「异常退出」
  （改以 Docker `OOMKilled` 判异常，137/143 视为已停止）；**#189** 接口读视图给**有效 MTU**（配置优先、
  否则运行态，列表与详情共用一个视图）——控制台 MTU 列由恒「—」变真实值；**#190** 脚本模式「值未变化」
  改为**结构化提示**（`CLIExecuteResult.warning`）不再中止脚本，幂等脚本可重跑（套件不再需前置复位）。
  **四套真机工具跑数（1.1.48~dev2）**：`cli-fulltest` **194/1/12**（唯一失败=已登记的 `show vpp runtime`）、
  `cli-semantic-check` **12/0/1**、`cli-lifecycle-check` **21/0/3**、`cli-pty-smoke` **10/10**。
  **已发布 v1.1.48**（tag `v1.1.48` → `main 08f4f6a`；deb sha256 `2bd380d8…`、`.run` sha256 `2d8f0ffc…`；
  两件均连打两次逐字节一致，回下载 sha256 一致）。**新登记未修**：① 提交失败的**补偿再失败**时残渣只出现在
  当次提交输出、不进告警；② 同一 candidate 内「改 NAT 出接口 + 删旧 L3 交换机」因该 VPP 锁必然失败（拆两次提交）。
  **工具假红已修**：生命周期检查器的自由口枚举把产品命名的 bond（`bond0`）当物理口 → L1-2 恒假红。
  证据 `docs/evidence/v1-closeout-round86-fresh-walkthrough-and-5-fixes.txt`。
- M4 验收现状（`docs/M4-验收记录.md`）：M4-1~M4-11 真机通过（`make integration` 全绿）；M4-12 CLI 侧命令真机冒烟通过
  （show/request/delete 交互确认/console ticket/审计/动态候选）。**已知环境限制**：SR-IOV 无 PF/VF 未真机验证；
  容器侧 memif 通流未验（离线无自带 memif 的容器镜像）。
- M3 验收现状（`docs/M3-人工演示记录.md`）：D1/D2/D3/D6/D8 真机通过；**D4 NAT 已在本轮 M5 补齐并真机端到端通过**
  （决策 #52 跨 VRF：inside=virtual-switch 的 VRF、outside=出接口所属 VRF，VPP 单实例仅一对）；**D5 SPAN 抓包已在 T0-7 实证通过**；
  D7 LLDP 仍环境受限（无对端），启用与命令均正常、M3 的 internal error 未复现。
- 验证环境 nfvis-vm 当前状态（**2026-09-27 round85 又恢复过一次干净快照**，此后按 round85 流程重装并配置；
  快照恢复会清掉全部现场，
  重建路径见 `docs/evidence/v1-closeout-round32-install-iso.txt` §4e——该轮的 ISO 交付已按决策 #111 废除，
  文档仅作历史记录）：
  系统 Ubuntu Server 26.04.1 + **USTC 源**（aliyun 实测几乎不可用，勿切回）；构建工具按需装齐
  （Go 1.26.0（apt）、make、sshpass 等；`dpkg -i` 装 VPP 用 `/root/vpp-v26.06-deb/` 的 9 个
  26.06-release deb——快照基线自带）。
  源码树 `/root/src`（git archive 同步，见待办 §3.3，无 .git → 构建**必须显式传 SOURCE_DATE_EPOCH**）。
  **round81 后现状**（2026-09-26：决策 #155 `show` 族 JunOS 化第一步——接口族全运行态 + `show configuration | display set`
  全家族反推（回放自校验），已发布，证据 `docs/evidence/v1-closeout-round81-cli-junos-runtime-displayset.txt`；
  round80 基线见 `docs/evidence/v1-closeout-round80-cli-web-fulltest-and-release-1.1.34.txt`，
  三件套复跑仍以 `docs/evidence/v1-closeout-round36-three-suites.txt` 为准）：
  nfvis **1.1.47** active（**已发布 v1.1.47**＝决策 #180~#182 的离线一键安装包 `.run`（见下）；tag `v1.1.47` → `main 44a8749`；v1.1.46＝#179 已发布：删 VRF 的「假成功」——接口仍绑表时删表返回 0 却不生效导致空表滞留；先解绑再删表 + 删后读回校验；v1.1.45＝#178；＝决策 #178：静态路由的 apply 撤销缺口（`plan()` 补 `del-route`，同族 del-vrf/del-bond 的最后一处）；v1.1.44＝#177；＝决策 #177：`show nat` 会话改按用户 dump 汇总（此前恒报「无 NAT 会话」，实为取错事实）；v1.1.43＝#175/#176；＝决策 #175/#176：NAT 地址池的 tenant VRF 改取 inside 转发域（跨 VRF 带 source-pool 的 NAT 因此可用）+ `delete nat rules <seq> <叶子>` 只清叶子、空 `nat` 空壳回收；v1.1.42＝#172~#174；＝决策 #172~#174：VNF 的 vNIC 可作 L3 接口（guest 网关落在自己口上）+ NAT inside 覆盖 vNIC / VPP 重连按配置重建登记 / NAT 下发幂等 + 池带转发域 VRF——**「VNF 经 NFViS NAT ping 通 Windows 宿主」端到端打通**；v1.1.41＝#170/#171；＝决策 #170/#171：删 bond 的运行态撤销缺口 + **VNF 侧 vNIC 声明从未把 vhost 口挂进 bridge domain**（两条声明不等价、相关分支是死代码——round84「VNF guest 连通性未达」的真因，修后真机 ping 5/5 0%）；v1.1.40＝决策 #161~#169：非交互确认假成功 + 物理口多角色并存致数据面静默失效 + CLI 删 L3 交换机留 VRF 等九条；v1.1.39＝#159/#160；＝决策 #159 首装 AppArmor/快照 raw 排除 + #160 容器镜像命名闭环；v1.1.38＝#158 delete 反向写入、v1.1.37＝#157 多字节输入、v1.1.36＝#156 console TLS、v1.1.35＝#155 JunOS 化第一批；均回下载 sha256 一致）；
  **round83 生命周期走查**：干净快照装机上 VNF 全生命周期（含快照回滚）与容器 create/start/stop/delete 已验证，vnf-a running 存续、镜像 debian-12/alpine 双就绪、docker 代理 192.168.155.1:2333（daemon.json）；v1.1.37＝#157 多字节输入、v1.1.36＝#156 console TLS、v1.1.35＝#155 JunOS 化第一批；均回下载 sha256 一致）；
  **2026-09-26 快照恢复后从安装重建**：main `3ac5200` git archive 同步、VM 源码构建（与 Release asset 逐字节一致）；round83 从安装拟人化走查完成，证据 round81 文件 §14；**口令已变：`yaEBXZGjd_5ARJDf@Aa1`**；rev 3；v1.1.36＝#156 console TLS、v1.1.35＝#155 JunOS 化第一批）；
  ⚠️ 快照恢复后须重做：apt 源换回 USTC（aliyun 不可用）、make/golang-1.26/cloud-image-utils/qemu-utils 重装、
  libvirt/docker 按 provision.sh 清单安装（qemu-kvm 在 26.04 无候选）、**alpine.qcow2 与 docker alpine:3.20 未重建**；
  管理口令 `LQo79mRLbzhYje8y@Aa1`（**round85 首装时的一次性口令；随快照恢复/重装而变**，取法见待办 §3.1；
  round81 已按哈希比对法核对**匹配**）；
  VPP 26.06 运行（`request vpp restart` 拉起，主堆 2M 大页）、ens192/ens224 交 DPDK **且已在配置里声明**
  （`set interfaces … description` + `set vpp dpdk dev …`；只 bind-dpdk 不声明的话 VPP 里不会出现该口）；
  cmdline（round85 基线）：`default_hugepagesz=1G hugepagesz=1G hugepages=4 hugepagesz=2M hugepages=768 isolcpus=2-5 nohz_full=2-5 rcu_nocbs=2-5 irqaffinity=0-1 intel_iommu=on iommu=pt`；资源池 1G=4 / 2M=768，隔离核 2,3,4,5（VPP 占 2/3/4，空 5 给 VNF）；
  **round85 后现场：无 VNF/容器/交换机**（快照恢复 + 三件套跑完已清理；round84 的 vs-vnf/vnf-a/vnf-b 随恢复消失）；
  vnf-b 的 user-data 与 vnf-a 同址 .11，BVI↔vnf-b 无独立流量口径——历史如此，非本轮引入）；
  镜像：`alpine.qcow2`（阶段 3 前置，round85 已放回）+ 容器镜像 `alpine:3.20`（round85 已 `docker load` 放回）；
  `debian-12-generic-amd64.qcow2` **已随快照恢复丢失**（跑 `make integration` 前需重新准备）；
  配置库 rev 40+ / audit 若干（round85 的三件套会自己建删对象；**v1.1.48 起「值未变化」是提示而非失败（决策 #190）**，套件重复运行不再因此级联中止；
  但**上轮对象本身仍要清**（重复声明会撞其它校验：bond 成员口角色冲突、隔离核被已声明 VNF 占满等），
  并确认夹具镜像（`alpine.qcow2`）与 docker `alpine:3.20` 还在），
  `request interfaces ens224 enable` 走一次性事务 +1 rev/audit，**内容指纹未变**）；
  `/var/lib/nfvis/tech-support` 0700 + 归档 0600、`/var/lib/nfvis/backup` 0600（决策 #149 口径在既有安装上也生效）；
  **`nodejs` v22.22.1**（round37 装，
  仅用于 `node --check internal/api/ui/app.js` 校验 Web 前端语法——前端与 CI 都**不依赖** node）。
  ⚠️ 管理口令、`alpine.qcow2`、`docker alpine:3.20`、`rev/audit` 这四项**都会随快照恢复而变/丢失**，
  每次从快照重建后要重新核对并回写（round36 就撞上其中两条）。
  ⚠️ 集成测试环境（VPP 运行、镜像、1G 大页布局、ens192/ens224 交 VPP）随快照清掉——
  跑 `make integration` 前需先重建（布局与流程见待办 §3.3 / §0 第 1 条）。
  `ens160` 是管理口（vmxnet3、承载 SSH）——**永不拿管理路径做试验**的红线不变。
  设计基线在 `docs/`，**不要凭记忆重设计**。
- **round99（#317 回归收口：决策 #318，2026-10-01）**：round98 真机在 `2.0.0~dev11` 上抓到 #317 引入的**锁泄漏**
  ——`cli-fulltest` 198/0/11 → **195/3/11**、语义 12/0/1 → **10/1/1**，签名同为
  `%% candidate 会话锁被占用: 由 admin@ssh 持有`，现场 `sessions` 躺着 `{holder:"admin@ssh", dirty:false}` 的**干净锁**。
  调查结论（谁留的锁）：`commit and-quit` 提交后把 CLI 本地模式改回 oper 却**从不释放引擎锁**，而客户端收尾只在
  config 模式才 discard/exit；登出也清不掉它——登出请求的会话是 `user@api`，锁的键是 `user@ssh#<token>`（#317 按 token
  拆开后不再被下一次调用「同身份键幂等」接管）。**决策 #318** 两条口径：① **干净锁不排他**——`dirty=false` 时同用户的
  新会话可直接接管（`dirty=true` 严格排他、跨用户仍被拒、被接管者再操作报 `ErrLockLost`）；② **路径级释放**——
  `commit and-quit`（成功才退出并释放）、操作模式 `exit/quit`、新增 `Engine.DiscardSession(user,id)`（登出/吊销**不区分
  接入源**）、干净锁用更短的空闲阈值（默认 1 分钟，复用既有巡检）。无 schema 迁移，是**收敛修正**不是回退。
  证据 `docs/evidence/v2-round99-d318-clean-lock-takeover.txt`。
- 已定决策 222 项见规格书附录 A（main/1.x 线 #1~#201；本仓库当前在 **v2/2.x 开发线**，决策自 **#300** 起、
  #202~#299 为 main 预留号段，双线发版约定见决策 #300，v2 线已有 #300~#320）——实现中遇到"该怎么做"的问题，先查附录 A，不要重新发明。
  **Web 控制面**：V1 不含（规格书 §12 V2 候选），已于**决策 #115** 启动 V2 增量 1——
  只读总览，内嵌进 nfvisd 同源托管于 `GET /api/v1/ui/`，前端**免构建**（原生 HTML/CSS/JS，无 npm）。
  新增端点/读物类型时必须同步：OpenAPI 契约、`routes_contract` 守护、`user_text` 守护（`.html/.js/.css`）。
  **响应形状**（决策 #116）：契约声明的响应字段必须真的发得出来，由 `shape_contract_test.go` 守护
  （条件出现的字段进显式白名单并写明理由）；契约 YAML **不得有重复键**（PyYAML 静默取最后一个，
  由 `check_openapi_no_duplicate_keys.sh` 守护——R37-1 的 VppStatus 就是这么藏住的）。
  **round39 已做 Web 控制台可视验收**（Browser Use 插件黑盒：截图 + 只读 DOM 双向印证），
  收口三处缺陷（决策 #117）：CSS `hidden` 属性被作者 display 覆盖（登录后登录卡片不消失）、
  `/login` 响应 `user` 未按契约回对象、`/system/status.uptime_seconds` 取的是守护进程而非主机
  运行时长；形状守护已扩到 `POST /login`。**结论：任何"已交付但没人眼看过"的界面，验收都不算完**。
  **界面交付的验收口径（决策 #141，2026-09-23 起生效）**：控制台按"资源域一级 + 对象详情二级"重构
  （定位 = CLI 的图形等价物、全图形鼠标操作、维持免构建、分级确认），设计文档见
  `docs/superpowers/specs/2026-09-23-web-console-ia-design.md`；**每个交付的功能都要在浏览器里
  真的操作一遍**（Browser Use 黑盒：操作前后 DOM 快照 + 关键步骤截图 + **与独立事实源对照**——
  界面显示对了不算完，要证明它真的改到了底座，如点"停止 VM"后 `virsh domstate` 确实变了）。
- **V1 验收收口**：`docs/V1-验收检查表.md` 把规格书 **109 条 FR** 逐条对照证据
  （**通过 101 / 未验 4 / 降级 2 / 移 V2 2**；2026-09-18 收口：NFR-005/NFR-006 转通过、FR-SEC-006 拆两半），降级理由与签字建议见其 §5/§6；
  **待办与未完成项的唯一入口见 `docs/V1-收尾待办.md`**（含环境要点与踩坑记录）。
  已发布 **v1.0.0 / v1.1.0 / v1.1.1 / v1.1.2 / v1.1.3 / v1.1.4 / v1.1.5 / v1.1.7 / v1.1.8 / v1.1.9 / v1.1.10 /
  v1.1.15 / v1.1.19 / v1.1.20 / v1.1.25 / v1.1.26 / v1.1.27 / v1.1.28 / v1.1.29 / v1.1.30 / v1.1.31 / v1.1.32 / v1.1.33 / v1.1.34 /
  v1.1.35 / v1.1.36 / v1.1.37 / v1.1.38 / v1.1.39 / v1.1.40 / v1.1.41 / v1.1.42 / v1.1.43 / v1.1.44 / v1.1.45 / v1.1.46 / v1.1.47 /
  v1.1.48 / v1.1.49**（见 GitHub Releases；**跳过 v1.1.6**——那次发布已撤回、其提交不在 `main`，
  以及 **1.1.11~1.1.14、1.1.16~1.1.18、1.1.21~1.1.24**——同一 merge 线上的内部验证构建、从未发布，
  故由 v1.1.10 跳到 v1.1.15、v1.1.15 跳到 v1.1.19、v1.1.20 跳到 v1.1.25；v1.1.26 紧接 v1.1.25、v1.1.27 紧接 v1.1.26、v1.1.28 紧接 v1.1.27、v1.1.29 紧接 v1.1.28、v1.1.30 紧接 v1.1.29、v1.1.31 紧接 v1.1.30、v1.1.32 紧接 v1.1.31、v1.1.33 紧接 v1.1.32，无跳号）。
  **用户文档**：`docs/NFViS-用户手册.md`（安装→使用全流程）、`docs/NFViS-CLI命令全表.md`
  （256 条命令 + 逐条真机实测状态）；真机手动脚本：`contrib/scripts/cli-fulltest.sh`（问「命令能不能用」）、
  `contrib/scripts/cli-semantic-check.sh`（问「结果对不对」）、`contrib/scripts/cli-pty-smoke.sh`（交互行为）。
- `docs/NFViS-openapi.yaml` 与 `docs/NFViS-CLI命令树完整设计.md` 是**契约**。

## 不可违反的规则

1. **契约先行**：改 CLI 命令、API 端点、资源模型，必须先改 docs/ 下对应契约文档并在规格书附录 A 追加决策记录，再写代码。CLI 命令树、OpenAPI 路径、附录 B 映射表三处必须同步。
2. **需求可追溯**：实现某个功能的提交/PR 必须引用 FR-xxx 编号。
3. **目录结构**：产品代码按 `docs/NFViS-Go工程目录骨架设计.md` 的布局（module 为 `github.com/xzjt/nfvis`），依赖方向规则（CLI 前端不得 import 事务引擎）由 `internal/archtest` 自动守护、`make check` 执行——违规即测试失败。`prototype/` 是演示代码，禁止把产品逻辑写进去。
4. **测试门槛**：`internal/schema` 与 `internal/config`（事务引擎）单测覆盖率 ≥ 70%；底座交互（govpp/libvirt/docker）必须藏在 Provider 接口后，单测用 mock。
5. **换行符 LF**（.gitattributes 已强制）；提交信息用中文，格式 `<type>: <摘要>`，type ∈ docs/feat/fix/chore/test。

## 每次改动后的自检清单

```bash
make check        # 全量测试 + vet + 覆盖率门槛 + 依赖方向守护 + 决策条数一致性 + 原型全绿
cd prototype && go build ./... && go vet ./... && go test ./...   # 原型仍应全绿（make check 已含）
python -c "import yaml;yaml.safe_load(open('docs/NFViS-openapi.yaml'))"  # 契约可解析
grep -rn "待评审\|TBD\|TODO" docs/   # 不允许引入未决标记
```

**改了会被"读"出去的行为后**（`show` 族、候选来源、任何"取哪份事实"的改动），除 `make check`
外还须在真机上跑这两项（CI 跑不了，二者互补）：

```bash
bash contrib/scripts/cli-fulltest.sh        # 「命令能不能用」：256 条契约命令，见 %/%% 即失败
bash contrib/scripts/cli-semantic-check.sh  # 「结果对不对」：与 VPP/内核/libvirt 独立事实源对照 + 扰动判别
```

**改了交付/安装路径后**（`deploy/debian/*`、`deploy/offline/`、离线包构建脚本），另跑（Linux + dpkg-deb；`make check`
已含其中的守护与自校准）：

```bash
bash contrib/scripts/offline-closure.sh check <deb目录>                      # 依赖闭包自洽（离线可跑）
bash contrib/scripts/offline-installer-selftest.sh --run build/nfvis-vX.run   # 产物结构：载荷 sha256/索引/清单/版本
```


**真机测试必须按「四维覆盖口径」跑（2026-09-27 round84 固化，见待办 §3.10）**：① 作用效果（底座真变了没有，
要有独立事实源）② **生命周期**（重启/重放/漂移之后还成立吗）③ **跨对象组合**（VNF↔交换机↔NAT 这类组合）
④ 对抗性等价对照（两种「等价」写法、两份事实源）。第 ②③ 项与「删/改后回读」由
`contrib/scripts/cli-lifecycle-check.sh` 承担——**它首次运行就抓到一条既有缺陷（删 L3 交换机后静态路由残留）**；
语义校验里**空结果一律进「不可判定」、不计入通过**（此前「两侧都空也算一致」是假绿）。

**新功能须同步入套件、或登记豁免（由 #319 的守护强制）**：契约里可执行的 CLI 命令要么出现在
`cli-fulltest-phase*.sh` 的命令清单里，要么逐条登记进 `contrib/scripts/cli-fulltest-exemptions.tsv`
（三列：命令 / 类别 / 理由，理由须说明**改由谁覆盖**）。守护 `contrib/scripts/check_suite_contract_sync.sh`
（自带桩式自校准）随 `make check` 的 `toolcheck` 跑——**只进实现不补套件的命令会被它挡住**（v2 线的
`show system api tokens`、`set/delete system login banner` 就是这么漏进过的）。四套件基线（v2 线，见 #319）：
`cli-fulltest` **210/0/13**（实测修正：2026-10-01 round101 真机为 210/0/13——设计里的 212 系逐阶段增量相加的算术偏差）、`cli-semantic-check` **24/0/1**、`cli-lifecycle-check` **21/0/3**、`cli-pty-smoke` **10/10**；
套件数字的每一处变化都要写清「哪条新增/移除、为什么」（#304 那次 195/0/12→198/0/11 的漂移是教训）。

后者是唯一能发现「命令成功但答非所问 / 取自配置而非运行态」的那层（决策 #84/#85）——
2026-09-15 它一次跑出 4 类契约违反，而同期 256 条冒烟是 197 全绿。

- **「假绿」有两层，都要防**：① **实现层**——命令没做成就别报成功。`ping` 一度把 vppctl 原始输出
  （含 `Statistics: 0 sent, 0 received, 0% packet loss`）原样返回且不报错，判定只看 `^%` 与退出码，
  于是**一个包都没发出去被算作通过**（决策 #89）；修法是「0 发包即报失败」，并让冒烟断言**判定自洽**
  （0 发包必须有 `%%`）而不是去猜环境。② **证据层**——`$CLI … | grep …; echo $?` 拿到的是 **grep 的**退出码；
  `OUT=$(cmd | grep …)` 之后 `$?` 同理。量退出码**不经管道**、重定向到文件再读。
- **换二进制做对比前，先确认谁在服务**：开发态实例叫 `/tmp/nfvisd-old` 时进程名是 `nfvisd-old`，
  `pkill -x nfvisd` **不命中**，新进程绑定失败、请求仍打到旧实例上——本轮因此得到过一组
  「新旧结果完全一样」的假对比。用 `for p in $(pgrep -f nfvisd); do readlink -f /proc/$p/exe; done` 确认。
- **拿标题/锚点做替换，改完要 `grep` 一次锚点还在**：决策 #88 的编辑把 `## 附录 B：…` 当锚点替换掉且没带回来，
  **v1.1.8 的包内规格书因此少了该标题**——`make check` 全绿、测试全过，因为没人检查文档结构。

- **防呆/守卫要验证它"真的生效"，不能因为"没触发"就假定存在**：2026-09-18 有会话要绑管理口
  `request interfaces ens160 bind-dpdk`，**第一次因 `vfio-pci` 未加载而失败，被误当成"守卫挡住了"**，
  于是第二次直接**把自己锁在门外**（`bind-dpdk` 把承载 SSH 的网卡交给了 DPDK，SSH 当场断，
  只能带外重启恢复）。✅ **2026-09-18 已补守卫（决策 #101 / 发现 #7）**：
  `dpdkController.SetDPDKBound` 在**任何 sysfs 动作之前**判定，命中即拒绝、**不提供 force 出口**；
  判据是三条高置信度事实（配置声明的管理口 / 承载默认路由的口 / 守护进程监听地址所属的口），
  启动日志有「管理口守卫事实」一行可核对。
  **仍然：永远不要拿管理路径做试验**：先用非管理口验证守卫与流程，或先声明管理口再用内核事实
  （默认路由 / SSH 源地址 / 有 IP 的口）确认哪个不能碰。
- **随包文档是发布件的一部分，发布前要核对它是不是当前版本**（2026-09-22 round48，R48-5）：
  v1.1.26 首切件的**用户手册 §10.12 还写着「Web 界面（只读总览）」「本页面不修改任何配置」**，
  而该版控制台已能改配置与做诊断——round43~47 只更新了契约/测试/证据，漏了手册。
  **发布校验要加一条「随包文档 vs 实现」的核对**；发布后若必须改随包文档，照 round29 先例
  **重切发布件**（重建 → `dpkg-deb -x` 逐文件对照证明"只有文档不同、二进制逐字节相同"→
  删原 release/tag 重建 → 回下载比对），并核对 asset 名（别把本地文件名后缀带进 asset 名）。
- **裸机/新装走一遍应作为发布前门槛**：2026-09-18 从零装一遍才暴露三条既有守护**完全看不到**的缺陷
  （#8 业务口交 VPP 的路走不通、#9 冒烟对 `show log audit` 的判定依赖环境历史、
  #10 `set system api tls self-signed regenerate` 后 CLI 因证书缺 IP SAN 而全断），
  而单测/守护/256 条冒烟在旧环境里全绿。**"装一遍"与"跑测试"不是同一件事。**
- **判定"失败"前先确认判据本身是对的，且先看级联关系**（2026-09-22 round36）：
  冒烟那 26 条失败**全是前置级联**（1G 大页池被已声明 VNF 占满 → 产品**正确地**拒绝再分配 cli-vm；
  阶段 2 预块第 1 条是"值未变化"的空操作 → 而 `-c` 脚本模式**任一行失败即停止** → 其后语句全没执行），
  语义校验那条失败是**脚本自己的 oracle 错了**（`vppctl show l2fib` 非 verbose 只有汇总行、
  `vppctl` 输出是 CRLF 未剥）。**顺序：级联关系 → 独立事实源（`vppctl` 原样输出/配置库）→ 才怀疑产品。**
  要拿"套件本身是否全绿"的结论，就跑**干净开发态基线**（round36：194 通过 / 1 失败，
  唯一失败是已登记的 `show vpp runtime`）。
- **工具假红也是缺陷，要修并加自校准**：语义校验的 oracle 修好后加了 `cli-semantic-selftest.sh`
  （桩 vppctl、CI 可跑，已并入 `make check` 的 `toolcheck`）——语义校验本身只能在真机跑，
  只有桩式自校准能在 CI 挡住 oracle 回归。（2026-09-22 round48 又撞一次：发布校验的 HTTPS 探针
  **漏传 `--cacert`** → curl exit 60、HTTP code 000，而且**失败时输出文件根本不落盘**，
  读起来像"所有端点都挂了"；修法见 round48 证据 R48-1。**写探针要先对已知可达的目标自校准**。）
- **口令/镜像这类"外部事实"不能只靠文档维护**：快照恢复会换掉管理口令、清掉冒烟阶段 3 的两个前置
  （`alpine.qcow2` 与 `docker alpine:3.20`）。每次从快照重建后**重新核对并回写**，别照抄旧值；
  核对口令**不要反复试登录**（5 次失败锁号），直接读配置库哈希比对（方法见待办 §3.1）。
- **停/起 VNF 后 guest 内的网络配置不会自己回来**：user-data 内容没变 → instance-id 摘要没变 →
  cloud-init 按 once-per-instance **不重放**，而 guest 启动时的网络栈只拿到 DHCP 地址。
  复通办法是**改一次 user-data 内容**再 `restart`（待办 §0 红线 12）。

## 外部文档查询（context7 MCP，个人启用）

涉及以下**版本敏感**的第三方接口时，优先用 context7 查询官方文档核对，不要凭训练记忆写调用代码：

- govpp binary API 结构体/消息（VPP 26.06 的 binapi 与旧版差异大）
- libvirt domain XML 元素与 go 绑定
- VPP 插件（acl/nat/memif/lldp/bonding）的 startup.conf 参数与行为
- Docker Engine API 字段
- SQLite 驱动（modernc.org/sqlite）的 DDL/事务语义

注意两点：
1. context7 对垂直领域库（govpp/VPP）覆盖可能不全——查不到时**以官方源码和版本锁定的文档为准**，不得用"记忆中的旧版 API"替代。
2. 底座行为的最终裁决标准是 nfvis-vm 上实际安装的版本（`vppctl show version`、`libvirtd --version`），文档与实测冲突时以实测为准并在规格书附录 A 记录差异。

## 常见错误（历史实际发生过，勿重蹈）

- 只改引擎执行逻辑没改命令树（或反之）→ `?`/Tab 与实际行为漂移。命令树与执行器必须同源。
- **守卫/校验只加在 HTTP handler → CLI 能绕过**（CLI 经 `api.cliExecutor` 直接调 Provider，不经 handler）。
  新增任何前置校验（权限/状态/参数）必须落在**两侧共同依赖的实现处**（如 `cmd/nfvisd/main.go` 的装配层），
  handler 层可保留同检查作纵深防御。2026-09-14 快照关机态守卫初版即犯此错（决策 #75）。
- 快照在 commit 后才拍 → rollback 语义错误。快照必须在覆盖 committed **之前**拍。
- show 命令的 case 顺序把无参提示放在最前 → 单参数命令被拦截。
- 参数节点双重语义（实例名 `<name>` 入路径 vs 取值 `<ip>` 不入路径）混淆 → cfgSet 解析错误。
- **给操作者看的文本里写需求编号**（`提交 candidate（FR-CFG-002/003）`、`%% …（FR-NET-012）`）：
  操作者读不懂，且掩盖了「这条消息到底说明了什么」。**需求可追溯（规则 2）靠注释与 `docs/`**，
  不靠用户可见字符串——`internal/archtest/user_text_test.go` 已守护（决策 #86），
  **判据是「这段文本会到达操作者吗」而非文件后缀**（决策 #87）：`.go` 字符串字面量、`.sh`/`.service`/
  `Makefile` 的非注释行、`deploy/` 下的一切（`postinst`/`prerm`/`postrm` **没有后缀**，`dpkg -i` 会直接打印）、
  以及随包安装的 `docs/NFViS-用户手册.md` 全文；**注释与设计类 `docs/` 里的引用照旧保留**。
  同批教训：**5 处测试断言写的是「消息里应含 FR-xxx」**，等于把泄漏固化成期望——测试别断言实现细节。
- **交互行为只用管道（非 TTY）演示 → 交互缺陷全部漏网**。管道下 `term.MakeRaw` 失败、退化 `readPlain`，
  raw 模式关闭终端 `OPOST` 后「裸 `\n` 不回车」的错位、以及只按键不回车的行为（`?` 即时列候选、Tab 多匹配列出）
  都**不会出现**（决策 #81）。交互行为必须在 **pty** 下验证：`script -qec '<cmd>' /dev/null`，
  并用 `cat -A` 核对行尾是 `^M$`（CRLF）而非裸 `$`（LF）；可复跑的手动冒烟见
  `contrib/scripts/cli-pty-smoke.sh`（10 项断言，修复前失败 7 项）。
