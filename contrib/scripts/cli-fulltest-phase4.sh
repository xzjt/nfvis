#!/usr/bin/env bash
# 阶段 4：request 运维动作（不含 reboot/shutdown/zeroize/format-data 等破坏性命令）
set -u
source "$(cd "$(dirname "$0")" && pwd)/cli-fulltest-lib.sh"

echo "############ 阶段 4：request 运维动作 ############"
{ echo "############ 阶段 4：request 运维动作 ############"; } >> "$LOG"

# ---- VM 生命周期 ----
run S4 "request virtual-machine-functions cli-vm start"
run S4 "show virtual-machine-functions cli-vm"
run S4 "request virtual-machine-functions cli-vm restart"
run S4 "request virtual-machine-functions cli-vm stop"
run S4 "show virtual-machine-functions cli-vm"

# ---- 快照（决策 #75：需关机态）----
run S4 "request virtual-machine-functions cli-vm snapshot create name snap-cli"
run S4 "show virtual-machine-functions cli-vm snapshots"
run S4 "request virtual-machine-functions cli-vm snapshot rollback name snap-cli"
run S4 "request virtual-machine-functions cli-vm snapshot delete name snap-cli"
# 运行中创建**应被拒**（决策 #75 的守卫——报错才是正确行为）
run S4 "request virtual-machine-functions cli-vm start"
expect_fail S4 "需先关机" "request virtual-machine-functions cli-vm snapshot create name should-fail"
run S4 "request virtual-machine-functions cli-vm stop"

# ---- 容器生命周期 ----
run S4 "request container-functions cli-ct2 start"
run S4 "show container-functions cli-ct2"
run S4 "request container-functions cli-ct2 log"
run S4 "request container-functions cli-ct2 restart"
run S4 "request container-functions cli-ct2 stop"

# ---- 物理口 enable/disable ----
run S4 "request interfaces ens224 enable"
run S4 "request interfaces ens224 disable"

# ---- SR-IOV（环境无 PF/VF：**预期明确报错**，不得静默成功/崩溃）----
expect_fail S4 "不支持 SR-IOV" "request sriov create-vfs ens224 count 2"

# ---- VPP 抓包 ----
# 注意时序：export 内部即 Stop(export=true)（见 api.requestVppTrace），
# 故 export 之后不应再 stop（会正确地报「当前无抓包会话」）。
run S4 "request vpp trace start interface ens192 count 10"
run S4 "request vpp trace stop"
run S4 "request vpp trace start interface ens192 count 10"
run S4 "request vpp trace export name cli-trace"
run S4 "show vpp capture"

# ---- 系统运维（非破坏性）----
run S4 "request system tech-support generate"
run S4 "show system tech-support"
run S4 "request system configuration backup"
run S4 "request system ntp sync"
run S4 "request system api tls regenerate"
run S4 "request system ssh host-key regenerate"
run S4 "show system core-dumps"
run S4 "request alarms clear all"
run S4 "show alarms all"
run S4 "request system software rollback"

# ---- 镜像删除（引用检查）----
run S4 "request images delete name naming-test.tar"

summary "阶段 4"
