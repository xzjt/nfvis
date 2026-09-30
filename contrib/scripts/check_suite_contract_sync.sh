#!/usr/bin/env bash
# 守护：真机套件的命令清单 ↔ 契约同步（决策 #319）。
#
# 由来：v2 线新增能力多次**只进实现与单测、没进真机套件的命令清单**——`show system api tokens`、
#   `set/delete system login banner` 都是这样（#301/#303）。于是 `cli-fulltest` 全绿只证明
#   「老命令没坏」，不证明「新命令能用」。本守护把**契约里声明的可执行命令**与**套件里真跑的
#   命令**对账：漏掉的必须逐条登记豁免并写明理由。
#
# 事实源与解析口径（**不手工再抄一份命令清单**）：
#   · 契约 = docs/NFViS-CLI命令全表.md 的 §1（操作模式）与 §2（配置模式）表格数据行。
#     判据行首是 `| \``；首个单元格即命令行；单元格内 `\|` 是转义竖线（先换成 \001 再按 `|` 切列），
#     ` / ` 分隔并列命令（如 `exit` / `quit` 计两条）。
#   · 套件 = contrib/scripts/cli-fulltest-phase*.sh：提取**双引号内文本**（跨行引号串按整段保留）
#     与 `<<'EOF'` heredoc 块的行，取以命令动词（show/request/set/…）起头的行。
#   · 豁免 = contrib/scripts/cli-fulltest-exemptions.tsv（`命令<TAB>类别<TAB>理由`）。
#   · 判据 = 契约命令的**字面 token 序列**必须能在某条套件命令里**按序**出现（子序列匹配）⇒
#     视为已入套件；否则必须逐条豁免。占位符 `<…>`、可选组 `[…]`、省略号、含 `|` 的备选
#     token 都不参与匹配（它们不是字面量）。
#     为什么用子序列而不是「前缀」：`set system login banner <text>` 与已被套件覆盖的
#     `set system login password-policy …` 前缀相同——前缀匹配会让新语句静默漏网。
#
# 守护本身也会错，故必须可自校准：`--selftest` 用桩夹具跑**同一批解析与判定函数**
#   （已知漏 → 报缺口；已知全 → 通过；空理由 → 报错；失效登记 → 报错；前缀相同的新语句 → 报缺口），
#   并入 make check 的 toolcheck（参照 cli-fulltest-selftest.sh 的桩式先例）。
#
# 已知边界（如实登记）：
#   · 判据是**静态文本**——「套件里有这条命令」≠「真机上一定跑通」；后者是四套件自身的责任。
#   · 命令由其他套件（semantic/lifecycle/pty）或单测覆盖时，同样要在此**显式豁免并写明理由**。
#   · 全局 CLI 选项（如 `-f`、`--yes`）不在命令全表内，本守护不覆盖；由决策 #319 手写入套件。
#
# 用法：
#   bash contrib/scripts/check_suite_contract_sync.sh              # 用仓库缺省路径对账
#   bash contrib/scripts/check_suite_contract_sync.sh --selftest   # 桩式自校准
#   bash contrib/scripts/check_suite_contract_sync.sh --contract F --suite-dir D --exempt E
# 退出码：0 = 通过；1 = 有缺口/登记问题；2 = 用法或输入缺失。
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
CONTRACT="$ROOT/docs/NFViS-CLI命令全表.md"
SUITE_DIR="$HERE"
EXEMPT="$HERE/cli-fulltest-exemptions.tsv"
SELFTEST=0

while [ "$#" -gt 0 ]; do
    case "$1" in
        --contract)  CONTRACT=${2:-}; shift 2;;
        --suite-dir) SUITE_DIR=${2:-}; shift 2;;
        --exempt)    EXEMPT=${2:-}; shift 2;;
        --selftest)  SELFTEST=1; shift;;
        -h|--help)   awk 'NR>=2 && NR<=42 {sub(/^# ?/, ""); print}' "$0"; exit 0;;
        *) echo "未知参数：$1（--help 看用法）" >&2; exit 2;;
    esac
done

# ---------- 契约命令解析 ----------
# 输出：`命令<TAB>字面 token 串`（字面 token：去掉占位符/可选组/省略号/含备选竖线的 token）。
contract_commands() {
    awk '
      function literals(cmd,   n, tk, i, t, out) {
        n = split(cmd, tk, /[ \t]+/); out = ""
        for (i = 1; i <= n; i++) {
          t = tk[i]; gsub(/[`,]/, "", t)
          if (t == "") continue
          if (t == "…") continue
          if (t ~ /<|>|\[|\]|\||"/) continue
          out = out " " t
        }
        sub(/^ /, "", out)
        return out
      }
      /^## 1\./ { ins = 1 }
      /^## 3\./ { if (ins) exit }
      !ins { next }
      /^\| `/ {
        line = $0
        gsub(/\\\|/, "\001", line)                # 转义竖线不可当列分隔符
        n = split(line, f, "|")
        if (n < 3) next
        cell = f[2]
        gsub(/`/, "", cell)
        gsub(/^[ \t]+|[ \t]+$/, "", cell)
        gsub(/\001/, "|", cell)
        m = split(cell, cmds, / \/ /)
        for (i = 1; i <= m; i++) {
          c = cmds[i]
          gsub(/^[ \t]+|[ \t]+$/, "", c)
          if (c == "" || c == "?" || c == "Tab") continue
          if (substr(c, 1, 1) == "|") continue    # 通用管道族不是命令
          printf "%s\t%s\n", c, literals(c)
        }
      }
    ' "$1"
}

# ---------- 套件命令解析 ----------
# 双引号内的文本（跨行引号串整段保留）+ heredoc 块；再按「首 token 是命令动词」筛出命令行动。
SUITE_VERBS="show request set delete configure commit clear ping traceroute monitor help start run annotate rollback load save discard edit exit quit wizard top up"
ENDRE="<<'?EOF'?[ \\t]*\$"
suite_commands() {
    awk -v VERBS="$SUITE_VERBS" -v ENDRE="$ENDRE" '
      BEGIN { inq = 0; hd = 0 }
      {
        line = $0
        sub(/^[ \t]*#.*$/, "", line)              # 整行注释
        if (hd) {
          if (line ~ /^EOF[ \t]*$/) { hd = 0; next }
          emit(line)
          next
        }
        if (!inq && line ~ ENDRE) { hd = 1; next }
        buf = ""
        n = length(line)
        for (i = 1; i <= n; i++) {
          ch = substr(line, i, 1)
          if (ch == "\\" && i < n) {              # 转义：引号内的 \" 不当收尾
            if (inq) buf = buf substr(line, i + 1, 1)
            i++
            continue
          }
          if (ch == "\"") {
            if (inq) buf = buf "\n"               # 一段引号结束 = 一行命令
            inq = !inq
            continue
          }
          if (inq) buf = buf ch
        }
        if (buf != "") emit(buf)
      }
      function emit(text,   k, j, ln, tk, first) {
        k = split(text, arr, "\n")
        for (j = 1; j <= k; j++) {
          ln = arr[j]
          gsub(/^[ \t]+|[ \t]+$/, "", ln)
          if (ln == "") continue
          split(ln, tk, /[ \t]+/)
          first = tk[1]
          if (index(" " VERBS " ", " " first " ") > 0) print ln
        }
      }
    ' "$@"
}

# ---------- 对账 ----------
run_check() {
    local ok=1
    [ -f "$CONTRACT" ] || { echo "✗ 找不到契约文件：$CONTRACT" >&2; return 2; }
    [ -d "$SUITE_DIR" ] || { echo "✗ 找不到套件目录：$SUITE_DIR" >&2; return 2; }
    [ -f "$EXEMPT" ] || { echo "✗ 找不到豁免清单：$EXEMPT" >&2; return 2; }

    local t; t=$(mktemp -d)
    local all="$t/all.tsv"
    : > "$all"
    contract_commands "$CONTRACT" | sed 's/^/C\t/' >> "$all"
    # shellcheck disable=SC2086
    suite_commands "$SUITE_DIR"/cli-fulltest-phase*.sh | sed 's/^/S\t/' >> "$all"
    # 豁免：合法行进对账；不合法行（字段不足/理由为空）单列，判失败。
    awk -F'\t' -v bad="$t/exempt_bad.txt" '
      BEGIN { print "" > bad }
      /^[ \t]*$/ { next }
      /^[ \t]*#/ { next }
      {
        c = $1; gsub(/^[ \t]+|[ \t]+$/, "", c)
        k = $2; gsub(/^[ \t]+|[ \t]+$/, "", k)
        w = $3; gsub(/^[ \t]+|[ \t]+$/, "", w)
        if (c == "" || k == "" || w == "") { print $0 > bad; next }
        printf "E\t%s\n", c
      }
    ' "$EXEMPT" >> "$all"

    awk -F'\t' -v gaps="$t/gaps.txt" -v stale="$t/stale.txt" -v red="$t/redundant.txt" \
        -v unk="$t/unjudgeable.txt" -v counts="$t/counts.txt" '
      function matches(line, lit,   nn, nt, n, lt, i, j, t) {
        nn = split(lit, nt, " ")
        if (nn == 0) return -1
        n = split(line, lt, /[ \t]+/)
        j = 1
        for (i = 1; i <= n && j <= nn; i++) {
          t = lt[i]; gsub(/[`,]/, "", t)
          if (t == nt[j]) j++
        }
        return (j > nn) ? 1 : 0
      }
      BEGIN {
        # 结果文件先建出来：awk 的 `>` 只在首次写入时建文件，
        # 零结果的桶不建会导致调用方读不到（曾因此把「无失效登记」误报成读文件失败）。
        printf "" > gaps; printf "" > stale; printf "" > red; printf "" > unk
      }
      $1 == "C" { nc++; cc[nc] = $2; cl[nc] = $3; inC[$2] = 1 }
      $1 == "S" { ns++; su[ns] = $2 }
      $1 == "E" { ne++; ex[$2] = 1; exl[ne] = $2 }
      END {
        for (i = 1; i <= nc; i++) {
          cmd = cc[i]; lit = cl[i]
          if (lit == "") { print cmd > unk; continue }
          cov = 0
          for (j = 1; j <= ns; j++) if (matches(su[j], lit) == 1) { cov = 1; break }
          if (cov) { if (ex[cmd]) print cmd > red }
          else if (!ex[cmd]) print cmd > gaps
        }
        for (i = 1; i <= ne; i++) if (!inC[exl[i]]) print exl[i] > stale
        printf "%d %d %d\n", nc, ns, ne > counts
      }
    ' "$all"

    local nc ns ne
    read -r nc ns ne < "$t/counts.txt"
    local ng nb nd nr nu
    ng=$(grep -c . "$t/gaps.txt" || true)
    nb=$(grep -c . "$t/exempt_bad.txt" || true)
    nd=$(grep -c . "$t/stale.txt" || true)
    nr=$(grep -c . "$t/redundant.txt" || true)
    nu=$(grep -c . "$t/unjudgeable.txt" || true)

    echo "契约命令 $nc 条 ｜ 套件命令 $ns 条 ｜ 豁免登记 $ne 条"
    if [ "$nb" -gt 0 ]; then
        echo "✗ 豁免清单有条目缺字段或理由为空（须为「命令<TAB>类别<TAB>理由」三列）："
        sed 's/^/    /' "$t/exempt_bad.txt"
        ok=0
    fi
    if [ "$nd" -gt 0 ]; then
        echo "✗ 豁免清单有**失效登记**（不在契约命令里；命令改名/删除后请同步清理）："
        sed 's/^/    /' "$t/stale.txt"
        ok=0
    fi
    if [ "$ng" -gt 0 ]; then
        echo "✗ 契约命令未进套件、也未登记豁免（$ng 条）——新命令请补进 cli-fulltest-phase*.sh 的相应阶段，"
        echo "   或按「破坏性 / 需交互确认 / 环境受限 / 由其他套件或单测覆盖」分类登记进："
        echo "   $EXEMPT （三列：命令<TAB>类别<TAB>理由）"
        sed 's/^/    /' "$t/gaps.txt"
        ok=0
    fi
    [ "$nu" -gt 0 ] && { echo "· 无可判定字面 token 的契约行（未计入判定）："; sed 's/^/    /' "$t/unjudgeable.txt"; }
    [ "$nr" -gt 0 ] && { echo "· 以下豁免项已被套件覆盖（登记可移除，不计失败）："; sed 's/^/    /' "$t/redundant.txt"; }
    rm -rf "$t"
    if [ "$ok" = 1 ]; then
        echo "✓ 套件命令清单与契约同步（缺口 0，登记问题 0）"
        return 0
    fi
    return 1
}

# ---------- 桩式自校准 ----------
# 用临时夹具跑**同一批解析与判定函数**：已知漏 → 必报缺口；已知全 → 必通过；
# 空理由 → 必报错；失效登记 → 必报错；前缀相同的新语句 → 必报缺口（子序列判据的反例）。
selftest() {
    local t rc=0
    t=$(mktemp -d)
    mkdir -p "$t/suite"
    cat > "$t/contract.md" <<'EOF'
## 1. 操作模式命令树
| 命令 | 说明 | 落点 | 实测 |
|---|---|---|---|
| `show version` | 版本 | `/system/version` | ✅ |
| `show system api tokens` | 活动会话 | `GET /system/api-tokens` | ✅ |
| `request system reboot` | 重启 | `POST /system:reboot` | 🚫 |
| `exit` / `quit` | 退出 | 本地 | ✅ |
## 2. 配置模式命令树
| 命令 | 说明 | 落点 | 实测 |
|---|---|---|---|
| `set system login banner <text>` | 横幅 | 配置库 | ✅ |
| `set system hostname <s>` | 主机名 | 宿主 | ✅ |
## 3. 统计
| `show` | 68 | 族 |
EOF
    cat > "$t/suite/cli-fulltest-phase1.sh" <<'EOF'
#!/usr/bin/env bash
run S1 "show version"
run S1 "show system api tokens"
EOF
    cat > "$t/exempt.tsv" <<'EOF'
# 命令	类别	理由
request system reboot	破坏性	夹具：破坏性不真执行，由非交互拒绝检查确认
set system login banner <text>	配置语句	夹具：横幅语句暂未逐条执行
EOF
    echo "— 桩式自校准（check_suite_contract_sync.sh --selftest）—"

    # ① 已知漏：set system hostname / exit / quit 未入套件也未豁免 → 必须报缺口。
    local out c
    out=$(bash "$0" --contract "$t/contract.md" --suite-dir "$t/suite" --exempt "$t/exempt.tsv" 2>&1); c=$?
    if [ "$c" -ne 0 ] && printf '%s' "$out" | grep -q 'set system hostname' \
       && printf '%s' "$out" | grep -qE '^    (exit|quit)$'; then
        echo "  ✓ 已知漏（set system hostname / exit / quit）→ 报缺口、退出码非零"
    else
        echo "  ✗ 已知漏未报缺口（退出码 $c）"; printf '%s\n' "$out" | head -5 | sed 's/^/      | /'; rc=1
    fi
    if printf '%s' "$out" | grep -q 'show system api tokens'; then
        echo "  ✗ 已入套件的命令被误报为缺口（假红）"; rc=1
    else
        echo "  ✓ 已入套件的命令未误报"
    fi

    # ② 已知全：补齐套件与豁免后必须通过。
    cat >> "$t/suite/cli-fulltest-phase1.sh" <<'EOF'
run S1 "set system hostname fixture"
run S1 "exit"
run S1 "quit"
EOF
    cat > "$t/exempt2.tsv" <<'EOF'
request system reboot	破坏性	夹具：破坏性不真执行，由非交互拒绝检查确认
set system login banner <text>	配置语句	夹具：横幅语句暂未逐条执行
EOF
    out=$(bash "$0" --contract "$t/contract.md" --suite-dir "$t/suite" --exempt "$t/exempt2.tsv" 2>&1); c=$?
    if [ "$c" -eq 0 ]; then
        echo "  ✓ 已知全 → 通过（退出码 0）"
    else
        echo "  ✗ 已知全却未通过："; printf '%s\n' "$out" | sed 's/^/      | /'; rc=1
    fi

    # ③ 空理由：豁免条目缺第三列 → 必须报错。
    cat > "$t/exempt-empty.tsv" <<'EOF'
request system reboot	破坏性	
set system login banner <text>	配置语句	理由齐全
EOF
    out=$(bash "$0" --contract "$t/contract.md" --suite-dir "$t/suite" --exempt "$t/exempt-empty.tsv" 2>&1); c=$?
    if [ "$c" -ne 0 ] && printf '%s' "$out" | grep -q '理由为空'; then
        echo "  ✓ 豁免理由为空 → 报错"
    else
        echo "  ✗ 空理由未被判错（退出码 $c）"; rc=1
    fi

    # ④ 失效登记：豁免了契约里没有的命令 → 必须报错。
    cat > "$t/exempt-stale.tsv" <<'EOF'
request system reboot	破坏性	理由齐全
set system login banner <text>	配置语句	理由齐全
request system no-such-command	破坏性	理由齐全
EOF
    out=$(bash "$0" --contract "$t/contract.md" --suite-dir "$t/suite" --exempt "$t/exempt-stale.tsv" 2>&1); c=$?
    if [ "$c" -ne 0 ] && printf '%s' "$out" | grep -q '失效登记'; then
        echo "  ✓ 失效登记（不在契约里）→ 报错"
    else
        echo "  ✗ 失效登记未被判错（退出码 $c）"; rc=1
    fi

    # ⑤ 反例：前缀相同而多一个词的新语句必须被识别为缺口
    #    （`set system login banner <text>` 不得被 `set system login password-policy …` 掩盖）。
    cat > "$t/suite/cli-fulltest-phase2.sh" <<'EOF'
#!/usr/bin/env bash
run S2 "set system login password-policy min-length 12"
EOF
    cat > "$t/exempt-min.tsv" <<'EOF'
request system reboot	破坏性	理由齐全
set system hostname <s>	配置语句	理由齐全
exit	本地模式	理由齐全
quit	本地模式	理由齐全
EOF
    out=$(bash "$0" --contract "$t/contract.md" --suite-dir "$t/suite" --exempt "$t/exempt-min.tsv" 2>&1); c=$?
    if [ "$c" -ne 0 ] && printf '%s' "$out" | grep -q 'set system login banner'; then
        echo "  ✓ 前缀相同的新语句仍被判为缺口（子序列判据有效）"
    else
        echo "  ✗ 前缀相同的新语句漏网（退出码 $c）"; rc=1
    fi

    # ⑥ 冗余登记只提示、不判失败（豁免项已入套件）。
    cat > "$t/exempt-red.tsv" <<'EOF'
request system reboot	破坏性	理由齐全
set system login banner <text>	配置语句	理由齐全
exit	本地模式	理由齐全
quit	本地模式	理由齐全
show version	只读	夹具：已被套件覆盖的豁免
EOF
    out=$(bash "$0" --contract "$t/contract.md" --suite-dir "$t/suite" --exempt "$t/exempt-red.tsv" 2>&1); c=$?
    if [ "$c" -eq 0 ] && printf '%s' "$out" | grep -q '可移除'; then
        echo "  ✓ 冗余登记 → 提示且不判失败"
    else
        echo "  ✗ 冗余登记处理不符预期（退出码 $c）"; rc=1
    fi

    rm -rf "$t"
    if [ "$rc" -eq 0 ]; then echo "全部符合预期"; else echo "有不符合预期的用例"; fi
    return "$rc"
}

if [ "$SELFTEST" = 1 ]; then
    selftest
    exit $?
fi
run_check
exit $?
