#!/usr/bin/env bash
# 阶段 2：配置模式语句（契约 §2）逐条隔离测试。
#
# 手法：① 先用 run() 提交一批**前置对象**（资源池/物理口/交换机/ACL/QoS），
#       使后续「实例内语句」有宿主元素（SPAN 语句见下方独立会话，不提交）；
#       ② 再把每条契约语句放进**独立会话**
#       `configure → <语句>`（不 commit、不手写 discard——脚本模式会自动收尾）逐条执行。
# 为什么独立会话：配置模式内任一行失败会输出 %% 并中止后续行，同会话串跑会**一条失败掩盖其余**。
# 判定：该语句自身报 % / %% 即失败。「语句未产生配置变更」（值未变化）自 v1.1.48 起是
#       **提示**（非 % 前缀、不中止脚本），因此不再计入失败（幂等重跑可用）。
set -u
source "$(cd "$(dirname "$0")" && pwd)/cli-fulltest-lib.sh"

echo "############ 阶段 2：配置语句 ############"
{ echo "############ 阶段 2：配置模式语句 ############"; } >> "$LOG"

# ---------- 前置对象（一次提交，使实例语句有宿主） ----------
# 端口角色互斥（commit 校验）：一个业务口在 {bond 成员 / 交换机端口 / L3 接口 / 镜像源或分析口}
# 中只能出现一次。本机业务口只有 ens192/ens224 两个，而阶段 3 把 ens192 收进 bond0、
# 阶段 5 要用 vs-l3 的地址（192.168.155.10）做 ping source，故这里把 L3 接口放在 ens224。
# **不提交镜像会话**：镜像的源口与分析口必占两个物理口，与上述二者相撞（此前本块用
# ens192 同时当 vs-l3 的 l3-interface 与镜像源口，提交必被新校验拒绝）；镜像语句本身的
# 覆盖由下方 pm-test2 两条独立会话承担（不 commit，故不落进配置）。
run S2-pre "configure
set resource-pools hugepages page-size 1G count 2
set resource-pools cpu isolated-cores 1-4
set interfaces ens192 description cli-pre
set interfaces ens224 description cli-pre
set acls acl-test rule 10 source any destination any protocol tcp destination-port 443 action permit
set qos policies pol-test cir 1000000000 cbs 1000000
set virtual-switches vs-l2 type l2
set virtual-switches vs-l2 gateway ip 192.168.100.1/24
set virtual-switches vs-l3 type l3
set virtual-switches vs-l3 l3-interface ens224 ip address 192.168.155.10/24
set vpp cpu main-core 1
set vpp cpu corelist-workers 2
commit"

# ---------- 契约语句逐条（每条独立会话） ----------
while IFS= read -r stmt; do
  [ -z "$stmt" ] && continue
  case "$stmt" in \#*) continue;; esac
  stmt=${stmt//__API_PORT__/$API_PORT}   # 端口参数化（见 lib 里的 API_PORT）
  run S2 "configure
$stmt"
done <<'EOF'
# —— system（§2.2）——
set system hostname nfvis-cli
set system timezone Asia/Shanghai
set system ntp server 192.168.155.1
set system ntp server 192.168.155.2 prefer
set system dns server 8.8.8.8 secondary 8.8.4.4
set system api port __API_PORT__
set system api token-ttl-minutes 60
set system api max-sessions 8
set system api tls self-signed regenerate
set system management interface ens160
set system idle-timeout-minutes 10
set system kernel nmi-watchdog false
set system kernel transparent-hugepages madvise
set system kernel iommu on
set system kernel tuned-profile throughput-performance
set system kernel params audit=0
set system health thresholds cpu-temp-celsius 90
set system health thresholds disk-temp-celsius 60
set system health thresholds disk-used-percent 85
set system syslog host 192.168.155.1 port 514 facility local0 severity info
set system syslog local level info
set system syslog local retention-days 14
set system syslog local max-size-mb 200
set system login password-policy min-length 12
set system login password-policy complexity true
set system login password-policy expire-days 90
set system login password-policy lockout-threshold 5
set system login password-policy lockout-minutes 15
# —— protocols lldp（§2.2b）——
set protocols lldp enable true
set protocols lldp advertisement-interval 30
set protocols lldp interface ens224 enable true
# —— interfaces / bonds（§2.3）——
set interfaces ens224 mtu 9000
set interfaces ens224 disable
set bonds bond0 members 0 ens192
set bonds bond0 lacp mode active interval fast
set bonds bond0 mtu 9000
set bonds bond0 description cli-bond
# —— vpp（§2.9）——
set vpp memory main-heap-size 1G
set vpp memory buffers-per-numa 16384
set vpp memory hugepage-preference 1G
set vpp dpdk dev rx-queues 2
set vpp dpdk dev tx-queues 2
set vpp dpdk dev rx-descriptors 1024
set vpp dpdk dev tx-descriptors 1024
set vpp dpdk dev ens192 rx-queues 4
set vpp dpdk uio-driver vfio-pci
set vpp plugins acl state enable
# —— acls / qos / port-mirroring / nat（§2.5）——
set acls acl-test rule 20 source 10.0.0.0/8 destination any protocol any action deny
set acls acl-test rule 10 direction ingress
set qos policies pol-test2 cir 2000000000 cbs 2000000
set port-mirroring pm-test2 source interface ens224 direction ingress
set port-mirroring pm-test2 analyzer interface ens192
set nat source-pool pool-test address-range 203.0.113.1 to 203.0.113.10
set nat static 10.0.0.5 to 203.0.113.5
# —— virtual-switches（§2.4）——
set virtual-switches vs-l2 vlan access 100
set virtual-switches vs-l3 static-routes 10.99.0.0/16 next-hop 192.168.155.1
set virtual-switches vs-l3 static-routes default next-hop 192.168.155.1
# —— virtual-switches dhcp-relay（§2.4；决策 #335，仅已配网关的 L2 交换机可配）——
# 前置：S2-pre 里给 vs-l2 配了网关（中继源地址自动取 BVI 的 IPv4 地址），relay 语句才可提交。
set virtual-switches vs-l2 dhcp-relay server 192.168.100.2
delete virtual-switches vs-l2 dhcp-relay
# —— virtual-switches learn-limit（§2.4；决策 #337，MAC 学习上限=环路缓解，不依赖网关）——
set virtual-switches vs-l2 learn-limit 8192
delete virtual-switches vs-l2 learn-limit
# —— 数据面 DNS 代理（§2.2 全局 / §2.4 按域；决策 #345，自研转发器 + punt socket）——
# 仅验证解析与落点（本阶段不 commit）；数据面生效与读视图对照见语义套件 DNS 项与真机走查。
set system dns proxy server 8.8.8.8 secondary 8.8.4.4
delete system dns proxy server 8.8.4.4
delete system dns proxy server secondary 8.8.4.4
delete system dns proxy server
set virtual-switches vs-l2 dns proxy server 10.0.0.53
delete virtual-switches vs-l2 dns proxy server secondary 10.0.0.54
delete virtual-switches vs-l2 dns proxy server
# —— resource-pools（§2.6）——
set resource-pools cpu numa node 0 cores 1-4
# —— system login 横幅（§2.2；决策 #303）——
# 单行、≤512 字节；上面「逐条独立会话」的形态只验证解析/接线，真落库与清除见下方提交往返。
set system login banner 本机为套件测试实例，请勿用于生产
EOF

# ---------- 登录横幅：真落库 → 回读 → 清除（决策 #303）----------
# 单条语句只在独立会话里解析不够——横幅的价值在「提交后真的进了配置、删得掉」。
# 这是一次真实提交往返（值含中文与连字符），提交后立即删除，不改变机器的长期现场。
run S2-banner "configure
set system login banner 套件横幅-roundtrip
commit"
# 写→读往返：只断言「提交不报错」等于假绿（值没落库也照样 0 退出），故要求配置回读里
# 真的出现刚写的值（决策 #303 的 banner 落在 system.login.banner）。
expect_out S2-banner "套件横幅-roundtrip" "show configuration"
run S2-banner "configure
delete system login banner
commit"

summary "阶段 2"
