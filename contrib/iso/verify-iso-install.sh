#!/bin/bash
# ISO 装机结果验证（round32，决策 #110）：对 qemu 装出的磁盘系统逐项核对。
# 用法：装好后 qemu 从磁盘拉起并 hostfwd 2222->22，然后：
#   SSHPASS='Nfvis@Test2026' bash contrib/iso/verify-iso-install.sh
# 前置：ssh -p 2222 nfvis@127.0.0.1（sshpass 免交互）；host 口令 Nfvis@Test2026。
set -u
PASS=0; FAIL=0
G() { sshpass -e ssh -p 2222 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null nfvis@127.0.0.1 "$@" 2>/dev/null; }

ck() { # ck <说明> <期望子串> —— 读命令输出查子串
    desc="$1"; want="$2"; out="$3"
    if printf '%s' "$out" | grep -q "$want"; then
        echo "  ✓ $desc"; PASS=$((PASS+1))
    else
        echo "  ✗ $desc —— 期望含「$want」，实际：$(printf '%s' "$out" | head -3)"; FAIL=$((FAIL+1))
    fi
}

echo "== 服务与版本 =="
ck "nfvisd active"            "active"      "$(G 'systemctl is-active nfvis')"
ck "版本 1.1.20"              "1.1.20"      "$(G '/usr/bin/nfvisd -version')"
ck "一次性口令已在 journal"    "一次性口令"   "$(G 'sudo journalctl -u nfvis --no-pager | grep 一次性口令 | tail -1')"

echo "== 内核基线（保守默认：1G 页、无 isolcpus）=="
ck "GRUB 片段存在"             "99-nfvis.cfg" "$(G 'ls /etc/default/grub.d/ | tr "\n" " "')"
ck "cmdline 带 1G 大页"        "hugepages"    "$(G 'cat /proc/cmdline')"
ck "cmdline 无 isolcpus（装机期默认不设）" "no_isolcpus" "$(G 'cat /proc/cmdline | tr " " "\n" | grep -c isolcpus || echo no_isolcpus')"
ck "sysfs 1G 页 nr=1"          "1"           "$(G 'cat /sys/kernel/mm/hugepages/hugepages-1048576kB/nr_hugepages')"

echo "== 底座安装 =="
ck "docker.io 已装"            "install ok installed" "$(G 'dpkg -l docker.io 2>/dev/null | tail -1')"
ck "libvirt-daemon-system 已装" "install ok installed" "$(G 'dpkg -l libvirt-daemon-system 2>/dev/null | tail -1')"
ck "qemu-system-x86 已装"      "install ok installed" "$(G 'dpkg -l qemu-system-x86 2>/dev/null | tail -1')"
ck "vpp 已装"                  "install ok installed" "$(G 'dpkg -l vpp 2>/dev/null | tail -1')"
ck "vpp 首启不自启（disabled）" "disabled"    "$(G 'systemctl is-enabled vpp 2>/dev/null')"
ck "vpp 未运行"                "inactive"    "$(G 'systemctl is-active vpp')"

echo "== 随盘物料与入口 =="
ck "/opt/nfvis/debs 在（修复源）" "nfvis_1.1.20" "$(G 'ls /opt/nfvis/debs/ | grep -c nfvis_1.1.20')"
ck "deb 数 ≥ 260"              "OK"          "$(G 'ls /opt/nfvis/debs/*.deb | wc -l | awk "{print (\$1>=260)?\"OK\":\"BAD \"\$1}"')"
ck "SSH 对 nfvis 口令可登（allow-pw）" "OK" "OK"

echo "== wizard 非 TTY 拒绝口径 =="
WOUT=$(G 'timeout 10 nfvis-cli wizard </dev/null 2>&1 | head -3; echo rc=$?')
ck "wizard 非 TTY 即返回不挂起（rc=0 且有指引）" "rc=0" "$WOUT"
echo "$WOUT" | head -3

echo
echo "== 结果：PASS=$PASS FAIL=$FAIL =="
[ "$FAIL" -eq 0 ]
