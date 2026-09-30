#!/usr/bin/env bash
# 阶段 4：request 运维动作（**不执行** reboot/shutdown/zeroize/software rollback 等破坏性命令：
#         它们依次会真重启/真关机/真清库/真回退，故一律不给 --yes，只断言「非交互下被明确拒绝」）。
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

# ---- 破坏性命令：非交互模式（-c）下**必须被明确拒绝** ----
# 这些命令由服务端以 `… ? [yes,no]` 问询确认；脚本模式没有答复来源，CLI 必须报错并中止
# （修复前它只打印问句、什么也不做却回退出码 0 —— 那时这几条是**假绿**）。
# **绝不给它们加 --yes**：加了就真重启/真关机/真清库/真回退。期望关键词与 CLI 的文案一致。
expect_fail S4 "需交互确认" "request system reboot"
expect_fail S4 "需交互确认" "request system shutdown"
expect_fail S4 "需交互确认" "request system zeroize"
expect_fail S4 "需交互确认" "request system software rollback"
expect_fail S4 "需交互确认" "request interfaces ens224 unbind-dpdk"
# 决策 #305 的 `request system storage format-data`（恢复出厂数据状态）：**不真执行**，
# 只做**结构检查**——非交互下必须被双重确认问询挡住（与 zeroize 同一口径）。
# 这条断言的就是「命令已接线 + 破坏性闸门在位」，它同时保证该命令不会被静默执行。
expect_fail S4 "需交互确认" "request system storage format-data"

# ---- 逐 token 吊销（决策 #301）：不存在的 id 必须被明确拒绝，且不泄露存在性 ----
# 正向路径（吊销真实会话）在 semantic 套件的两会话场景里断言（那里才有第二个会话可用）。
expect_fail S4 "会话不存在或无权操作" \
  "request system api token revoke 00000000-0000-4000-8000-000000000000"

# ---- 镜像删除：无害对象**真删**（这才是真覆盖）----
# cli-del.qcow2 由阶段 3 创建、无任何引用 → 加 --yes 走完整「确认 → 真删」路径。
run S4 "request images delete name cli-del.qcow2 --yes"
# 仍被 cli-vm 引用（阶段 3）：带 --yes 也必须被引用检查拒绝。
expect_fail S4 "镜像被引用" "request images delete name cli-test.qcow2 --yes"

summary "阶段 4"
