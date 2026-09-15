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
  M5-9 剩余占位（`show system hardware` 之外的 `monitor vnf`；`show system hardware` 已接）、
  以及 §10 吞吐/容量类基准（需流量发生器与规模压测）。详见 `docs/M5-11-端到端与基准报告.md` §4。
  **T0-5（快照磁盘内容级回滚）已于 V1 收尾第七轮完成**（决策 #75）。
- M4 验收现状（`docs/M4-验收记录.md`）：M4-1~M4-11 真机通过（`make integration` 全绿）；M4-12 CLI 侧命令真机冒烟通过
  （show/request/delete 交互确认/console ticket/审计/动态候选）。**已知环境限制**：SR-IOV 无 PF/VF 未真机验证；
  容器侧 memif 通流未验（离线无自带 memif 的容器镜像）。
- M3 验收现状（`docs/M3-人工演示记录.md`）：D1/D2/D3/D6/D8 真机通过；**D4 NAT 已在本轮 M5 补齐并真机端到端通过**
  （决策 #52 跨 VRF：inside=virtual-switch 的 VRF、outside=出接口所属 VRF，VPP 单实例仅一对）；**D5 SPAN 抓包已在 T0-7 实证通过**；
  D7 LLDP 仍环境受限（无对端），启用与命令均正常、M3 的 internal error 未复现。
- 验证环境 nfvis-vm 当前状态：**VPP 运行中**（main-core 4 / corelist-workers 5，ens192/ens224 绑 vfio-pci）、
  nfvisd 未运行、**无 domain/接口残留**、**1G 大页仅 1 页空闲**（测试 VM ≤1G、串行）、
  引导镜像 `alpine.qcow2` 已恢复；Docker 本地有 `alpine:3.20`。真机前 `pkill -x nfvisd`；
  集成测试 `make integration`（CI 不跑）。设计基线在 `docs/`，**不要凭记忆重设计**。
- 已定决策 80 项见规格书附录 A——实现中遇到"该怎么做"的问题，先查附录 A，不要重新发明。
- **V1 验收收口（2026-09-14）**：`docs/V1-验收检查表.md` 已把规格书 **111 条 FR** 逐条对照证据
  （通过 89 / 未验 7 / 降级 15 / 移 V2 2），降级理由与签字建议见其 §5/§6；
  待办与未完成项入口见 `docs/V1-收尾待办.md`。**新增登记的降级**：物理口 link 告警、告警转发 syslog、
  日志级别联动、`GET /api/v1/openapi.json` 与列表分页、FR-SEC 若干强制力项。
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
