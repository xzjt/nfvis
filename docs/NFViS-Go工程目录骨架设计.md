# NFViS Go 工程目录骨架设计

| 文档属性 | 内容 |
|---|---|
| 版本 | V1.0 |
| 上游文档 | 《NFViS 系统产品需求与目标架构需求规格书》V1.0 |
| 日期 | 2026-09-12 |

## 1. 总体原则

1. **单一数据模型**：配置模型定义一份（`internal/model`），schema 驱动 CLI 补全、API 校验、事务引擎三处消费。
2. **底座适配层可替换**：VPP / libvirt / Docker 全部藏在接口后面（`orchestrator` 包），业务逻辑只依赖接口，单测用 mock，不依赖真实底座。
3. **标准 Go 布局**：`cmd/` 只放入口，业务全在 `internal/`；两个二进制（`nfvisd`、`nfvis-cli`）同仓同版本发布（同一 deb）。
4. **CLI 薄客户端**：`nfvis-cli` 不含业务逻辑，补全依据的命令树 schema 在编译期共享（见 §3.3），执行一律转发 `nfvisd`。

## 2. 目录结构

```
nfvis/
├── go.mod                          # module github.com/<org>/nfvis
├── Makefile                        # build / test / lint / deb 打包
├── api/
│   └── openapi.yaml                # OpenAPI 3.0 规范（随产品发布，见配套文档）
├── cmd/
│   ├── nfvisd/
│   │   └── main.go                 # 守护进程入口：装配依赖、信号处理、优雅退出
│   └── nfvis-cli/
│       └── main.go                 # CLI 入口：无参数→交互模式；-c "cmd"→单命令(脚本化)
├── internal/
│   ├── model/                      # ★ 配置模型（单一数据源）
│   │   ├── model.go                #   强类型配置结构（System, Interface, VirtualSwitch,
│   │   │                           #   Vrf, Acl, Nat, PortMirroring, Qos, ResourcePool,
│   │   │                           #   VMFunction, ContainerFunction …）
│   │   ├── validate.go             #   语义校验（引用存在性、配额、地址冲突）
│   │   └── diff.go                 #   模型 diff（配置 compare 用）
│   ├── schema/                     # ★ 命令树/schema（CLI 补全 + 校验共用）
│   │   ├── node.go                 #   Node 类型：关键字/参数/取值枚举/描述/class权限位
│   │   ├── tree_oper.go            #   操作模式命令树定义
│   │   ├── tree_config.go          #   配置模式命令树定义
│   │   └── gen_test.go             #   schema 一致性测试（命令树⇄模型字段映射）
│   ├── config/                     # ★ 事务引擎 + 持久化
│   │   ├── engine.go               #   candidate 生命周期：edit/discard/commit/confirmed/rollback
│   │   ├── store_sqlite.go         #   SQLite 存储：committed 配置、rollback 历史(50)、candidate 会话锁
│   │   ├── apply.go                #   commit 时调度 orchestrator 下发（网络→计算→容器，含失败补偿）
│   │   └── migration.go            #   版本升级时的配置 schema 迁移
│   ├── orchestrator/               # ★ 底座适配层（接口 + 实现）
│   │   ├── provider.go             #   NetworkProvider / ComputeProvider / ContainerProvider 接口
│   │   ├── network/                #   govpp 实现：接口/BD/VRF/ACL/NAT/SPAN/policer
│   │   │   ├── vpp.go              #     govpp 连接管理、重连、重放
│   │   │   ├── bridge_domain.go
│   │   │   ├── l3.go
│   │   │   ├── acl.go
│   │   │   ├── nat.go
│   │   │   └── qos_span.go
│   │   ├── compute/                #   libvirt 实现：domain 定义/生命周期/console/快照
│   │   │   ├── libvirt.go
│   │   │   ├── domain_xml.go       #   XML 组装（vhost-user/sriov hostdev/大页/绑核/串口）
│   │   │   └── cloudinit.go        #   NoCloud seed ISO 生成
│   │   └── container/
│   │       └── docker.go           #   容器生命周期、镜像 pull/rm、memif socket 挂载
│   ├── images/                     # 镜像仓库：本地目录、URL 拉取(断点续传/sha256)、元数据
│   ├── state/                      # 运行态查询（实时读底座，不落库）
│   │   ├── state.go                #   StateReader 接口
│   │   └── collect.go              #   show/API 详情数据组装
│   ├── api/                        # REST server（消费 config/orchestrator/state/events）
│   │   ├── server.go               #   路由、TLS、Token 认证中间件、错误统一格式
│   │   ├── handlers_*.go           #   按资源的 handler（system/vswitch/vmfn/...）
│   │   └── cli_bridge.go           #   CLI 专用端点：执行命令树节点(内部socket, 短路HTTP开销)
│   ├── events/                     # 事件总线 + 告警表（SQLite）+ SSE 推送
│   ├── recovery/                   # 启动收敛：committed ⇄ 运行态对比、VPP 重启重放
│   ├── metrics/                    # Prometheus collector（系统/VPP/VNF 指标）
│   ├── audit/                      # 审计日志（独立 SQLite 表 + journald 转发）
│   └── aaa/                        # 本地用户/login class、口令策略、Token 签发校验
├── pkg/                            # 可被外部引用的稳定 API（原则上只放 nfvis-cli 客户端库）
│   └── cliclient/                  #   Go 客户端 SDK（Web 控制面也可复用）
├── deploy/
│   ├── systemd/nfvisd.service
│   ├── debian/                     # deb 打包（postinst：安装期底座优化、驱动接管脚本）
│   └── installer/                  # 整机安装器（分区、大页内核参数、isolcpus 基线）
├── docs/                           # 命令树文档、支持矩阵、验收基准
└── test/
    ├── integration/                #   需要真实 VPP/libvirt 的集成测试（可跑于容器）
    └── e2e/                        #   CLI/API 端到端场景（登录→建交换机→部署VM→验证连通）
```

## 3. 关键设计说明

### 3.1 依赖方向（自上而下，禁止反向）

```
cmd/nfvisd → api, recovery, metrics → config(事务) → orchestrator(接口) → model
                                     ↘ events / audit / aaa
cmd/nfvis-cli → internal/cli(编辑器) → schema + pkg/cliclient
```

`internal/cli`（CLI 前端）不允许 import `config`/`orchestrator`——它只能通过 `pkg/cliclient` 访问守护进程。这是"薄客户端"原则在编译期的强制。

### 3.2 底座接口（`orchestrator/provider.go`）示意

```go
type NetworkProvider interface {
    ApplyBridgeDomain(ctx context.Context, vs model.VirtualSwitch) error
    DeleteBridgeDomain(ctx context.Context, name string) error
    ApplyVRF(ctx context.Context, vrf model.Vrf) error
    ApplyACL(ctx context.Context, acl model.Acl) error
    // ... Span/QoS/NAT 同理
    EnsureConsistent(ctx context.Context, cfg model.Config) []error  // 恢复收敛用
}
type ComputeProvider interface {
    DefineVM(ctx context.Context, vm model.VMFunction, res model.AllocatedResources) error
    StartVM / StopVM / SnapshotVM / ConsoleEndpoint ...
    EnsureConsistent(ctx context.Context, cfg model.Config) []error
}
type ContainerProvider interface{ /* 同理 */ }
```

事务引擎 `config/apply.go` 按依赖顺序（资源池→网络→VM/容器）调用 Provider，任一步失败即执行逆序补偿并使 commit 报错（底座回到变更前状态），保证 JunOS 式"全有或全无"。

### 3.3 命令树 schema 的归属（决策）

`internal/schema` 定义命令树，被两个二进制**编译期共享**：

- `nfvisd`：用于 API 校验与 `cli_bridge` 端点；
- `nfvis-cli`：用于 `?`/Tab 补全与本地语法提示。

理由：两个二进制同 deb 同版本发布，编译期共享无漂移风险，且补全不依赖守护进程存活（连接断开时仍可编辑提示，执行时才报连接错误）。备选方案（守护进程动态下发 schema）在"CLI 与 nfvisd 版本可能不一致"的场景才有价值，V1 排除。

### 3.4 事务引擎核心状态机（`config/engine.go`）

```
        edit()                    set/delete...               commit
无锁 ──────────► 持锁(有candidate) ─────────────► 持锁(dirty) ──────► 校验+下发+落库
 ▲                   │    discard()/超时释放           │  confirmed: commit confirmed N
 │                   └────── discards ───────► 无锁(cleanup)      │    ├─ 确认: 提交生效
 └── rollback N: 取历史committed为candidate，再 commit ────────────┘    └─ 超时: 自动回滚+告警
```

- candidate 会话锁记录在 SQLite（持有者、获取时间、最后活动时间），空闲超时自动释放。
- `commit confirmed` 计时器在 nfvisd 侧（而非 CLI 侧），CLI 断线不影响定时回滚。
- 每次 commit 成功后配置快照编号 +1，保留最近 50 份。

### 3.5 启动装配（`cmd/nfvisd/main.go`）

```go
// 伪代码：装配顺序即依赖顺序
store   := config.NewSQLiteStore(cfg.DBPath)
netp    := network.New(govpp.Connect(vppSock))
compp   := compute.New(libvirt.Connect(qemuSock))
contp   := container.New(dockerSock)
engine  := config.NewEngine(store, apply.New(netp, compp, contp))
events  := events.NewBus(store)
recovery.Run(ctx, engine, netp, compp, contp)   // 启动收敛（阻塞直到完成或降级告警）
api.Listen(engine, state, events, aaa, metrics)
```

## 4. 编码与质量约定

| 项 | 约定 |
|---|---|
| Go 版本 | 1.26+；`gofumpt`、`golangci-lint`（errcheck、govet、staticcheck 必开） |
| 错误处理 | 业务错误用 `fmt.Errorf("...: %w")` 包装；对外 API 错误统一转 `{code, message, detail[]}` |
| 日志 | `log/slog`，JSON handler；所有编排操作带 `op`、`user` 字段以便审计关联 |
| 并发 | 事务引擎内部串行（单写多读）；Provider 实现需声明是否并发安全 |
| 测试 | 单测：model/schema/config（mock Provider）；集成：真实 VPP 容器 + libvirt（test/integration）；覆盖率门槛：config/schema ≥ 70% |
| govpp | 用 `govpp` 核心 + 生成绑定（binapi 26.06），连接断开自动重连并触发恢复收敛 |
| 数据库 | SQLite（`modernc.org/sqlite` 纯 Go 驱动，免 CGO，交叉编译友好） |

## 5. 里程碑建议（实现顺序）

1. **M1**：model + schema + config 事务引擎（SQLite，mock Provider 全绿）——系统语义先于底座。
2. **M2**：api + aaa + cli_bridge；nfvis-cli 交互/补全跑通（此时可完整演示 CLI 事务，不含真实网络）。
3. **M3**：orchestrator/network（govpp：BD/VRF/ACL/NAT/SPAN/QoS）+ recovery。
4. **M4**：orchestrator/compute + container + images（vhost-user VM、cloud-init、memif 容器）。
5. **M5**：events/metrics/audit 收尾、deb 打包、e2e 与验收基准测试。
