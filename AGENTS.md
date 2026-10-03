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
- **v2 线收口两则（决策 #321/#322，2026-10-01）**：清掉 v2 待做 二.10（R84-22 恢复收敛顺序）与 二.11
  （补偿残渣告警无消解路径）。**#321**：非 VRF 表类残渣（ACL / bridge-domain）并入决策 #192 的同一份
  对账视野（新增 `ACL_LEFTOVER` / `BRIDGE_DOMAIN_LEFTOVER`，`L2Network.ReconcileResidue` 供
  `EnsureConsistent` 与 15s 巡检共用），按数据面事实**重建**告警（不落盘，跨 nfvisd 重启仍可见）、
  对象消失自动消解；提交期 `COMMIT_COMPENSATION_FAILED` 在对象已对得上配置时由对账消解（其余类别
  如实不猜测）。**#322**：恢复收敛在重放 vNIC 接入**之前**用 `L3Provider.PrecreateTables` 预建声明表
  （声明集由 `DeclaredTables` 单一真源给出，与残留表对账共用），首次收敛即「先建表、再置接口/成员」，
  不再出现 `No such FIB / VRF (-3)` 未收敛项。两决策均**契约先行**（附录 A）、`AGENTS.md` 决策条数
  222→224，用户手册告警表补两码。真机验证步骤见交付说明（待执行）。
- **v2 线收口两则（决策 #323/#324，2026-10-01）**：**#323 套件收尾清场**——`cli-fulltest` 跑完
  **自己清干净**（套件入口拍「开始前现场」快照 + 末尾自动清场 + EXIT/INT/TERM trap 兜底；
  `--cleanup-only` 仅供异常中断后补救）；凭什么认定套件自己的对象：固定名带套件特征的对象
  （`vs-l2/vs-l3/acl-test/pol-test/cli-vm/cli-ct2/cli-*.qcow2`）恒删，通用名对象（`bond0`、容器镜像
  `alpine:3.20`）**只删本轮开始前不存在的**，打标字段（`description cli-pre`、主机名 `cli-tx-*`）按值识别；
  清场失败逐条如实报、退出码非零。**#324 operator 角色粒度**——服务端权威 class 经既有 `POST /login`
  的 `user.class` 下发（不新造权限模型）；`GET /cli/candidates` 与 CLI `?`/Tab 候选按 `aaa.Authorize`
  同源过滤（预置档等级映射单源在 `schema.PresetClassLevel`，`aaa` 与 CLI 共用），无权入口不列出、
  执行路径的既有拒绝不变（纵深防御）；Web 控制台按 class 切 `body.role-nonsuper`、隐藏未带
  `data-op` 的写入口（#145 的 operator 缺口）。两决策均**契约先行**（附录 A）、`AGENTS.md` 决策条数
  224→226，`docs/v2待做.md` 二.3 标为已收口。真机验证步骤见交付说明（待执行）。
- **v2 线收口两则（决策 #325/#326，2026-10-01）**：**#325「秘密出口」全量复查**——枚举「出口 × 敏感项」
  并逐条分类，抓出并修掉**两处真泄漏**：① CLI `show configuration | display json|xml` 直接取自**原始配置树**
  （`x.structured`），只读账号即可读走全部用户口令哈希（`display set` 自带剥离、json/xml 没有）——修法是在
  `internal/api/pipes.go` 渲染前经 `model.RedactSensitive`；② CLI `show log system` 与 `GET /system/logs`
  泄露产品首启打印的**一次性口令**（`nfvisdLogTail` 的 journal 原文，#149 只剥了归档）——修法是把剥离实现
  导出为 `system.ScrubBootstrapCredential`（唯一实现）并在装配期用 `scrubLogSource` 包裹日志来源。
  守护落成 `internal/api/secrets_egress_test.go` 的**出口矩阵** + **穷尽 GET 路由分类**（新增出口未分类即失败）；
  例外清单每条写明理由（备份下载/save 导出/抓包/guest 日志/user_data 自由文本）。
  **#326 交换机成员端口读视图**——VNF/容器 `interfaces <nic> virtual-switch <vs>` 声明的 vNIC 作为**派生条目**
  并入端口列表（`source=config|vnf|container|runtime`），**配置库 `Config` 结构不动**；三面同源
  （CLI `show virtual-switches <vs> ports` / REST `GET /virtual-switches/{n}/ports` / Web 详情页新增「来源」列）；
  派生条目只读，`delete … ports` 对派生条目**拒绝并指向 VNF/容器侧**（顺带修掉「删不存在序号会凭空建空壳端口」）。
  两决策均**契约先行**（附录 A）、`AGENTS.md` 决策条数 226→228，`docs/v2待做.md` 二.4 与 二.8 标为已收口。
  真机验证步骤见交付说明（待执行）。
- **v2 线收口（决策 #327，2026-10-01）**：**运维动作页写控件按 class 收敛（修 round105 登记的 R105-1）**——
  round105 用 Browser Use 以 operator 登录看到运维动作页仍渲染 super-user 级写入口（软件版本卡、内核基线卡）。
  本轮**先用无头 Chrome 加载真实 `index.html`+`style.css`（`body.role-nonsuper`）取 `getComputedStyle` 复核**：
  #324 的既有机制**有效**（Su 控件 computed `display:none`，仅 `data-op` 的 O 级入口可见），静态控件的 class
  本就合规；**真正漏点是动态写控件未经 `wbtn` 打 `data-write`**——整份 `app.js` 排查抓到唯一一处：**镜像列表的
  「删除」**（`DELETE /images/{name}`，super-user 级）用 `el('button')` 生成，read-only/operator 都看得见、点下去
  403。修法走**既有机制不新造第二套**：该处改 `wbtn`；`data-op` 例外 7 条**逐条复核确为 O 级**（诊断 ping/traceroute、
  core dump 清单导出，端点均 `ClassOperator`）。**守护**：新增 `internal/api/ui_ops_writes_gating_test.go`——把运维
  动作页家族（`#/ops/actions`、`#/system/kernel`、`#/system/tls`、`#/ops/capture`、`#/ops/diagnostics`，39 个控件）
  逐条列成 `id→期望 class（hide/op/read）`清单，并**从 `server.go` 现读「控件触发端点→服务端 class」交叉核对**
  （新增/删除控件或标错 class 未同步清单即失败）；另一条断言**动态写入口必须经 `wbtn`**。**说明性文字与控件分清**
  （判据是「有没有可点的写控件」，卡片解释文字对 operator 保留可见，本轮未发现有诱导点击的文案）。契约先行（附录 A）、
  `AGENTS.md` 决策条数 228→229、`docs/v2待做.md` R105-1 标为已收口；**真机验证（Browser Use，operator）待执行**。
- **v2 线收口（决策 #329，2026-10-01）**：**大页池「无主占用」的回收路径**（清 `docs/v2待做.md` 二.6）——
  `show system hugepages`（及 REST/控制台资源池页）此前会显示「期望 N 实际 M」（M>N）却**没有回收路径**（只能重启或手工 sysctl）。
  成因已由 #199/#201 消掉，但历史遗留/手工设置/早期版本升上来的机器仍可能带着多余页。**口径**：① **三方数字如实呈现**
  （声明值=配置唯一真源 / 内核实际=sysfs / **在用值**=实际−空闲 / **可回收**=空闲的多余页；取不到就如实说取不到）；
  ② **自动**＝在**既有 60s 系统巡检**里对账式收敛（未新造定时器），**手动**＝新增 `request system hugepages reclaim`
  （class 与 `request system kernel apply` **同档 = S**）；③ **在用页一律不动**（红线，绝不抽走 VPP/VNF 的大页）；
  ④ 回收**有界**（每池每轮至多一次写 + 一次**回读**）、**写成功 ≠ 收敛**；无法回收时报出**谁在占用**（配置账本 / VPP 页尺寸偏好 /
  hugetlbfs 挂载三类可查证据，查不到就如实说明）；⑤ 只收敛到**已声明**值，**不改声明值**（改声明仍是 `set resource-pools hugepages … count <n>`，需 reboot）。
  **三面同源**：CLI `show system hugepages` / `request system hugepages reclaim`、REST `GET /system/hugepages` /
  `POST /system/hugepages:reclaim`、Web「资源池」页（三方数字表 + 回收按钮，按既有 `wbtn`/`data-write` 门禁）。
  收敛不掉时以 `HUGEPAGE_POOL_SURPLUS`（warning，跨重启按内核实况重建、收敛后自动消警）如实呈现。契约先行（附录 A #329）、
  `AGENTS.md` 决策条数 230→231、`docs/v2待做.md` 二.6 标为已收口；命令全表/命令树设计/openapi/形状与 UI 门禁守护均同步。
  **真机验证（造「无主占用」现场：root 抬高 1G 池 nr_hugepages → show/reclaim/回读/Web 三面）待执行**。
- **v2 线收口两则（决策 #330/#331，2026-10-01）**：**#330 IPv6 诊断 + 支持矩阵如实化**（清 `docs/v2待做.md` 三.8、三.4 部分）——
  新增 `ping [ipv6] <host> [source|count|vrf]`（`vppctl ping ipv6`；命令树把 `ipv6` 建成可选无值叶子关键字、与 `<host>` 平级，
  `?`/Tab 可补且「参数组」枚举器不错序；执行器容忍 `ipv6` 在目标前后两种顺序，既有 v4 写法不变）与
  `traceroute [ipv6] <host> [vrf]`（**先确认 raw ICMPv6 可行**——nfvisd root 可开 `ip6:ipv6-icmp`，`trace6` 与 v4 同构后落地）；
  **未通即失败对 v4/v6 同口径**（0 发包 / 0 应答都报错、判不出汇总行不判失败），提示语按 v6 口径；
  `traceroute` 的 `vrf` 对 **v4/v6 都明确拒绝**（不静默降级）+ 给替代；命令树设计 §1.3 与用户手册给
  「命令 × 平面 × v4/v6 × vrf」支持矩阵小表；三面同源（CLI `?` 候选 / REST `POST /diagnostics/ping|traceroute` 加 `ipv6` 布尔 /
  Web 诊断页 `diag-ipv6` 勾选、走既有 O 级 `data-write data-op` 门禁）。**#331 QoS 出向限速绑定**（清 `docs/v2待做.md` 三.5）——
  新增同族语句 `set/delete interfaces <ifname> egress-policy <name>`（与 `ingress-policy` 同一参数化别名实现，仅模型字段不同），
  数据面**复用同一 VPP policer**、经 `PolicerOutput`（`policer_output`）装成 output feature，与入向 `PolicerInput` 对称；
  `ApplyInterface` 入/出两向**各自独立**比较增删（`swapBindings`）、`DeleteQos` 先解两向再删；校验沿用同一策略账本、
  删除被引用策略**入向/出向都拒**并分别指明方向；读视图能看出方向（`show qos policies`/`GET /qos/policies` 加 `bindings` +
  `bound_interfaces`，接口详情显示「QoS: 入向 X / 出向 Y」）。两决策均**契约先行**（附录 A）、`AGENTS.md` 决策条数
  231→233、`docs/v2待做.md` 三.5 收口 / 三.8 收口，命令全表/命令树设计/openapi/UI 门禁守护同步。
  **真机验证（待执行）**：#330 走 v6 可达/不可达两路与既有 v4 回归；#331 用 VNF/流量造现场、同一策略做「出向小 cir 实验组
  vs 不绑定对照组」，以 `vppctl` 计数/丢包为独立事实源判定限速生效（**不得只看命令成功**）。
- **v2 线收口（决策 #332，2026-10-01）**：**Web QoS 策略卡「类型/目标」恒「—」（R110-1）**——round110b 用
  Browser Use 发现策略有出向绑定（读视图 `ens224:out`）时网络对象页 QoS 卡两列仍「—」。根因是**字段级**：
  该卡自决策 #128 起取 `q.type`/`q.target`/`q.interface`，而 `/qos/policies` 响应里**从来没有**这些字段
  （#331 前后都不存在）——「类型/目标」自始恒「—」，#331 三面同源补了 CLI/REST/详情页/接口详情，漏了这张列表卡；
  路径级覆盖守护（`ui_coverage_test.go`）挡不住「端点对、字段错」。修法：卡列改与 CLI `show qos policies` 同源
  （`名称 / CIR(bps) / 绑定（接口:in|out）`），绑定文本抽 `qosBindingsText` **单源**（列表卡与详情页共用）；
  新增**字段级守护** `internal/api/ui_qos_card_fields_test.go`（卡片消费的 `q.*`/`b.*` 字段逐一对照 openapi
  QosPolicy schema，含 `bindings.items`，解析器带自校准）。openapi **不改**（`bindings` 已是 #331 契约）。
  `AGENTS.md` 决策条数 233→234。**真机复验已完成（round111，Browser Use）**：当前构建确认（页面加载的
  app.js 含新代码）、列表卡 `pol-web 8000 ens224:out` / 无绑定策略如实「—」、详情页与接口详情页方向一致、
  与 CLI/REST 三方对照一致；证据 `docs/evidence/v2-round111-d332-qos-card-r110-1-fix.txt`。
- **round111b（#330/#331 真机验证收口 + 新登记两条，2026-10-01）**：交接清单 §3.2 第 2/3/4 条全部收口。
  **#331 限速定量对照通过**：实验组（cir 8000 绑 bvi0:out）policer conform ≈4.94 kbps ≤ cir、violate 丢包、
  bvi0 tx 18.4→6.4 pps；解绑后计数冻结、速率复原——三段窗口唯一变量是绑定状态；如实登记 ICMP 自钟摆
  效应（offered 降速，后续用 UDP/iperf）与「BVI tx_drops 不计 policer 丢包」。**#330 可达路径成立**：
  BVI v6 走 `set virtual-switches <vs> gateway ip <v6-prefix>`（**没有** `interfaces <if> address`），
  `ping ipv6 2001:db8::2 vrf <交换机VRF>` rc=0；候选两面（REST/pty）确认含 `ipv6`，探针测试
  `TestCLIDiagIPv6Candidates` 入仓（PR #258）；经验：非表内目标 ping 默认查表 0 → `0 sent`，
  判不可达建议带 vrf 复核。**套件复跑**：语义 **25/0/0**（不可判定 0）；lifecycle **19/0/4**（0 失败；
  首跑 L2-3 失败系 round110 遗留 `interfaces bvi0` 空壳声明，删除后转绿）。**新登记**：
  **R111-1**（中，根因已定位）恢复收敛告警族只在 VPP（重）连时重估——声明已删/口已出现都不消警
  （`runRecovery` 只挂 `OnConnect`，15s 残留对账有意只动残渣码），修法方向见 `docs/v2待做.md` 二.21；
  **R111-2**（低）lifecycle L3-2 邻居表查询不按转发域取（#328 同族套件 oracle 候选），见二.22。
  证据 `docs/evidence/v2-round111b-d331-d330-suites-and-observations.txt`。
- **round112（R111-1/R111-2 收口：决策 #333/#334，2026-10-01）**：**#333 恢复收敛告警族随事实消解**——
  R111-1 实测「口已出现在 VPP」「配置声明已删」两种事实变化都不消警、直到 VPP 重连才消（`runRecovery`
  只挂 `OnConnect`，15s 残留对账有意只动残渣码）。修法**不做周期性全量重放**：新增
  `L2Network.ReconcileRecoveryAlarms`（15s 巡检与 `ReconcileResidue` 同块调用），对 recovery 作用域
  活动告警逐条**按来源廉价复核**：来源对象已不在 committed 配置 → 消解（逐族映射；family 级与不可解析者
  保守保留）；`RECOVERY_IFACE_MISSING` 且来源 `interfaces/<n>` 且配置仍声明 → 用既有 `SwInterfaceIndex`
  查口是否已在 VPP，已出现即消解；其余（UNCONVERGED 等）保守保留不猜。告警码/形状不变，openapi 不改。
  **#334 lifecycle L3-2 邻居表按域取**（修 R111-2，#328 同族套件 oracle；根因真机更正：`show ip neighbors`
  无参＝v4+v6 全表混排、非「只给表 0」，卡点在邻居行锚 bvi0 而非 vhost + 起机期条目瞬时消失）——
  行匹配对端口==vhost 或对端口 table-id==域表、table-id 全动态推导，fixture 现场 L3-2 转可判定（5/5 正控），
  全套件 20/0/3。两决策均契约先行（附录 A）、`AGENTS.md` 决策条数
  234→236、`docs/v2待做.md` 二.21/二.22 标收口。**真机验证已完成（PR #259，2.0.0~dev22）**：#333 两方向
  都在一个 15s 巡检周期内消解（删声明/口进 VPP，均不重启），且事实未变时告警保持不误清；告警现场的
  正确造法是「fixture 在场时声明→fixture 落口→重启触发」（口不在 VPP 时声明会被正确拒绝、candidate 不收）。
  #334 已在 dev21 验证（见上）。证据 `docs/evidence/v2-round112-d333-d334-r111-closeout.txt`。
- **round113 第 1 批余项开工（决策 #335，2026-10-02，PR #260 已合并）**：**DHCP relay**——`set virtual-switches <vs>
  dhcp-relay server <ip>`（VPP dhcp proxy 按 rx-VRF 中继，src 自动取 BVI v4 网关地址；适用形态=「L2 交换机 + BVI 网关域」——实现期更正：type=l3 无 BVI/网关，语义不适用）。
  范围：只做 relay（server 延后）、server 须域内可达（跨 VRF 不在 v1）；恢复重放必须含 relay。
  **真机验证（dev23）**：proxy 下发正确（`show dhcp proxy` 与读视图三方一致）；**pcap 转发签名实证**——
  relay ON 时客户端 DISCOVER 被以 `192.168.99.1:68 → 192.168.99.10:67` 单播重发、OFF 时仅直连广播
  （两态相减=中继净效果；注意 VPP relay 以**源地址重写**转发、giaddr=0）；撤销路径幂等；恢复重放
  存活（VM/nfvis/libvirtd 多次重启）。套件：fulltest **212/0/13**（+2）、语义 **25/0/1**（S11 无 relay
  现场如实跳过）。**端到端租约未取得**（in-guest DHCP server 观测受限：guest 串口静默/无 exec/1G 大页
  仅 2 页），遗留下轮。**新登记** R113-1/R113-2（见 `docs/v2待做.md` 二.23/二.24）。
  证据 `docs/evidence/v2-round113-d335-dhcp-relay.txt`。
- **round114（R113-1 撤回 + 决策 #336 收口 R113-2，2026-10-02，PR #261）**：round113 登记的
  R113-1「停机 VM 改配置后 start 不重定义 domain」经代码复核**撤回**——`model.AllocationFor` 的
  `HugepageSize` 未设时**缺省回落资源池主池（1G）**、`domain_xml.useHugepages` 除 `backing normal`
  外一律大页后备，故「删 hugepage-size」的语义是回落缺省、域仍带 1G 是文档化行为（apply 计划对变更
  VM 确有 `DefineVM` 重定义）；**产品零改动**，教益=判定前先确认字段语义。R113-2 按决策 #336 收口：
  提交校验失败提示补足「仅本会话内保留（会话/进程结束即释放）」——保留行为不动（#317/#318 的会话级
  生命周期是刻意设计），两处同源文案 + 测试夹具更新，make check RC=0，决策条数 237→238。证据
  `docs/evidence/v2-round114-r113-1-retracted-d336.txt`。真机仍 2.0.0~dev23。
- **round115（决策 #337 L2 环路防护 v1 真机收口 + 判据补强，2026-10-02，PR #262+#263）**：范围=**不做
  STP/RSTP**（VPP 无实现，留 v3），v1=缓解（`set virtual-switches <vs> learn-limit <n>` → VPP
  `l2.BridgeDomainSetLearnLimit`，删配置回默认 16777216）+ 检测告警 `LOOP_SUSPECTED`（独立 scope，
  只告警不阻断）。**真机造环（双 vNIC VNF + guest 内 bridge）实测暴露 v1 判据结构性漏报**：~137k pps
  风暴下 MAC 稳定落单口（判据①不触发）、学习数仅 3（判据②远离上限）——**契约先行补入判据③**（每成员口
  rx 同时 ≥50000 pps 连续 2 轮）后复验命中（消息双证据、138.8k pps 与 vppctl 独立吻合）；误报基线 0、
  拆环两轮消解、删 learn-limit 回默认。套件：fulltest **214/0/13**（+2）、语义 **24/0/3**（S11/S12
  无配置按设计跳过；S8 窗口敏感系重复性弱点 → 登记 **R115-1**，**已由决策 #343 收口**：S8 正控改为
  窗口内增长周期计数 ⇒ 语义 **25/0/2**）。复现教益：停/起 VM 不重建 guest 内
  的桥——造环复现须按红线 12 改 user-data 再 restart。证据 `docs/evidence/v2-round115-d337-loop-protection.txt`。
- **round116（决策 #338 数据面 DNS 代理：真机实证「未达」→ 整体撤回，2026-10-02）**：用 VPP 内置 dns 插件
  做域内 DNS 代理（PR #264 已合入、dev26 真机验证）——**客户端面向成立**（查询到达 VPP：`dns4-request`
  计数、缓存 pending `probe.test`、pcap `.10→.1:53`），**上游面向不成立**：VPP 26.06 的 dns 插件**只在
  默认表（table 0）解析上游**（binapi `DNSNameServerAddDel` 无 VRF/fib 字段、CLI 无表参数），而产品域都是
  VRF 且**无法在默认表配地址**（`interfaces <if> address` 不存在）——pcap 实证 VPP **从未向上游发包**、
  缓存永久 pending，即「配了就是启用、但永远解析不了」。按「**不留假功能**」纪律**整体 revert PR #264**
  （CLI/REST/Web/套件/手册/实现；撤销路径已先行正向验证），附录 A #338 改写为撤回记录（决策号保留，
  计数仍 240），`docs/v2待做.md` 三.3 复原为未收口并登记三条后续路线（默认表寻址 / VPP 按表 / 自研转发器）。
  **教益**：契约先行写下的能力前提（「上游须在 VPP FIB 内可达」）必须在实现前就按**产品的网络模型**
  落到具体表/VRF 上核实——「FIB 内可达」在本产品=「默认表可达」，而默认表恰恰是产品配不了的那张。
  证据 `docs/evidence/v2-round116-d338-dns-proxy-not-viable.txt`。
- **round117（决策 #339 ACL 逐规则命中计数：机制成立、现场受阻；网关 ACL 区域挖出三个真缺陷，2026-10-02，
  PR #265 + dev28）**：#339 的**机制与接线成立**——ACL 逐规则计数确在 stats segment（组合计数
  `3:<规则>:<线程>:<packets>:<bytes>:/acl/<idx>/matches`，口径以 VPP 源码定死，更正初读的 hi:lo）、
  产品的解析/映射/读视图三面齐全（`show acls <n> detail` 能显示 hits，REST 同源），语义加 S13。但
  **现场非零命中未取得**，原因是一个**安全相关的真缺陷**：**R117-1（高）网关（BVI）绑定的 ACL 对域内
  流量零生效**——deny 规则在场时转发流量照走（pcap）、原始计数恒 0；机制=`show interface features bvi0`
  显示 ACL 入向特性只在 `ip4-unicast` 弧，而 BD→BVI 的 IPv4 走 `l2-input-ip4` 弧（未启用）；命令全表该行
  ✅ 已更正为 ⚠️。另两条（均由本轮现场暴露）：**R117-2（高）**同提交删「网关 ACL+交换机/ACL」违反倒序
  （先删 BVI 再解绑 → `Invalid sw_if_index (-2)` + 补偿失败、整体回滚；ACL 删除未落数据面由
  **ACL_LEFTOVER** 如期指出、重启数据面后清）；**R117-3（中）**网关绑 L3 ACL 后域内 **ARP 被 ACL 插件的
  非 IP 白名单丢弃**（`bvi0 rx=drops`）→ 域内主机静默无法解析网关（产品无 macip 配置与提示）。
  **#339 的非零命中验证待重做**（改用「vNIC 作 L3 接口」绑定形态，或先修 R117-1）；网关 ACL 三缺陷
  优先级高于 #339 收尾。证据 `docs/evidence/v2-round117-d339-acl-hits-and-gateway-acl-defects.txt`。
- **round118（#339 整体撤回 + #68 更正；ACL 能力边界实证，2026-10-02）**：接 round117 的线——
  ① **「vNIC 作 L3 接口」形态下 ACL 确实会拦**（正确的 L3 入向、特性在、绑定可见；`/err/acl-plugin-in-ip4-fa/
  ACL deny packets` 递增 8→12），即**产品的 ACL 能力在 L3 接口形态下是可用的**（R117-1 只关乎网关/BVI 形态）；
  ② 但 **`/acl/<idx>/matches`（逐规则组合计数）恒 0**——启用全局 `acl_stats_intf_counters_enable`
  （VPP 侧 CRC `0xb3e225d2` 与 govpp 一致）并重绑定后**依旧不递增** ⇒ **VPP 26.06 不写该计数**（注册在、
  不更新），「逐规则命中」在本底座取不到真值、读视图会恒 0 误导 → **按「不留假功能」纪律整体 revert
  PR #265**（决策 #339 改写为撤回记录，计数仍 241；`docs/v2待做.md` 一.6 复原为未收口并记新原因）。
  ③ **对 #68 的更正**：其「govpp 调该 API 会被误执行为 `acl_del`（危险）」**不成立**——实测请求 CRC 两面
  一致、VPP 按 `counters_enable` 正常执行；报错实为 **govpp 对空应答的 CRC 碰撞误报**（VPP 所有空应答共用
  `0xe8d4e804`，govpp 把它解成了 `acl_del_reply` 并丢弃）——是应答识别噪声、非请求误执行；**结论（能力不可用）
  仍成立，原因更正**（不是危险，而是 26.06 不递增）。④ 清 L3 现场时补记 **删除倒序链**（ACL 被 l3-interface
  引用、l3-interface 引用 vNIC、VM 引用交换机——须逐级解绑；产品校验会明确拒绝，属 R117-2 同族）。
  证据 `docs/evidence/v2-round118-acl-l3-form-and-counters-never-increment.txt`。
- **round119（决策 #340：网关 ACL 绑定提交期硬拒绝 + 指引 L3 形态，2026-10-02，PR #266 + dev29）**：真机实证
  的处置落地——`set virtual-switches <n> gateway acl-in|acl-out` 在**提交期直接拒绝**（同管理口守卫先例：
  静默失效的保护手段一律硬拒、不给 force 出口），文案写明「VPP 26.06 不评估 BVI 域内流量」与替代
  （`l3-interface <if> acl-in <acl>`，已实证生效）；`L3Interface.AclIn`/`VSwitchPort` 行为不动；契约/全表/手册
  如实化（含**绑 ACL 的接口会丢弃非 IP 含 ARP、对端需预置静态邻居**这条代价，与「端口级绑定待验」的标注）。
  **真机复验（dev29）**：网关绑定被拒且文案含替代指引 ✓；L3 形态提交成功且 VPP 侧 `input acl(s)` 绑上 ✓。
  测试：`TestValidateGatewayACLRejected` 新增、`TestAclEndpoint` 改用 L3 形态；套件豁免改「设计拒绝」；
  make check RC=0。**端口级已补验并扩展拒绝（round119，PR #267 + dev30）**：vppctl 直绑成员 vhost 口的
  deny ACL 同样不拦 ⇒ 同机制；校验对 `VSwitchPort.AclIn/AclOut` 一并提交期拒绝（真机经 REST 写
  `ports[].acl_in` 后提交报 `ports[1].acl_in` 拒绝文案 ✓）；命令树本无该语句（字段仅 REST 可达）→
  登记 **R119-1**（契约一致性：将来底座支持须先补树叶子）。顺带印证 #317/#318 会话锁语义
  （REST 脏候选持锁时 CLI 新会话 commit 被正确拒绝）。
- **round120（决策 #341 根治 R117-3：绑 L3 ACL 自动放行非 IP/ARP，2026-10-02，PR #268 + dev31）**：产品在绑定
  `l3-interface <if> acl-in <acl>` 时**自动创建并绑定伴随 macip 白名单**（tag `nfvis-nonip-permit`，permit 任意
  ip/mac、mask 0），修复「唯一可用的 ACL 形态一绑上就丢域内 ARP」。真机验证（dev31，guest **不配静态邻居**）：
  ① 产品读视图 `acl-in-note 含非 IP/ARP 自动放行`、VPP 侧 macip 建/绑（`macip interface = sw_if_index 4: 0`）；
  ② **邻居表出现对端**（ARP 通、无需静态邻居）；③ 翻 deny 后 IP 侧计数增长（真在拦）而邻居表不受影响；
  ④ `request vpp restart` 后 macip 随重放回来、ARP 重新通。**R117-2 扩为四次复现/三种形态**（含「接口先删、
  ACL 绑定登记陈旧 → 删 ACL 撞 -2、须 restart nfvisd」的新变体）；修法方向已写进待做（接口消失时同步清
  ACL 绑定登记 / 对「接口不存在」容错解绑）。证据 `docs/evidence/v2-round120-d341-macip-arp-verified.txt`。
- **round121（决策 #342 收口 R117-2：ACL 解绑容错 + 删除计划先解引用，2026-10-02，PR #269 + dev32）**：真机四次
  复现的删除倒序家族定根同修——`acl.go` 解绑遇 VPP `Invalid sw_if_index (-2)` 按**已达成**处理（复用既有
  `isMissingIfaceErr`，清 `bound/macipBound` 登记、不打断删除；**真实触发点是 `DeleteACL` 的重绑循环**，
  `BindIndex` 空绑定分支实际不可达——子智能体核实并如实报告）+ `apply.go` 删除计划把 **ACL 提到最前**
  （先解引用、后删被引用，与 #196 口径一致）。**真机复验（dev32）**：同提交删「L3 交换机 + 其 ACL」、
  先删 `l3-interface` 再删 ACL——**均一次提交成功、无需重启 nfvisd**（修复前必撞 -2 + 回滚/须 restart）。
  结束后出现的 `COMMIT_VRF_DELETE_DEFERRED`/`VRF_TABLE_LEFTOVER` 是**既有删表延后行为**（#192，按文案
  `request vpp restart` 后自动消解，已与本次修复区分）。测试：4 例容错 + 顺序断言反转；make check RC=0。
  证据 `docs/evidence/v2-round121-d342-acl-delete-order-verified.txt`。
- **round122（决策 #343 收口 R115-1：语义 S8 正向控制改窗口周期计数，2026-10-02，PR #270）**：S8 原判据是
  **单次 3s 窗口**比对成员口 rx，而 fixture guest beat 在 ping 超时后降为 ~3s/发，**窗口会整窗跨零** →
  有流量却判 noflow（round115 三次复跑均如此）。改为：新增纯函数 `s8_rx_cycles_of` + `s8_rx_window`
  （各成员口**共用 20s 窗口**、每 1s 采一次、`S8_WINDOW_SECS` 可覆盖），**任一成员口周期数 ≥2** 即 learned；
  无增长仍如实 UNKNOWN（不把不可判定改成通过）；`s8_verdict` 语义不动；自校准加 6 条红-绿用例并验证判别力。
  **真机复跑：语义 25/0/2**（S8 转可判定并通过；余 2 项 S11/S12 按设计跳过）——套件基线变化已按纪律登记。
  顺带实测：fixture down 后 `VNF_PORT_DOWN` 在对象消失的下一个巡检周期**自动消解**（#188 对账口径成立）。
  证据 `docs/evidence/v2-round122-d343-s8-window-verified.txt`。
- **round123（决策 #344 ACL 绑定字段契约如实化，收口 R119-1，2026-10-02，PR #271）**：openapi 里三处
  `acl_in/acl_out` 原是无说明的 `{type:string}`（网关/端口/端口读视图），而网关与端口两种绑定已被 #340
  提交期拒绝、L3 接口才是唯一可用形态（#341 自动放行非 IP/ARP）——契约不说明会让人以为三种都能用。
  四处字段加如实 description（不支持者 + 替代指引；支持者 + 自动放行说明），命令树设计补「端口级无语句是
  **有意**」，`openapi.json` 以 `--write` 同步；**只改契约文档、不改代码/命令/行为**（模型字段保留解析兼容、
  编排路径保留防御性）。make check 全绿（含 openapi 同步/可解析/无重复键守护）。**ACL 线至此全部收口**
  （#339 撤回、#340 网关/端口拒绝、#341 自动放行 ARP、#342 删除倒序容错、#344 契约如实）。
  证据 `docs/evidence/v2-round123-d344-acl-contract-truthful.txt`。
- **round129（启动超时根因定位 + R129-1/#349 收口 + #348 重启测试达成 + 环境恢复，2026-10-03）**：接手 round124~128 交接的
  「真机环境未恢复」（nfvis 卡 activating → systemd 90s 超时 → restart 循环）。**只读诊断三层证据定根**
  （journal 90s 零输出 / SIGQUIT 全栈 / virsh 同挂）：① **环境级**——libvirtd 开机自启 sem-vm
  （产品 `"autostart":true` ⇒ 产品建 libvirt autostart 软链）时**假死**（socket 可连、对一切客户端零响应，
  qemu 以 `-S` 暂停态启动后不再 resume）；② **产品缺陷 R129-1（高）**——`compute.Connect` 只在入口查一次
  `ctx.Err()`，随后调 go-libvirt **包级 `ConnectToURI`（不收 context）**，认证握手无限阻塞 ⇒ main.go 装配处的
  10s `libvirtCtx` 形同虚设 ⇒ nfvisd 永远到不了 READY（Type=notify）⇒ systemd 启动超时循环、管理面整体不可用。
  **round128 的两个假设被更正**：「启动含恢复收敛、现场重所以超 90s」不成立（恢复收敛未开始）；「#348 四次
  真机失败」全是本环境故障所致、与其代码无关（四次均卡在 main.go:255，未走到 #348 装配点）——**环境已恢复**（杀假死 qemu → 摘 autostart 软链 → restart libvirtd → virsh 秒回 → nfvisd 启动 20s 走完 →
  服务 active；软链随后被产品自动重建）。**两项决策当日收口（PR + 真机证据齐全）**：**#349**（PR #279，底座
  无响应时启动有界降级，收口 R129-1）——libvirt 连接**自持 conn 有界化**（中断 = 超时关 conn；go-libvirt 的
  `Disconnect()` 内部先发 RPC、对假死进程同样挂死，不可用作中断手段——源码核实）+ Docker 探测/AppArmor
  Ensure 有界 ctx + 降级告警 `COMPUTE_UNAVAILABLE`/`CONTAINER_UNAVAILABLE`；四步真机验证全过（假死现场
  11s active、告警在场、底座恢复后告警保持且 VM 动作如实拒绝、健康回归无告警、Docker 同款对照）。
  **#348**（PR #278，重基于 #349）——dev44 重启测试**达成**：`reboot` 后单次启动即 active（无 90s 循环）、
  VPP 自动拉起（journal「检测到 VPP 未运行，发起拉起（不等待）」→「已连接 VPP」→「恢复收敛完成」）、
  幂等路径「VPP 已在运行，无需拉起」同场验证；开机 libvirtd 假死未复现（偶发非必现）。**新登记 R129-2（中）**：
  开机竞态——libvirt 自启 1G 大页 VNF 期间不服务客户端握手 >10s ⇒ nfvisd 按 #349 有界降级且不自动重连 ⇒
  **每次开机后 VM 编排需人工 `systemctl restart nfvis`**（VNF/数据面本身不受影响；候选修法登记在
  `docs/v2待做.md`）。证据 `docs/evidence/v2-round129-startup-hang-rootcause.txt`、
  `v2-round129-d349-bounded-startup.txt`、`v2-round129-d348-reboot-verified.txt`。
- **round130（#350 撤回 + #351 交付收口 R129-2，2026-10-03）**：① R129-2 第一案（#350 启动期 libvirt 重试
  3×10s）实现与降级路径回归都成立，但**重启测试未达**——三次开机实测 libvirt 可服务窗口 ≈ **65s / >32s / ≈90s**
  （libvirtd「Started」后要先完成 autostart 等驱动初始化才服务握手），固定启动期重试被 TimeoutStartSec（90s）
  顶死 ⇒ 按「不留假功能」撤回（PR #280 不合并，决策号保留）。② **#351（PR #281）落地**：启动序列不再同步等
  libvirt 到降级为止——快路径失败后由**后台接入循环**（30s 节奏 × 10s 有界、重试静默）兜底，接入成功即
  **原子换装**（动态持有层转发 ComputeProvider 9 法 + StartVMChecked/Console/快照 4 法、ContainerProvider 9 法；
  未接入语义表逐法对齐今天 nil/Noop 行为）+ INFO 日志 + 消解告警 + 补跑 EnsureConsistent；**重启测试零人工
  干预闭环**（boot 后 ~2min 内「计算编排已接入（后台）」、告警自动消解、sem-vm running 可见、VM 动作可用），
  快路径逐字同序、降级文案逐字一致。**如实边界**：REST 降级态生命周期错误状态码 503→5xx（文本对齐）、
  REST PUT VM 降级态由「跳过运行态检查」变 503（更安全）、console 失败时点后移；接入成功后连接中断的
  自动重连不在范围（残余边界如实登记）。证据 `v2-round130-r350-retracted-window-measured.txt`、
  `v2-round130-d351-async-connect.txt`。③ **#352（PR #282）交付收口三.4 v6 专项核实**：核实实证
  ACL 校验/管道/插件全双栈、唯一缺口是 `prefixOrAny` 的 any 硬编码 v4（含 any 的 v6 规则被 VPP -57
  打回）、NAT44 校验层放行 v6 而 apply 层报 invalid IP4——修法为 any 家族跟随 + 显式混族校验期拒绝 +
  icmp 家族映射（v6=58）+ NAT44 三处校验期拒绝（文案注明仅支持 IPv4）；真机三探针全过（v6+any 下发
  成功且 VPP 显示 `ipv6 … dst ::/0`、混族与 NAT v6 均校验期拒绝）。**登记未竟**：v6 ACL enforcement
  （真拦 v6 流量）待 vNIC-L3 场景验证；NAT66/NAT64 插件在构建里、未接。**教训**：`-c` 脚本模式每次
  运行是独立会话，set 与 commit 必须同会话（#336 会话级候选）。证据 `v2-round130-v6-coverage.txt`。
- **round131（#335 relay 端到端租约补验达成，2026-10-03）**：round113 遗留（观测通道受限）的 relay
  端到端租约**达成**——观测通道换法：server 移到 Windows 宿主（VMnet8 次地址 99.10 + 测试专用最小
  DHCP server，只应答中继单播、广播按策略忽略留痕）、客户端/回程观测全在网络侧（server 事务日志 +
  `vppctl pcap trace intfc any` 三跳 pcap + **客户端以租约地址为源的出包痕迹**（IGMP/LLMNR，无需进
  guest）+ VPP 错误计数）；对照（relay OFF：8 条广播到达但被忽略、无租约）与实验（relay ON：完整
  Discover→Offer→**Request→ACK**，Your-IP 99.50，客户端实际使用）两态相减。**关键发现**：VPP dhcp
  proxy 回程要求 server **回显 option 82**（不回显则应答被静默丢弃，唯一线索是
  `show errors` 的 `dhcp-proxy-to-client DHCP option 82 missing`）——已补进用户手册 §8.3（排障要点）。
  边界如实：本机拓扑 client/server 同 L2，**续租单播直发 server 不经代理**（跨 L3 续租路径另行评估）；
  现场双向还原（配置 rev 1113 回基线 + 宿主服务/地址/防火墙全还原）。证据
  `docs/evidence/v2-round131-d335-relay-lease-verified.txt`。
- **round132（四套件复跑，2026-10-03）**：#348/#349/#351/#352 落地后的完整回归——**四套全绿零失败**：
  `cli-fulltest` **231/0/13**（214→231：各轮新契约命令入套件累积；自清场正常）、`cli-semantic-check`
  **26/0/3**（S13 DNS punt 实链通过；不可判定 = S11 无 relay 现场 + S12 按设计 + 1）、
  `cli-lifecycle-check` **22/0/3**（历史最好档）、`cli-pty-smoke` **10/10**。环境清场（自由处置留痕）：
  删历史遗留 **bond0**（占 ens192，round87 时代套件残留）、启用 ens224、跑 fulltest 前停 sem-vm 让
  1G 页（cli-vm 用 1G）；收尾 `restart vpp+nfvis` 收敛回声明态、套件残留 grep=0。证据
  `docs/evidence/v2-round132-four-suites-baseline.txt`；基线沿革已同步 `v2已做.md` §五。
- **round133（#352 登记项补验：v6 ACL 数据面真拦，2026-10-03）**：形态改为**物理口 ens192 作 L3 接口**
  （免 VM/免 1G 大页；同一绑定机制），宿主（VMnet8 加 2001:db8::2 + 静态邻居）发 UDP6，VPP 错误计数器
  两相判定：permit 相位 `acl-plugin-in-ip6-fa checked 20 / permit 20`（v6 路径过 ACL 弧）→ 翻 deny 后
  `checked 40 / deny 20`（**20 个 v6 包全被拒**）。至此 #352 四种真机验证（any 家族/混族拒绝/NAT v6 拒绝/
  enforcement）全达成，三.4 该项登记撤销。顺带如实记录：绑「只含 v6 规则」的 ACL 后接口上环境 v4 流量按
  缺省语义被拒（ACL 绑定对两族都生效）。证据 `docs/evidence/v2-round133-d352-acl6-enforcement.txt`。
- **round134（#353 交付：1G 池「数据面固定占用」进用户可见口径，2026-10-03）**：读视图（CLI/REST/Web 三面
  同源）增「数据面占用」**实测**列（`held_by_dataplane`，**独占归属**）+ 1G 说明「VNF 可起页数 = 空闲页数」
  + 向导（1G 问句提示 N−1；0<N<2 出「无法再起 VNF」告警）。**Browser Use 复核抓到并修掉一个真缺陷
  （round134b）**：首版「首见归属」按 /proc 扫描顺序归属——**vhost-user 会把 VNF 的 guest RAM 大页映射进
  VPP**，sem-vm 停/起换 pid 后归属在 1↔2 间非确定翻转（Web 显 2、CLI 显 1、REST 显 2，三源互相矛盾）；
  改**独占归属**（仅 vpp 映射且无其它进程共享才计入；comm 读不到按非数据面计）后稳定 dp=1，含共享页
  排除回归测试。**教益**：归属判据用全量映射者集合、不用扫描顺序；跨事实源对照能暴露非确定读数。
  验证：sysfs/smaps/CLI 三方 + REST + 停/起动态 + wizard 预演 + **dev51（合并后构建）Browser 复核 + 截图**
  （`docs/images/round134-web-pools-dataplane-column.png`）；证据 `v2-round134-d353-hugepage-visibility-verified.txt`。
- **round135（#354 交付：接入成功后连接中断的自动重连，2026-10-03）**：libvirt 侧驱动改**常驻状态机**
  （未接入 30s 节奏接入同 #351；已接入**每 15s 探活**、连续 2 次失败判中断 ⇒ 运行期告警（同码
  COMPUTE_UNAVAILABLE）+ 有界复连（10s×3）⇒ 成功换装/消警/EnsureConsistent，失败退回 30s 节奏**永不放弃**
  ——彻底反转 #349 的「不自动重连」边界）；Docker 侧**不改**（无会话态，契约明文）。真机三验：① 运行中
  restart libvirtd ⇒ **20s 内**中断判定→复连 INFO→消警→读数恢复；② 长窗口 stop ⇒ 告警保持（运行期文案
  含探活/复连口径与手查路径）→ start 后 06:45:47「已恢复」+ 消警 + sem-vm running；③ docker restart
  对照 ⇒ libvirt 侧零动作零告警、容器侧自然恢复（start sem-ct 一次成功）。**教益**：采证窗口要窄/带时间戳
  （首采被上一轮旧日志撞成假阳性，已更正）。证据 `v2-round135-d354-runtime-reconnect-verified.txt`。
- **round136（三.7 核实 + #355 交付：/metrics 可用性如实与逐对象，2026-10-03）**：真机全量盘点证明
  覆盖面**远广于遗留条目记录**（主机 + VPP 线程/内存/buffer/逐接口计数 + 配置计数 + 告警 + VNF/容器聚合，
  46 行）；真缺口二：编排不可用时聚合 running **静默计 0**（看板误读「全停」）、无逐对象序列。**#355**
  （PR #285）补 `nfvis_vnf_runtime_available{kind}`（1/0 始终发出，同 #68 口径）与
  `nfvis_vnf_up{kind,name}`（0/1；不可查询时**不发**该对象——宁缺不谎报）＋手册读法；聚合指标名/标签/取值
  逐字不变。真机三验：基线 up=1；容器起/停的 up 与计数双向同步；**停 libvirtd ⇒ available{vm}=0 且 vm
  逐对象消失**（对照修复前静默 0）、起回恢复（与 #354 复连联动）。证据 `v2-round136-d355-metrics-verified.txt`。
- **round137（三.6 交付：#356 历史时序存储 v1，2026-10-03）**：清 `docs/v2待做.md` 三.6——
  `/metrics` 是即时快照、Web 无趋势底座。**先评规模**（本机 ≈80 序列/次、60s 采样 ⇒ 4.6 MB/天、7 天 ≈32 MB，
  数据分区 79 GB 可用）⇒ **SQLite 足够、不引外部 TSDB**。**#356**（PR #286）：采集侧把 `handleMetrics` 取数体抽成
  `Server.GatherSamples`，`/metrics` 与后台采样器**共用同一路径**（不做第二套清单）；存储＝**独立库**
  `/var/lib/nfvis/metrics.db`（不与配置库的事务/候选锁竞争、不进配置备份恢复语义）；保留＝时间窗（默认 7 天）
  + **硬行顶 2,000,000** 双界；配置 `set/delete system metrics history interval|retention-days`（改配置**免重启**即生效）；
  读视图**三面同源**（CLI `show system metrics history [name <metric> [last/step]]`、REST `GET /metrics/history`
  **需 Bearer**（与无鉴权的 `/metrics` 有意不对称）、Web「系统 · 历史趋势」页内联 SVG）。**真机四维全过**：
  ① CLI 概览与**直读 SQLite** 逐值吻合；② 重启 nfvis/重装 deb 后历史延续（min 不变）、间隔 60→10 免重启生效、
  存储启用与「就绪」日志相隔 21ms（不阻塞启动）；③ 容器起/停 ⇒ `nfvis_vnf_up` 历史 0→1→0；④ `/metrics` 与历史
  最新点反推开机时刻**差 0.4s**（同一内核 uptime）、库打不开时读视图如实报不可用+原因、无 token 401 / `/metrics` 仍 200。
  **Browser Use 复核**（dev56）通过，截图 `docs/images/round137-web-metrics-history.png`。
  **四套件**：fulltest **240/0/13**（231→240：新命令入套件 +9）、语义 **26/0/3**、lifecycle **22/0/3**、pty **10/10**。
  **本轮实测修掉 7 处**（详见证据 §7）：概览「（默认）」注记恒真、`truncated` 用点数猜、查询失败被答成「无数据」、
  删空留 `system.metrics` 空壳致 display set 回放报内部错误（`pruneEmptySingleton` 补一层）、Web 坐标轴浮点原始串、
  `limit` 无上界、schema 节点描述里写了决策号（archtest 拦下）。证据 `docs/evidence/v2-round137-d356-metrics-history.txt`。
- **round138（一.9 交付：#357 容器 exec v1（非交互），2026-10-03）**：差别分析 §1-17「容器只有 log、
  没有 exec」的 v1。**能力前提先核实**（#338 的教训）：真机直接打 Docker Engine API spike——非 TTY exec 的
  `start` 回**多路复用流**（8 字节头 `01`=stdout / `02`=stderr + 长度，xxd 实证）、`GET /exec/{id}/json` 回
  `ExitCode`、TTY 形态回 raw-stream；三者成立才动手。**#357**（PR #287）：`request container-functions
  <name> exec <command> [timeout <seconds>]` + REST `POST /container-functions/{name}:exec` + Web 容器详情页
  「执行命令」分栏（`data-write` 门禁）。**权限 S**（与 VM 串口 console 的关键差别：console 进 guest 仍需
  guest 凭据，exec 是**免凭据的容器内命令执行**，operator 本不能创建容器故不能经此绕过）。**有界且如实**：
  超时 30s（1..300）、stdout/stderr 各 256 KiB（超限置 truncated 并继续读掉）；⚠️ **超时只中止客户端等待**
  （Docker 无 exec 中止接口，容器内进程可能仍在跑——真机 `docker exec ps` 实证 `sleep 60` 仍在）；
  退出码未知时不报（不谎报 0）；**退出码是结果不是失败**（跑完即算本操作成功）。**真机四维全过**：
  ① 与 `docker exec` 直跑**逐字一致**、退出码一致；② restart 后可用、停机如实拒绝并指向 start（CLI/REST 同款文案）；
  ③ exec 内 `ps` 看到的是容器自己的进程视图；④ 超时/大输出/命令不存在(127)/忘加引号/超时越界各如实处理，
  **operator 双面拒绕**（CLI `%% 无权限`、REST 403；对照读日志 200）。**Browser Use 复核**（dev60）通过，
  截图 `docs/images/round138-web-container-exec.png`。**本轮实测修掉 3 处**（CLI 非运行态漏指向 start；
  **Web「执行命令」分栏点了没反应——`CT_TABS` 常量漏加 `'exec'`，静态守护全看不见、Browser Use 真机点一遍才抓到**；
  新增分栏一致性守护 `TestDetailTabsConsistentBetweenHTMLAndJS` 并自校准）。**环境事故（入册）**：换身份验证
  忘了给 `-u`，失败登录计到 admin 头上触发**15 分钟锁号**（#76 策略按设计生效）——别拿真账号试口令。
  证据 `docs/evidence/v2-round138-d357-container-exec.txt`。
- **v2 清单分册（2026-10-02 整理）**：**已完成**（决策 #300~#344、已收口的缺陷与特性）见 `docs/v2已做.md`；
  **未做**见 `docs/v2待做.md`（**只列未做**，保留原编号便于交叉引用；原「二·29 条登记缺陷」已全部收口，
  索引在 `v2已做.md` §二）。立项前先看 `v2待做.md`、查「这条是不是已经做过」看 `v2已做.md`。
- 已定决策 260 项见规格书附录 A（main/1.x 线 #1~#201；本仓库当前在 **v2/2.x 开发线**，决策自 **#300** 起、
  #202~#299 为 main 预留号段，双线发版约定见决策 #300，v2 线已有 #300~#358，其中 #350 撤回）——实现中遇到"该怎么做"的问题，先查附录 A，不要重新发明。
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
  （257 条命令 + 逐条真机实测状态）；真机手动脚本：`contrib/scripts/cli-fulltest.sh`（问「命令能不能用」）、
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
bash contrib/scripts/cli-fulltest.sh        # 「命令能不能用」：257 条契约命令，见 %/%% 即失败
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
`cli-fulltest` **210/0/13**（实测修正：2026-10-01 round101 真机为 210/0/13——设计里的 212 系逐阶段增量相加的算术偏差）、`cli-semantic-check` **25/0/2**（决策 #343 后：无新增/移除项，是既有 S8 由「不可判定」转「通过」——正控改为窗口内增长周期计数、慢周期下不再整窗跨零，收口 R115-1；另 2 项不可判定为 S11/S12 按设计无配置跳过）、`cli-lifecycle-check` **21/0/3**、`cli-pty-smoke` **10/10**；
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
