#!/usr/bin/env bash
# 阶段 5：操作模式其余命令 + show 管道 + vpp trace 正确时序
set -u
source "$(cd "$(dirname "$0")" && pwd)/cli-fulltest-lib.sh"

echo "############ 阶段 5：操作命令与管道 ############"
{ echo "############ 阶段 5：操作命令与管道 ############"; } >> "$LOG"

# ---- 其余操作命令（契约 §1.3）----
# ping 只覆盖 **VPP 数据面**（附录 A #89）。这里不用 run，也不用「预测环境」分流，而是断言
# **判定自洽**：0 发包必须报失败（修复前它把 0 发包算作 ✓，这条长期是假绿）；
# 真发过包则是环境相关结果。理由见 lib 里 expect_ping_coherent 的注释。
expect_ping_coherent S5 "ping 192.168.155.1"
expect_ping_coherent S5 "ping 192.168.155.1 count 2"
# source 须为**VPP 接口**地址（管理口 ens160 是内核口，不是 VPP 接口；用 vs-l3 的地址）
expect_ping_coherent S5 "ping 192.168.155.1 source 192.168.155.10 count 2"
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
