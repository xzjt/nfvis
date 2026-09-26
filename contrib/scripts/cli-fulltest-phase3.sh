#!/usr/bin/env bash
# 阶段 3：前置对象（供阶段 4/5/6 操作）。**幂等**：重复运行只会因「无变更」报错，属正常。
#
# 创建：镜像（vm-image + container-image）、VM `cli-vm`（关机态）、容器 `cli-ct2`、
#       bond0、以及阶段 5 需要的 vs-l2/vs-l3/acl-test。
# 依赖：阶段 2 已建资源池（大页 + 隔离核）；Docker 本地有 alpine:3.20。
set -u
source "$(cd "$(dirname "$0")" && pwd)/cli-fulltest-lib.sh"

echo "############ 阶段 3：前置对象 ############"
{ echo "############ 阶段 3：前置对象 ############"; } >> "$LOG"

mkdir -p /data/incoming
cp -f /var/lib/nfvis/images/alpine.qcow2 /data/incoming/cli-test.qcow2 2>/dev/null
# cli-del.qcow2：**供阶段 4 真删**的一次性镜像（不声明进任何 VNF/容器 → 无引用，删除必成功）。
# 没有它就只剩「删历史残留的镜像名」这条路——那在本套件自己的产物之外，新装实例上并不存在。
cp -f /var/lib/nfvis/images/alpine.qcow2 /data/incoming/cli-del.qcow2 2>/dev/null

run S3 "request images upload name cli-test.qcow2 type vm-image file /data/incoming/cli-test.qcow2"
run S3 "request images upload name cli-del.qcow2 type vm-image file /data/incoming/cli-del.qcow2"

# 容器镜像：目录名必须**等于 Docker tag**（alpine:3.20），否则下发 Docker API 404
# （报 docker: not found）——见决策 #76 §4①。
docker save alpine:3.20 -o /data/incoming/ct.tar 2>/dev/null
run S3 "request images upload name alpine:3.20 type container-image file /data/incoming/ct.tar"

run S3 "configure
set virtual-machine-functions cli-vm image cli-test.qcow2
set virtual-machine-functions cli-vm vcpu count 1
set virtual-machine-functions cli-vm memory size-mb 1024
set virtual-machine-functions cli-vm memory hugepage-size 1G
set virtual-machine-functions cli-vm description cli-fulltest-vm
commit"

run S3 "configure
set container-functions cli-ct2 image alpine:3.20
set container-functions cli-ct2 vcpu count 1
set container-functions cli-ct2 memory size-mb 128
set container-functions cli-ct2 command /bin/sh
set bonds bond0 members 0 ens192
commit"

run S3 "show virtual-machine-functions"
run S3 "show container-functions"
run S3 "show images"

summary "阶段 3"
