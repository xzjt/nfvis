#!/usr/bin/env bash
# 阶段 8：镜像启动画像（决策 #485）——vm-image-profiles 镜像级画像 + VNF startup 实例覆盖。
#
# 覆盖原先「真机验证待执行」的 21 条契约命令（`show vm-image-profiles` 1 条、
# `set/delete virtual-machine-functions <n> startup …` 10 条、`set/delete vm-image-profiles …` 10 条）：
#   · 镜像画像：九个叶子各设一次并提交 → 读视图（值 + extra-args 的「已绕过资源账本」标注）→
#     display set 反推等价语句 → 逐叶子删 / 裸 delete 清节两形态；
#   · 实例覆盖：在 cli-vm 上设 startup 叶子（生效优先级 实例覆盖 > 镜像画像 > 产品默认），
#     `show virtual-machine-functions cli-vm detail` 的「有效启动画像」块按来源标注，再清覆盖；
#   · 防呆（提交期必须拒绝并点名）：跨字段组合（q35+ide、tcg+host-passthrough）、extra-args denylist、
#     telnet 控制台端口由 VM 名派生撞车（点名两台 VM）。
#
# 判据与其他阶段同源：run 量「命令能不能用」；expect_out 量「答的是不是这件事」（读视图 / display
# set 真含刚写的值——只返回 0 不算）；expect_fail 量「该拒的拒了」。用**已在镜像仓库**的 vm-image
# 作画像键（优先阶段 3 夹具 cli-test.qcow2）——缺则相应段落**如实跳过**，不伪装通过。
# 依赖：阶段 3 已上传 vm-image 并建 cli-vm（缺则跳过；不会因此判失败）。
set -u
source "$(cd "$(dirname "$0")" && pwd)/cli-fulltest-lib.sh"

echo "############ 阶段 8：镜像启动画像 ############"
{ echo "############ 阶段 8：镜像启动画像 ############"; } >> "$LOG"

# expect_fail_all：命令必须失败，且输出**同时**含两个关键词。
# 用于 telnet 控制台端口撞车——报错要点名两台 VM（`virtual-machine-functions[vm54]` 路径里一台 +
# 消息里的 `VNF vm29`），而 expect_fail 只收一个关键词，分开判会漏判「只点名一台」。
expect_fail_all() { # expect_fail_all <阶段> <关键词1> <关键词2> <命令>
  local phase="$1"; shift
  local k1="$1"; shift
  local k2="$1"; shift
  local cmd="$*" out rc
  out=$($CLI -c "$cmd" 2>&1); rc=$?
  out=$(printf '%s\n' "$out" | sed '1{/^连接 /d;}')
  if printf '%s\n' "$out" | grep -qF -- "$k1" && printf '%s\n' "$out" | grep -qF -- "$k2"; then
    EXPECTED=$((EXPECTED+1)); printf '  ⊘ %s（预期报错，含「%s」「%s」）\n' "$cmd" "$k1" "$k2"
  else
    FAIL=$((FAIL+1))
    FAILED_LIST+=("[$phase] $cmd :: 本应同时报「$k1」「$k2」但得到: $(printf '%s' "$out" | head -1)")
    printf '  ✗ %s\n' "$cmd"
    printf '%s\n' "$out" | sed 's/^/       | /' | head -5
  fi
  printf '=== [%s][expect-fail-all] %s\n%s\n\n' "$phase" "$cmd" "$out" >> "$LOG"
}

# 选画像键（＝镜像名）：优先阶段 3 的夹具 cli-test.qcow2，回退 alpine / 仓库里首个 vm-image。
mkdir -p "$SUITE_STATE_DIR"
IMG=""
imgs=$(_suite_cli "show images")
for cand in cli-test.qcow2 alpine alpine.qcow2; do
  if printf '%s\n' "$imgs" | grep -qE "^$cand([[:space:]]|$)"; then IMG=$cand; break; fi
done
if [ -z "$IMG" ]; then
  IMG=$(printf '%s\n' "$imgs" | awk 'NR>1 && $2=="vm-image" {print $1; exit}')
fi
# 记录选用镜像名：套件收尾清场据此删除本阶段建的画像（项键＝镜像名，见 lib 的清理段）。
printf '%s\n' "$IMG" > "$SUITE_PROFILE_IMG_FILE"

if [ -z "$IMG" ]; then
  echo "  ⊘ 镜像仓库无可用的 vm-image（阶段 3 未跑且无回退镜像），跳过镜像启动画像与实例覆盖用例（如实跳过）"
else
  echo "  · 画像键（镜像名）=$IMG"

  # ---- 8-A 镜像画像：九个叶子各设一次并提交 ----
  # 有效组合：ide 需 machine pc（q35 系列无 IDE 总线）；tcg 需 cpu mode≠host-passthrough。
  run S8 "configure
set vm-image-profiles $IMG machine pc
set vm-image-profiles $IMG cpu mode custom
set vm-image-profiles $IMG cpu model Nehalem
set vm-image-profiles $IMG firmware bios
set vm-image-profiles $IMG disk-interface ide
set vm-image-profiles $IMG boot-order cd
set vm-image-profiles $IMG accel tcg
set vm-image-profiles $IMG console telnet
set vm-image-profiles $IMG extra-args \"-global kvm-pit.lost_tick_policy=discard\"
commit"

  # 读视图：逐项值 + extra-args 的「已绕过资源账本」标注（契约 §4.4 / 用户手册 §9.6）。
  expect_out S8 "已绕过资源账本" "show vm-image-profiles"
  expect_out S8 "kvm-pit.lost_tick_policy=discard" "show vm-image-profiles"
  expect_out S8 "custom/Nehalem" "show vm-image-profiles $IMG"
  expect_out S8 "telnet" "show vm-image-profiles $IMG"

  # display set 往返：反推出等价 set 语句（cpu 平铺字段 → cpu mode / cpu model 两层）。
  expect_out S8 "set vm-image-profiles $IMG machine pc" "show configuration | display set"
  expect_out S8 "set vm-image-profiles $IMG cpu mode custom" "show configuration | display set"
  expect_out S8 "set vm-image-profiles $IMG cpu model Nehalem" "show configuration | display set"
  expect_out S8 "set vm-image-profiles $IMG firmware bios" "show configuration | display set"
  expect_out S8 "set vm-image-profiles $IMG disk-interface ide" "show configuration | display set"
  expect_out S8 "set vm-image-profiles $IMG boot-order cd" "show configuration | display set"
  expect_out S8 "set vm-image-profiles $IMG accel tcg" "show configuration | display set"
  expect_out S8 "set vm-image-profiles $IMG console telnet" "show configuration | display set"
  expect_out S8 "set vm-image-profiles $IMG extra-args" "show configuration | display set"

  # 删除形态之一：带叶子只清该叶子（清 extra-args 后再设回，读视图保持完整）。
  run S8 "configure
delete vm-image-profiles $IMG extra-args
commit"
  run S8 "configure
set vm-image-profiles $IMG extra-args \"-global kvm-pit.lost_tick_policy=discard\"
commit"

  # ---- 8-B 防呆：跨字段组合与 extra-args denylist 必须提交期拒绝 ----
  expect_fail S8 "IDE" "configure
set vm-image-profiles $IMG machine q35
set vm-image-profiles $IMG disk-interface ide
commit"
  expect_fail S8 "host-passthrough" "configure
set vm-image-profiles $IMG cpu mode host-passthrough
set vm-image-profiles $IMG accel tcg
commit"
  expect_fail S8 "不被允许" "configure
set vm-image-profiles $IMG extra-args \"-m 4096\"
commit"

  # ---- 8-C telnet 控制台端口撞车：vm29 / vm54 由 VM 名派生的端口同为 5820 ----
  # 提交期比对端口 ⇒ 必须点名两台并拒绝；**不启动**（校验在提交期，不消耗大页），故两台不会落库。
  expect_fail_all S8 "vm29" "vm54" "configure
set virtual-machine-functions vm29 image $IMG
set virtual-machine-functions vm29 vcpu count 1
set virtual-machine-functions vm29 memory size-mb 1024
set virtual-machine-functions vm29 startup console telnet
set virtual-machine-functions vm54 image $IMG
set virtual-machine-functions vm54 vcpu count 1
set virtual-machine-functions vm54 memory size-mb 1024
set virtual-machine-functions vm54 startup console telnet
commit"
fi

# ---- 8-D 实例级覆盖 + 有效画像读视图（依赖阶段 3 的 cli-vm；缺则如实跳过）----
if printf '%s\n' "$(_suite_cli 'show configuration')" | grep -qE '^virtual-machine-functions cli-vm \{'; then
  # 先只覆盖 machine：其余叶子来自镜像画像 ⇒ 读视图同时标出「实例覆盖」与「镜像画像」两个来源。
  run S8 "configure
set virtual-machine-functions cli-vm startup machine pc
commit"
  expect_out S8 "有效启动画像" "show virtual-machine-functions cli-vm detail"
  expect_out S8 "实例覆盖" "show virtual-machine-functions cli-vm detail"
  # 其余叶子来自镜像画像——仅当画像键就是 cli-vm 的镜像（正常即夹具镜像）才有「镜像画像」来源；
  # 无画像时其余叶子回落「产品默认」，此时不作此断言（不把回落当绿）。
  if [ -n "$IMG" ]; then
    expect_out S8 "镜像画像" "show virtual-machine-functions cli-vm detail"
  fi
  # 其余八个叶子各设一次（覆盖契约的 startup 各叶子语句；组合与镜像画像一致，提交期校验通过）。
  run S8 "configure
set virtual-machine-functions cli-vm startup cpu mode custom
set virtual-machine-functions cli-vm startup cpu model Nehalem
set virtual-machine-functions cli-vm startup firmware bios
set virtual-machine-functions cli-vm startup disk-interface ide
set virtual-machine-functions cli-vm startup boot-order cd
set virtual-machine-functions cli-vm startup accel tcg
set virtual-machine-functions cli-vm startup console telnet
set virtual-machine-functions cli-vm startup extra-args \"-global kvm-pit.lost_tick_policy=discard\"
commit"
  # 清实例覆盖（裸 delete＝清整块）——不改变 cli-vm 既有形态（收尾清场仍会整台删除 cli-vm）。
  run S8 "configure
delete virtual-machine-functions cli-vm startup
commit"
else
  echo "  ⊘ cli-vm 不存在（阶段 3 未跑或已被清场），跳过实例覆盖 + 有效画像读视图（如实跳过）"
fi

# ---- 8-E 裸 delete 清整节（契约 `delete vm-image-profiles <image> [<leaf>]` 的另一形态）----
# 放在末尾：此前各段（读视图 / display set / cli-vm 有效画像）都需要画像在场。
if [ -n "$IMG" ]; then
  run S8 "configure
delete vm-image-profiles $IMG
commit"
fi

summary "阶段 8"
