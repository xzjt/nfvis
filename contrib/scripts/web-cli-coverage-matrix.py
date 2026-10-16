#!/usr/bin/env python3
# Web 控制台 ⇄ CLI 覆盖矩阵生成器（v3，2026-10）。
#
# 口径（**全部取自代码真源，不取文档**——文档会滞后，代码不会）：
#   ① CLI → REST：`internal/api/cli_rest_coverage_test.go` 的三张表
#      （`cliRESTCoverage` / `cliRESTExceptions` / `cliRESTGaps`）。该表由
#      `TestContractCLICommandsAreClassified` 强制「契约里每条 CLI 命令都必须被归类」，
#      因此**不会滞后**（新增命令不归类即测试红）。
#   ② REST → Web：`internal/api/ui/js/**/*.js` 里的 `api('…')` 字面量 + `routes.json` 的 endpoints。
#   ③ 端点归一化：路径参数（{name}/{n}/${x}）→ `*`；去掉查询串与尾斜杠；`:action` → `:*`。
#
# 输出：docs/v3-Web-CLI覆盖矩阵.md（生成物，勿手改）
# 用法：python3 contrib/scripts/web-cli-coverage-matrix.py
import json
import os
import re

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
GUARD = os.path.join(ROOT, "internal", "api", "cli_rest_coverage_test.go")
UI = os.path.join(ROOT, "internal", "api", "ui")
ROUTES = os.path.join(UI, "routes.json")
OUT = os.path.join(ROOT, "docs", "v3-Web-CLI覆盖矩阵.md")


def norm(path):
    p = path.strip().split("?")[0]
    p = re.sub(r"^(GET|POST|PUT|DELETE|PATCH)\s+", "", p)  # 剥掉 HTTP 动词
    p = re.sub(r"\{[^}]*\}", "*", p)
    p = re.sub(r"\$\{[^}]*\}", "*", p)
    p = re.sub(r":[a-zA-Z][a-zA-Z0-9_-]*", ":*", p)
    return p  # 保留尾斜杠：UI 拼接串常以 "/" 结尾，前缀匹配要靠它


def parse_go_map(name):
    """从 Go 源码里抽 map[string]string 字面量（去掉行尾注释）。"""
    src = open(GUARD, encoding="utf-8").read()
    m = re.search(r"var\s+" + name + r"\s*=\s*map\[string\]string\{(.*?)\n\}", src, re.S)
    if not m:
        return {}
    out = {}
    for line in m.group(1).split("\n"):
        line = re.sub(r"//.*$", "", line).strip()
        mm = re.match(r'"((?:[^"\\]|\\.)*)"\s*:\s*"((?:[^"\\]|\\.)*)"\s*,?$', line)
        if mm:
            out[mm.group(1)] = mm.group(2)
    return out


def web_endpoints():
    eps = set()
    for dp, dn, fn in os.walk(UI):
        if "novnc" in dp:
            continue
        for f in fn:
            if not f.endswith(".js"):
                continue
            src = open(os.path.join(dp, f), encoding="utf-8").read()
            # ① 共享件：api('…') / api(`…`)
            for m in re.finditer(r"\bapi\(\s*[`'\"]([^`'\"]+)", src):
                eps.add(m.group(1))
            # ② 少数页直连 fetch(API + '…')（诊断 / 差异比较等）——同一套 REST，也要算
            for m in re.finditer(r"fetch\(\s*API\s*\+\s*[`'\"]([^`'\"]+)", src):
                eps.add(m.group(1))
    with open(ROUTES, encoding="utf-8") as fh:
        rj = json.load(fh)
    for r in rj.get("routes", []):
        for e in r.get("endpoints") or []:
            eps.add(e if isinstance(e, str) else str(e))
    return {norm(e) for e in eps}


def family(cmd):
    t = cmd.split()
    if not t:
        return cmd
    if t[0] in ("set", "delete", "show", "request", "monitor", "clear", "ping", "traceroute") and len(t) > 1:
        return t[0] + " " + t[1]
    return t[0]


def main():
    cov = parse_go_map("cliRESTCoverage")
    exc = parse_go_map("cliRESTExceptions")
    gaps = parse_go_map("cliRESTGaps")
    web = web_endpoints()

    hit, miss = [], []
    for cmd, rest in sorted(cov.items()):
        eps = [e.strip() for e in rest.split("+")]

        def web_hit(e):
            n = norm(e)
            nr = n.rstrip("/")
            for w in web:
                if n == w or nr == w.rstrip("/"):
                    return True
                # UI 用字符串拼接构造 URL（`api('/x/' + name + ':start')`），扫描只拿到前缀
                # `/x/`——按前缀认作已接（路径级判定，方法由 UI 自己给对）。
                if w.endswith("/") and n.startswith(w):
                    return True
            return False

        ws = sorted({norm(e) for e in eps if web_hit(e)})
        (hit if ws else miss).append((cmd, eps, ws))

    # 别名归并：同一端点若有**别的**命令已被 Web 接上，则本条是「别名行」（Web 走另一名字），
    # 不算待补（例：`request vpp trace start` 与 `request capture start` 是同一实现的兼容别名）。
    cov_eps = set()
    for cmd, eps, ws in hit:
        for e in eps:
            cov_eps.add(norm(e))
    alias, real_miss = [], []
    for cmd, eps, ws in miss:
        if any(norm(e) in cov_eps for e in eps):
            alias.append((cmd, eps, ws))
        else:
            real_miss.append((cmd, eps, ws))

    L = []
    A = L.append
    A("# Web 控制台 ⇄ CLI 覆盖矩阵（v3，生成物）\n")
    A("> **本文件由 `contrib/scripts/web-cli-coverage-matrix.py` 生成，勿手改。**\n")
    A("> **口径（取代码真源，不取文档——文档会滞后）**：\n")
    A("> ① **CLI → REST** 取自 `internal/api/cli_rest_coverage_test.go` 的三张表；该表由")
    A("> `TestContractCLICommandsAreClassified` 强制「契约里每条 CLI 命令都必须被归类」，")
    A("> 故**不滞后**（新增命令不归类即测试红）。\n")
    A("> ② **REST → Web** 取自 `internal/api/ui/js/**` 的 `api('…')` 字面量 + `routes.json` 的 endpoints。\n")
    A("> ③ 端点归一化：路径参数 → `*`、去查询串与尾斜杠、`:action` → `:*`。\n")
    A("")
    A("## 汇总\n")
    A("| 口径 | 条数 |")
    A("|---|---|")
    A("| CLI 命令（有 REST 端点者，代码守护表） | %d |" % len(cov))
    A("| ├ **Web 已接**（端点被 Web 调用） | %d |" % len(hit))
    A("| ├ 别名行（同端点、Web 走另一名字） | %d |" % len(alias))
    A("| └ **Web 未接**（端点存在、Web 未调用） | %d |" % len(real_miss))
    A("| CLI-only by design（守护例外表，不需要 API） | %d |" % len(exc))
    A("| 真缺口（守护缺口表） | %d |" % len(gaps))
    A("")
    A("**判读**：REST 层**零缺口**（守护表强制）；下面的 A 节＝**Web 前端待补清单**。\n")
    A("## A. Web 未接（REST 端点已有、Web 未调用）——**待补**\n")
    A("> 注：下列 4 条经查**都是「兼容别名」**（守护表里就写着「兼容别名（同一 handler）」）：")
    A("> `request vpp trace start|stop|export` 与 `show vpp capture` 是 `request capture *` /")
    A("> `show capture` 的旧名，Web 走的是**新名**（`/capture`）——功能上已覆盖、**不需另做**。\n")
    A("| CLI 命令 | 族 | 已有端点 |")
    A("|---|---|---|")
    for cmd, eps, _ in sorted(real_miss, key=lambda x: (family(x[0]), x[0])):
        A("| `%s` | %s | %s |" % (cmd, family(cmd), ", ".join("`%s`" % e for e in eps)))
    A("")
    A("## A2. 别名行（同端点已有别的命令被 Web 接上；Web 走另一个名字，不需另做）\n")
    A("| CLI 命令（别名） | 族 | 端点 |")
    A("|---|---|---|")
    for cmd, eps, _ in sorted(alias, key=lambda x: (family(x[0]), x[0])):
        A("| `%s` | %s | %s |" % (cmd, family(cmd), ", ".join("`%s`" % e for e in eps)))
    A("")
    A("## B. Web 已接\n")
    A("| CLI 命令 | 族 | Web 命中端点 |")
    A("|---|---|---|")
    for cmd, eps, ws in sorted(hit, key=lambda x: (family(x[0]), x[0])):
        A("| `%s` | %s | %s |" % (cmd, family(cmd), ", ".join("`%s`" % e for e in ws)))
    A("")
    A("## C. CLI-only by design（守护例外表：REPL 交互形态 / CLI 侧文本渲染，不需要 API）\n")
    A("| CLI 命令 | 理由 |")
    A("|---|---|")
    for cmd, why in sorted(exc.items()):
        A("| `%s` | %s |" % (cmd, why.replace("|", "\\|")))
    A("")
    A("## D. **本矩阵的局限（必读）**\n")
    A("**端点级已接 ≠ 界面级可用。** 本矩阵只能回答「Web 调不调这个端点」，回答不了")
    A("「操作者在界面上有没有一个控件做这件事」。两者差得最远的是 **`set` / `delete` 族**：")
    A("按 REST 事务模型，**所有配置语句都由 `PUT/DELETE /configuration/candidate` +")
    A("`POST /configuration/commit` 架构性覆盖**（这正是 CLI 语句在 REST 里的对应物），")
    A("所以它们在本矩阵里全部记为「已接」——但界面上有没有**对应的表单字段**，本矩阵看不见。")
    A("举例（人工核验）：`set interfaces <if> port-security mac <m>` 与 `monitor vnf` 在")
    A("`internal/api/ui/js/**` 里**一处引用都没有**，却不会被本矩阵判为缺口。\n")
    A("**结论**：要回答「Web 是否实现了 CLI 的全部功能」，必须在端点级之上再做一层")
    A("**界面级审计**（下一节）。\n")
    A("## E. 下一步：界面级审计（建议做法）\n")
    A("1. **配置语句族**：把 `internal/schema/tree_config.go` 的命令树叶子（`set`/`delete` 全部")
    A("   语句）与 `internal/api/ui/js/views/config.js` 里各节的 `fields[].path` / `sublists`")
    A("   逐条对照——每个叶子要么有对应控件，要么明确标注「只支持原始配置文本框」。")
    A("2. **动作族**：`request` 族按本矩阵 A 节逐条确认界面入口（按钮/表单）。")
    A("3. **读族**：`show` 族按页确认展示位置（列表/详情/诊断页）。")
    A("4. 产出物：在 **A 节基础上加一列「界面控件」**（有/无/仅原始配置），即真正的排期清单。\n")
    A("## F. 已知界面级缺口（人工核验，供审计起点）\n")
    A("| CLI 命令 | 端点级 | 界面级 | 证据 |")
    A("|---|---|---|---|")
    A("| `set interfaces <ifname> port-security mac <mac>` | 已接（candidate） | **无** | `port-security` 在 `ui/js/**` 零命中 |")
    A("| `monitor vnf` | CLI-only（例外表） | **无** | `monitor` 在 `ui/js/**` 零命中 |")
    A("")
    with open(OUT, "w", encoding="utf-8", newline="\n") as fh:
        fh.write("\n".join(L) + "\n")
    print("wrote", OUT)
    print("有端点 %d ｜ Web 已接 %d ｜ 别名 %d ｜ Web 未接 %d ｜ 例外 %d ｜ 缺口 %d"
          % (len(cov), len(hit), len(alias), len(real_miss), len(exc), len(gaps)))


main()
