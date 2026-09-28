#!/usr/bin/env bash
# 阶段 6：事务语义（契约 §2.1）。不手写 discard（CLI 脚本模式会自动收尾，重复 discard 会报 spurious %%）。
set -u
source "$(cd "$(dirname "$0")" && pwd)/cli-fulltest-lib.sh"

# 唯一后缀/取值：本阶段含「设置同一字段」。自 v1.1.48 起「值未变化」是提示而非失败
# （决策 #190），但仍用不同取值，让断言检验的是「值真的改到了」而不是空操作。
SFX=$(date +%s)
IDLE=$((10 + SFX % 10))

echo "############ 阶段 6：事务语义 ############"
{ echo "############ 阶段 6：事务语义 ############"; } >> "$LOG"

# commit check（不落库）
run S6 "configure
set system hostname cli-tx-check-$SFX
commit check"

# annotate（全路径）
run S6 "configure
annotate system hostname CLI事务注释
commit"

# save：candidate 导出 JSON
run S6 "configure
save /tmp/cli-candidate.json"

# show candidate（会话内）
run S6 "configure
set system hostname cli-tx-cand-$SFX
show
commit"

# rollback：取历史快照为 candidate
run S6 "configure
rollback 1
show
commit"

# show configuration compare rollback n
run S6 "show configuration compare rollback 1"

# run：配置模式内执行操作命令
run S6 "configure
run show version"

# commit and-quit
run S6 "configure
set system hostname cli-tx-andquit-$SFX
commit and-quit"

# load merge：JSON 导入 candidate
run S6 "configure
load merge /tmp/cli-candidate.json
show
commit"

# commit confirmed（短窗口，随后确认）
run S6 "configure
set system idle-timeout-minutes $IDLE
commit confirmed 5"
run S6 "configure
commit"

summary "阶段 6"
