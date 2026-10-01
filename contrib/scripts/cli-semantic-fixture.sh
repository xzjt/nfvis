#!/usr/bin/env bash
# NFViS 语义套件的「正向控制现场」——一台在虚拟交换机里**真的持续收发帧**的 VNF + 一个容器对象。
#
# 由来（决策 #328）：`cli-semantic-check.sh` 的 S8（`show virtual-switches <vs> mac-table` ↔ VPP l2fib）
# 语义是「**有流量时两侧必须一致**」。空现场里交换机没有能产生流量的成员 ⇒ 该项只能如实报「不可判定」。
# 本脚本用**产品既有手段**（不碰内核/GRUB/大页配置、不碰管理口）造出可判别的正控：
#   · 一台 L2 交换机 + BVI 三层网关（`set virtual-switches <vs> gateway ip`）
#   · 一台 VNF，其 vNIC 挂到该交换机；cloud-init 给 guest 配静态地址并**持续 ping 网关**
#     ⇒ guest 的帧从 vhost 成员口进来（该口 rx 增长）⇒ l2fib **学到 guest MAC**：
#       这正是 S8 需要的正向控制（成员口有学习条目 + 两个事实源可对照）。
#   · 一个容器对象（S9 的「容器列表」需要「有对象可对照」，否则两侧都空=不可判定）。
#
# 用法：
#   bash contrib/scripts/cli-semantic-fixture.sh up      # 造现场（先清后建，幂等）；成功即具备正控
#   bash contrib/scripts/cli-semantic-fixture.sh status  # 只看现场是否具备 S8/S9 正控条件（不改配置）
#   bash contrib/scripts/cli-semantic-fixture.sh down    # 清现场（停→删，并核对已清空）
#
# 环境（与 cli-semantic-check.sh 一致，可覆盖）：
#   SRV             缺省 https://127.0.0.1:443（已装实例）；开发态可 SRV=http://127.0.0.1:18443
#   CLI_BIN         缺省 /usr/bin/nfvis-cli；开发态可 /tmp/nfvis-cli
#   NFVIS_PASSWORD  管理口令（缺省 Admin@12345，与语义套件一致）
#   FIXTURE_IMAGE   VNF 用的 vm-image（缺省 debian-12-generic-amd64.qcow2）
#
# ⚠️ 真机踩坑（都已在本脚本里规避，勿退回去）：
#   1) **内存必须用 1G 大页**：本机 `/dev/hugepages` 以 `pagesize=1024M` 挂载，2M 大页**没有挂载点**，
#      libvirt 会以 `定义 domain 失败: invalid argument` 拒绝定义 domain（1G/1024MB 才成）。
#   2) **libvirtd 重启过而 nfvisd 未重启**时，VNF 定义会以同一个 `invalid argument` 失败
#      （nfvisd 缓存的 libvirt 连接已失效）——先 `systemctl restart nfvis` 再重试（本轮真机实测）。
#   3) **guest 需要能读 NoCloud seed**（产品把 seed 以 **virtio 盘的 iso9660** 交付，决策 #139）。
#      本机实测 **Debian 12 cloud image 生效**；Alpine cloud image 未应用 user-data（其内核 isofs
#      只作为模块存在、镜像无 udev 自动加载，seed 未挂上）——故缺省用 Debian。
#   4) guest 的 vNIC 在这些云镜像里通常叫 **enp1s0**（不是 eth0）：user-data 必须**按实际口名**操作，
#      脚本用「枚举 /sys/class/net 里第一个非 lo 口」避免写死。
#
# 与语义套件的关系：本脚本**只造现场**，判定的三档口径仍由 cli-semantic-check.sh 决定——
# 没有流量时 S8 依旧报「不可判定」（本脚本不改变判定，只让「有流量」这一条件成立）。
set -u

SRV=${SRV:-https://127.0.0.1:443}
CLI_BIN=${CLI_BIN:-/usr/bin/nfvis-cli}
PW=${NFVIS_PASSWORD:-Admin@12345}
IMAGE=${FIXTURE_IMAGE:-debian-12-generic-amd64.qcow2}

VS=sem-vs                 # L2 交换机名
VM=sem-vm                 # VNF 名
CT=sem-ct                 # 容器名
NIC=nic0                  # VNF 的 vNIC 名
GW_IP=192.168.99.1/24     # 交换机 BVI 网关
GUEST_IP=192.168.99.2/24  # guest 静态地址
UD=/root/sem-fixture-user-data.yaml   # cloud-init user-data（产品支持 user-data 取「文本或文件」）

cli() { "$CLI_BIN" -server "$SRV" -u admin -p "$PW" -source console -c "$1" 2>&1 | grep -v '^连接'; }
# 用**文件**交付 user-data（决策 #114：取值可为文件路径；比内联多行更稳，不受 CLI 引号/换行影响）
write_userdata() {
  cat > "$UD" <<EOF
#cloud-config
write_files:
  - path: /usr/local/bin/nfvis-sem-beat.sh
    permissions: "0755"
    content: |
      #!/bin/sh
      for i in \$(ls /sys/class/net); do
        [ "\$i" = lo ] && continue
        ip link set "\$i" up
        ip addr add ${GUEST_IP%/*}/${GUEST_IP#*/} dev "\$i" 2>/dev/null
      done
      while true; do ping -c1 -W2 ${GW_IP%/*} >/dev/null 2>&1; sleep 1; done
runcmd:
  - [ sh, -c, "nohup /usr/local/bin/nfvis-sem-beat.sh >/tmp/nfvis-sem-beat.log 2>&1 &" ]
EOF
}

precheck() {
  if [ ! -x "$CLI_BIN" ]; then echo "✗ 找不到可执行的 CLI: $CLI_BIN（用 CLI_BIN=… 指定）"; exit 1; fi
  if ! command -v vppctl >/dev/null 2>&1; then echo "✗ 找不到 vppctl（本脚本需要 VPP 作独立事实源）"; exit 1; fi
  if ! cli "show version" | grep -q NFViS; then
    echo "✗ CLI 无法通过 $SRV 取到版本（确认实例已起、口令正确：NFVIS_PASSWORD=… SRV=… CLI_BIN=…）"
    exit 1
  fi
}

# <ifname> → rx 包数（取不到 → 0）。注意 VPP 把「rx packets」打在**接口名下那一行**，
# 只有 rx bytes/drops 等才是缩进续行——按 `$1=="rx"` 认行会**恒得 0**（真机实测踩过）。
if_rx() {
  vppctl show interface "$1" 2>/dev/null | tr -d '\r' \
    | awk '{for (i = 1; i < NF; i++) if ($i == "rx" && $(i+1) == "packets") {print $(i+2); exit}}' \
    | grep -E '^[0-9]+$' || echo 0
}
# 该 VNF 的 vNIC 在 VPP 里的接口名（产品命名规则：vh-<vm>-<nic>）
vnic_if() { echo "vh-$VM-$NIC"; }

has_obj() { # has_obj <show 子命令> <名>：对象存在才返回 0（避免删不存在对象让整次提交中止）
  cli "show $1" | awk -v n="$2" 'NR>1 && $1==n {f=1} END{exit !f}'
}

do_down() {
  precheck
  echo "· 停止并删除现场对象（存在才删——删不存在的对象会让整次提交中止）"
  has_obj virtual-machine-functions "$VM" && cli "request virtual-machine-functions $VM stop" >/dev/null 2>&1
  has_obj container-functions "$CT" && cli "request container-functions $CT stop" >/dev/null 2>&1
  sleep 3
  # 按「先解引用、后删被引用」的顺序（决策 #196）：引用方（VNF/容器）先删，交换机后删。
  # 三者各用一次提交，任一不存在也不牵连其它（脚本模式遇错即停）。
  if has_obj virtual-machine-functions "$VM"; then
    cli "configure
delete virtual-machine-functions $VM
commit" 2>&1 | grep -E '^%%|错误' || true
  fi
  if has_obj container-functions "$CT"; then
    cli "configure
delete container-functions $CT
commit" 2>&1 | grep -E '^%%|错误' || true
  fi
  if cli "show configuration" | grep -q "^virtual-switches $VS {"; then
    cli "configure
delete virtual-switches $VS
commit" 2>&1 | grep -E '^%%|错误' || true
  fi
  sleep 2
  echo "· 终态核对"
  echo "  virtual-switches: $(cli 'show virtual-switches' | tr '\n' ' ')"
  echo "  VNF:              $(cli 'show virtual-machine-functions' | tr '\n' ' ')"
  echo "  容器:             $(cli 'show container-functions' | tr '\n' ' ')"
  rm -f "$UD"
}

do_up() {
  precheck
  do_down   # 先清后建，幂等
  echo "· 写 cloud-init user-data → $UD"
  write_userdata
  echo "· 建交换机 $VS（L2 + BVI $GW_IP）、VNF $VM（镜像 $IMAGE，1G 大页 1024MB，vNIC→$VS）、容器 $CT"
  cli "configure
set virtual-switches $VS type l2
set virtual-switches $VS gateway ip $GW_IP
set virtual-machine-functions $VM image $IMAGE
set virtual-machine-functions $VM vcpu count 1
set virtual-machine-functions $VM memory size-mb 1024
set virtual-machine-functions $VM memory hugepage-size 1G
set virtual-machine-functions $VM interfaces $NIC type vhost-user
set virtual-machine-functions $VM interfaces $NIC virtual-switch $VS
set virtual-machine-functions $VM cloud-init user-data $UD
set container-functions $CT image alpine:3.20
set container-functions $CT command /bin/sleep
set container-functions $CT args 3600
commit"
  echo "· 启动 VNF 与容器"
  cli "request virtual-machine-functions $VM start"
  cli "request container-functions $CT start"
  echo "· 等待 guest 起网并产生流量（最多 ${FIXTURE_WAIT:-600}s；Debian cloud 首启含 cloud-init 需数分钟）"
  local ifn base cur i n
  ifn=$(vnic_if); base=$(if_rx "$ifn"); n=$(( ${FIXTURE_WAIT:-600} / 5 ))
  for i in $(seq 1 "$n"); do
    sleep 5
    cur=$(if_rx "$ifn")
    if [ "$cur" -gt "$base" ] 2>/dev/null; then
      echo "  ✓ 成员口 $ifn rx 增长：$base → $cur（帧确实从成员口进来）"
      break
    fi
    if [ "$i" = "$n" ]; then
      echo "  ✗ ${FIXTURE_WAIT:-600}s 内 $ifn 的 rx 未增长——guest 未产生流量"
      echo "    诊断：virsh domstate $VM; vppctl show interface $ifn; vppctl show ip neighbors"
      echo "    （可调 FIXTURE_WAIT=900 再等；或确认镜像 $IMAGE 的 cloud-init 能读 NoCloud seed）"
      return 1
    fi
  done
  echo "· 现场证据（独立事实源）"
  echo "  -- 读视图（产品；含派生条目）:"; cli "show virtual-switches $VS ports" | sed 's/^/     /'
  echo "  -- VPP 成员口 $ifn:"; vppctl show interface "$ifn" 2>&1 | tr -d '\r' | sed 's/^/     /'
  echo "  -- VPP l2fib:"; vppctl show l2fib verbose 2>&1 | tr -d '\r' | sed 's/^/     /'
  echo "  -- VPP 邻居表:"; vppctl show ip neighbors 2>&1 | tr -d '\r' | sed 's/^/     /'
  echo "  -- CLI mac-table:"; cli "show virtual-switches $VS mac-table" | sed 's/^/     /'
  echo "· 正控已就绪：跑 bash contrib/scripts/cli-semantic-check.sh（S8 应可判定并通过）"
}

do_status() {
  precheck
  echo "· 交换机:"; cli "show virtual-switches" | sed 's/^/   /'
  echo "· 读视图（$VS）:"; cli "show virtual-switches $VS ports" | sed 's/^/   /'
  ifn=$(vnic_if)
  echo "· 成员口 $ifn rx=$(if_rx "$ifn")"
  echo "· libvirt: $(virsh domstate "$VM" 2>&1)"
  echo "· Docker:  $(docker ps -a --format '{{.Names}} {{.Status}}' 2>/dev/null | grep "^$CT " || echo '（无）')"
}

case "${1:-}" in
  up)     do_up;;
  down)   do_down;;
  status) do_status;;
  *) echo "用法: bash $0 {up|down|status}"; exit 2;;
esac
