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
- M4 验收现状（`docs/M4-验收记录.md`）：M4-1~M4-11 真机通过（`make integration` 全绿）；M4-12 CLI 侧命令真机冒烟通过
  （show/request/delete 交互确认/console ticket/审计/动态候选）。**已知环境限制**：SR-IOV 无 PF/VF 未真机验证；
  容器侧 memif 通流未验（离线无自带 memif 的容器镜像）。
- M3 验收现状（`docs/M3-人工演示记录.md`）：D1/D2/D3/D6/D8 真机通过；**D4 NAT 已在本轮 M5 补齐并真机端到端通过**
  （决策 #52 跨 VRF：inside=virtual-switch 的 VRF、outside=出接口所属 VRF，VPP 单实例仅一对）；**D5 SPAN 抓包已在 T0-7 实证通过**；
  D7 LLDP 仍环境受限（无对端），启用与命令均正常、M3 的 internal error 未复现。
- 验证环境 nfvis-vm 当前状态（**2026-09-19 两度被恢复为干净快照**；快照恢复会清掉全部现场，
  重建路径见 `docs/evidence/v1-closeout-round32-install-iso.txt` §4e——该轮的 ISO 交付已按决策 #111 废除，
  文档仅作历史记录）：
  系统 Ubuntu Server 26.04.1 + **USTC 源**（aliyun 实测几乎不可用，勿切回）；构建工具按需装齐
  （Go 1.26.0（apt）、make、sshpass 等；`dpkg -i` 装 VPP 用 `/root/vpp-v26.06-deb/` 的 9 个
  26.06-release deb——快照基线自带）。
  源码树 `/root/src`（git archive 同步，见待办 §3.3，无 .git → 构建**必须显式传 SOURCE_DATE_EPOCH**）。
  **round68 后现状**（2026-09-24 发布 v1.1.30，证据 `docs/evidence/v1-closeout-round68-release-1.1.30.txt`；
  三件套复跑仍以 `docs/evidence/v1-closeout-round36-three-suites.txt` 为准）：
  nfvis **1.1.30** active、管理口令 `WBF81vOA4M8GM28f@Aa1`（**随快照恢复而变**，取法见待办 §3.1）；
  VPP 26.06 运行、主堆用 2M 大页、ens192/ens224 交 DPDK；
  cmdline 含 hugepagesz=1G/2M + isolcpus=2-5 + intel_iommu=on；
  **vs-vnf 拓扑与 vnf-a/vnf-b 在跑、流量已复通**（BVI ping 双向 10/10、宿主经 DPDK 物理口 0.37~0.48ms）；
  镜像 `debian-12-generic-amd64.qcow2` + `alpine.qcow2`（阶段 3 前置）+ 容器镜像 `alpine:3.20`；
  配置库 rev 55 / audit 103（内容与复跑前基线逐字节一致）；**`nodejs` v22.22.1**（round37 装，
  仅用于 `node --check internal/api/ui/app.js` 校验 Web 前端语法——前端与 CI 都**不依赖** node）。
  ⚠️ 管理口令、`alpine.qcow2`、`docker alpine:3.20`、`rev/audit` 这四项**都会随快照恢复而变/丢失**，
  每次从快照重建后要重新核对并回写（round36 就撞上其中两条）。
  ⚠️ 集成测试环境（VPP 运行、镜像、1G 大页布局、ens192/ens224 交 VPP）随快照清掉——
  跑 `make integration` 前需先重建（布局与流程见待办 §3.3 / §0 第 1 条）。
  `ens160` 是管理口（vmxnet3、承载 SSH）——**永不拿管理路径做试验**的红线不变。
  设计基线在 `docs/`，**不要凭记忆重设计**。
- 已定决策 146 项见规格书附录 A——实现中遇到"该怎么做"的问题，先查附录 A，不要重新发明。
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
  v1.1.15 / v1.1.19 / v1.1.20 / v1.1.25 / v1.1.26 / v1.1.27 / v1.1.28 / v1.1.29 / v1.1.30**（见 GitHub Releases；**跳过 v1.1.6**——那次发布已撤回、其提交不在 `main`，
  以及 **1.1.11~1.1.14、1.1.16~1.1.18、1.1.21~1.1.24**——同一 merge 线上的内部验证构建、从未发布，
  故由 v1.1.10 跳到 v1.1.15、v1.1.15 跳到 v1.1.19、v1.1.20 跳到 v1.1.25；v1.1.26 紧接 v1.1.25、v1.1.27 紧接 v1.1.26、v1.1.28 紧接 v1.1.27、v1.1.29 紧接 v1.1.28、v1.1.30 紧接 v1.1.29，无跳号）。
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
