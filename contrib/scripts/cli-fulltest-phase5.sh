#!/usr/bin/env bash
# 阶段 5：操作模式其余命令 + show 管道 + vpp trace 正确时序
set -u
source "$(cd "$(dirname "$0")" && pwd)/cli-fulltest-lib.sh"

echo "############ 阶段 5：操作命令与管道 ############"
{ echo "############ 阶段 5：操作命令与管道 ############"; } >> "$LOG"

# ---- 其余操作命令（契约 §1.3）----
# ping 只覆盖 **VPP 数据面**（附录 A #89）：VPP 侧有 L3 地址时才是一条「功能用例」；
# 没有 L3 地址时本环境必然 0 发包，此时**必须**给出明确的 %% 失败。
#
# 为什么不能像以前那样无条件 run：修复前 `ping` 把 vppctl 原始输出（含
# `Statistics: 0 sent, 0 received, 0% packet loss`）原样返回且不报错，而判定只看 `^%` 与退出码
# ——于是「一个包都没发出去」被算作 **✓ 通过**（真机实测），这条用例长期是假绿。
# 两个分支都在验证真实行为，都不存在「盲过」。
if vppctl show interface address 2>/dev/null | grep -qE '^\s+[0-9a-fA-F:.]+/[0-9]'; then
  run S5 "ping 192.168.155.1"
  run S5 "ping 192.168.155.1 count 2"
  # source 须为**VPP 接口**地址（管理口 ens160 是内核口，不是 VPP 接口；用 vs-l3 的地址）
  run S5 "ping 192.168.155.1 source 192.168.155.10 count 2"
else
  # 0 发包必须判失败，且报错要点明平面口径（否则「0% 丢包」会被读成通了）
  expect_fail S5 "ping 192.168.155.1" "VPP 数据面"
  expect_fail S5 "ping 192.168.155.1 count 2" "VPP 数据面"
  expect_fail S5 "ping 192.168.155.1 source 192.168.155.10 count 2" "VPP 数据面"
fi
run S5 "traceroute 192.168.155.1"
run S5 "monitor interfaces ens224"
run S5 "monitor vnf cli-vm"
run S5 "clear interfaces statistics"
run S5 "clear interfaces statistics ens224"
run S5 "help"
run S5 "help show"
run S5 "help request"
run S5 "show version | match NFViS"
run S5 "show version | except NFViS"
run S5 "show interfaces | count"
run S5 "show log system | last 5"
run S5 "show configuration | display json"
run S5 "show interfaces | display json"
run S5 "show interfaces | begin system"

# ---- vpp trace 正确时序：start → export（会话进行中）→ stop ----
# trace：export 隐含 stop，故「start → export → 再 start → stop」验证两个方向
run S5 "request vpp trace start interface ens192 count 50"
run S5 "request vpp trace export name cli-trace2"
run S5 "show vpp capture"
run S5 "request vpp trace start interface ens192 count 50"
run S5 "request vpp trace stop"

# ---- 各 show 的二级子命令（契约 §1.1 全量）----
run S5 "show interfaces ens224 detail"
run S5 "show interfaces ens224 statistics"
run S5 "show interfaces ens224 sriov"
run S5 "show interfaces physical ens224 detail"
run S5 "show virtual-switches vs-l2 detail"
run S5 "show virtual-switches vs-l2 ports"
run S5 "show virtual-switches vs-l2 mac-table"
run S5 "show virtual-switches vs-l2 statistics"
run S5 "show vrfs"
run S5 "show vrfs vs-l3"
run S5 "show vrfs vs-l3 routes"
run S5 "show acls acl-test detail"
run S5 "show bonds bond0 detail"
run S5 "show lldp neighbors interface ens224"
run S5 "show log system level info last 5"
run S5 "show log audit last 5"
run S5 "show log vnf cli-vm last 5"
run S5 "show configuration permissions super-user"
run S5 "show virtual-machine-functions cli-vm detail"
run S5 "show virtual-machine-functions cli-vm interfaces"
run S5 "show virtual-machine-functions cli-vm statistics"
run S5 "show container-functions cli-ct2 interfaces"
run S5 "show images cli-test.qcow2 detail"

summary "阶段 5"
