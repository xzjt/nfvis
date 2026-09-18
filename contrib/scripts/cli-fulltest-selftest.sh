#!/usr/bin/env bash
# 发现 #9 自校准：判定模式必须「已知正确 → PASS、已知错误 → FAIL」，且不被历史回显误伤。
# 用法：bash contrib/scripts/cli-fulltest-selftest.sh   （在仓库根或任意目录均可）
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
# 取出 lib 里真正在用的模式，避免测试里另写一份（否则测的不是实现）
PAT=$(grep -oE "grep -qE '[^']+'" "$HERE/cli-fulltest-lib.sh" | head -1 | sed "s/grep -qE '//; s/'$//")
[ -z "$PAT" ] && { echo "✗ 取不到判定模式"; exit 1; }
echo "判定模式：$PAT"

fails() { printf '%s\n' "$1" | grep -qE "$PAT"; }
check() { # check <期望 pass|fail> <说明> <输出>
  local want=$1 desc=$2 out=$3 got=pass
  fails "$out" && got=fail
  if [ "$got" = "$want" ]; then
    printf '  ✓ %-46s → %s\n' "$desc" "$got"
  else
    printf '  ✗ %-46s → 期望 %s 实际 %s\n' "$desc" "$want" "$got"
    RC=1
  fi
}
RC=0

echo "— 应判失败（真实错误）—"
check fail "脚本模式 %% 错误" '%% 底座下发失败（已补偿）: 下发失败 interface[ens224]'
check fail "配置模式单 % 错误" '% 无效命令: show system version'
check fail "校验失败在行首（未提交）" '校验失败（未提交）:
  vpp.dpdk.per-dev[ens224]: "ens224" 未在 interfaces 中声明'
check fail "空输出里的裸 %" '%'

echo "— 应判通过（含历史回显）—"
check pass "审计历史里含「校验失败」（发现 #9 的假红来源）" 'ts         user   action          detail                 result
2026-09-18 admin  config.commit   校验失败: vpp.dpdk...  failure
2026-09-18 admin  config.commit   重签自签证书            success'
check pass "普通成功输出" 'uptime         1h2m（0d 1h 2m）'
check pass "审计里含 100% packet loss 字样" 'detail: ping 2 sent, 0 received, 100% packet loss'
check pass "正文里引用错误文案（非行首）" '提示：若报「拒绝操作管理口」，请改用业务口'
check fail "同一文案出现在行首（真错误）" '%% 拒绝操作管理口：ens160（配置中声明的管理口）'

if [ $RC -eq 0 ]; then echo "全部符合预期"; else echo "有不符合预期的用例"; fi
exit $RC
