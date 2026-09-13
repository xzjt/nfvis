# NFViS 开发约定（对本工作区内的所有开发会话生效）

任何在本仓库内工作的 AI 助手 / 开发会话，开始工作前先读本文件与 README.md。

## 项目状态与基线

- 当前阶段：**M1/M2/M3/M4 已完成并合并**（M4 = M4-P0 + M4-1~M4-12，14 个 PR #41~#54 已全部合入 main）。
  交付与真机证据见 `docs/M4-验收记录.md`、`docs/M4-11-集成测试验收记录.md`，进度/环境/技术坑见
  `docs/M4-进度交接.md`。**下一步 M5**：
  `/events`(SSE)、`/metrics`、`/vpp/capture`、备份/恢复、tech-support、core-dumps、hardware、tls、
  health thresholds、software/reboot/shutdown/zeroize/ntp、deb 打包、e2e（任务清单 `docs/M5-任务清单.md`）。
- M4 验收现状（`docs/M4-验收记录.md`）：M4-1~M4-11 真机通过（`make integration` 全绿）；M4-12 CLI 侧命令真机冒烟通过
  （show/request/delete 交互确认/console ticket/审计/动态候选）。**已知环境限制**：SR-IOV 无 PF/VF 未真机验证；
  容器侧 memif 通流未验（离线无自带 memif 的容器镜像）；M4-12 真机验证 `request images delete` 时移除了共享引导镜像
  `/var/lib/nfvis/images/alpine.qcow2`，致 `make integration` 中 3 个用例转 SKIP——恢复命令见
  `docs/M4-验收记录.md` M4-12 节「未验证项」第 3 条。
- M3 验收现状（`docs/M3-人工演示记录.md`）：D1/D2/D3/D6/D8 真机通过；**D4 NAT 端到端生效待定**——出接口语义已定为必填（决策 #38），但 inside/outside 跨 VRF 的 NAT 拓扑语义需在 M4 网络增强前决策；D5 SPAN 抓包、D7 LLDP 因环境受限未验（tap 插件未启用；vmxnet3 下 VPP LLDP 报 internal error 且无对端）。
- 验证环境 nfvis-vm 当前状态：**VPP 运行中**（main-core 4 / corelist-workers 5，ens192/ens224 绑 vfio-pci）、
  nfvisd 未运行、**无 domain/接口残留**、**1G 大页仅 1 页空闲**（测试 VM ≤1G、串行）、
  **引导镜像 `alpine.qcow2` 缺失待恢复**（见上）；Docker 本地有 `alpine:3.20`。真机前 `pkill -x nfvisd`；
  集成测试 `make integration`（CI 不跑）。设计基线在 `docs/`，**不要凭记忆重设计**。
- 已定决策 51 项见规格书附录 A——实现中遇到"该怎么做"的问题，先查附录 A，不要重新发明。
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
- 快照在 commit 后才拍 → rollback 语义错误。快照必须在覆盖 committed **之前**拍。
- show 命令的 case 顺序把无参提示放在最前 → 单参数命令被拦截。
- 参数节点双重语义（实例名 `<name>` 入路径 vs 取值 `<ip>` 不入路径）混淆 → cfgSet 解析错误。
