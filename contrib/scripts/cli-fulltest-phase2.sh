#!/usr/bin/env bash
# 阶段 2：配置模式语句（契约 §2）逐条隔离测试。
#
# 手法：① 先用 run() 提交一批**前置对象**（资源池/物理口/交换机/ACL/QoS/SPAN/NAT），
#       使后续「实例内语句」有宿主元素；② 再把每条契约语句放进**独立会话**
#       `configure → <语句>`（不 commit、不手写 discard——脚本模式会自动收尾）逐条执行。
# 为什么独立会话：配置模式内任一行失败会输出 %% 并中止后续行，同会话串跑会**一条失败掩盖其余**。
# 判定：该语句自身报 % / %% 即失败。注意「语句未产生配置变更」也算失败输出——
#       它常见于**重复设置默认值**（用例写法问题，非缺陷），需人工判读。
set -u
source "$(cd "$(dirname "$0")" && pwd)/cli-fulltest-lib.sh"

echo "############ 阶段 2：配置语句 ############"
{ echo "############ 阶段 2：配置模式语句 ############"; } >> "$LOG"

# ---------- 前置对象（一次提交，使实例语句有宿主） ----------
run S2-pre "configure
set resource-pools hugepages page-size 1G count 2
set resource-pools cpu isolated-cores 1-4
set interfaces ens192 description cli-pre
set interfaces ens224 description cli-pre
set acls acl-test rule 10 source any destination any protocol tcp destination-port 443 action permit
set qos policies pol-test cir 1000000000 cbs 1000000
set port-mirroring pm-test source interface ens192 direction both
set port-mirroring pm-test analyzer interface ens224
set virtual-switches vs-l2 type l2
set virtual-switches vs-l3 type l3
set virtual-switches vs-l3 l3-interface ens192 ip address 192.168.155.10/24
set vpp cpu main-core 1
set vpp cpu corelist-workers 2
commit"

# ---------- 契约语句逐条（每条独立会话） ----------
while IFS= read -r stmt; do
  [ -z "$stmt" ] && continue
  case "$stmt" in \#*) continue;; esac
  run S2 "configure
$stmt"
done <<'EOF'
# —— system（§2.2）——
set system hostname nfvis-cli
set system timezone Asia/Shanghai
set system ntp server 192.168.155.1
set system ntp server 192.168.155.2 prefer
set system dns server 8.8.8.8 secondary 8.8.4.4
set system api port 18443
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
# —— resource-pools（§2.6）——
set resource-pools cpu numa node 0 cores 1-4
EOF

summary "阶段 2"
