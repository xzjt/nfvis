package model

// FR-SEC-007 / 决策 #25、#149：敏感字段不得出现在任何回显/导出/归档里。
//
// 本文件是**脱敏的实现**（规则的唯一落点是 IsSensitiveKey，见 diff.go）。
// 原先实现只存在于 internal/api（配置视图回显），2026-09-24 下移到 model：
// 诊断归档（internal/system）也要脱敏同一条 `password_hash`，而它是 api 的**下游**
// （api 已 import system，反向 import 会成环）——两条路径必须共用同一份实现，
// 否则「同一份配置在一个出口被摘掉、在另一个出口原样交出」会重演（决策 #25 的成因）。

import (
	"encoding/json"
	"regexp"
	"strings"
)

// RedactSensitive 返回任意配置视图的**脱敏副本**（敏感字段整个移除，而非置为占位符）。
//
// 移除而非打码：客户端若把占位符回写，会写成非法哈希导致该账号无法登录；
// 字段缺失至少是可察觉的。口令的新增/修改请走专用端点
// （PUT /system/login-users/{name}、request system password change）。
func RedactSensitive(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v // 配置结构恒可序列化；兜底返回原值
	}
	var tree any
	if err := json.Unmarshal(b, &tree); err != nil {
		return v
	}
	RedactTree(tree)
	return tree
}

// RedactTree 就地递归移除敏感键（键名判定与 diff 渲染共用 IsSensitiveKey）。
func RedactTree(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if IsSensitiveKey(k) {
				delete(t, k)
				continue
			}
			RedactTree(val)
		}
	case []any:
		for _, e := range t {
			RedactTree(e)
		}
	}
}

// ---------- 文本级脱敏（round49 审计脱敏收口） ----------
//
// 上面两个函数处理**结构化**视图（按叶子键名移除）。但有些出口带的是**自由文本**：
// 容器 exec 的命令原文会进审计（可追溯），审计又经 CLI `show log audit`、
// `GET /audit-logs`、tech-support 的 `audit.json` 三个出口交给不同权限的人；而
// openapi 承诺「口令、口令哈希与证书/私钥正文一律不入审计」。命令里的口令是**文本形态**，
// 键名脱敏（RedactTree）看不见它，故需要一层文本级掩码。
//
// 口径（保守，宁少勿滥）：只在**确实存在「键 + 分隔符 + 值」**这种形状时掩码——普通命令
// （`ls -l /tmp`、`echo hello`、`mkdir -p /a/b`）逐字不变。代价如实登记：命令里若**恰好**
// 出现这些键名词（如 `grep -r password /etc/passwd`），其后一个词也会被掩码——这是
// 放宽方向的取舍（这类命令本身就在处理口令）。

var (
	// PEM 私钥块（含 RSA/EC/OPENSSH/ENCRYPTED 等种类）：整块掩码。
	scrubPEMRe = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)

	// Authorization 头的常见形态 `Bearer <token>`。
	scrubBearerRe = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=\-]+`)

	// 键 + 分隔符（`=` / `:` / 空白，两侧可带引号）+ 值。
	// 三个捕获组：1=键、2=分隔符、3=值。值的字符集排掉引号/逗号/分号，故
	// `{"token":"abc"}` 只掩掉 abc、JSON 结构不被破坏。
	scrubKeyValueRe = regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[-_]?key|apikey|authorization|private[-_ ]?key)\b` +
		`("?\s*[:=]\s*"?|\s+"?)([^\s,;'"]+)`)

	// 短选项贴值（`mysql -p123456`）；`-p` 与值之间有空格的（如 `mkdir -p /a`）不在此列。
	scrubShortPwdRe = regexp.MustCompile(`(^|\s)-p(\S+)`)

	// 工具感知的分隔形态：`sshpass -p <口令>`（round50 决策 #520）。上面的短选项正则只认
	// **贴值** `-p<值>`，而 `sshpass -p <口令>`（`-p` 与值之间是空白、值可带引号、`-p` 前可夹
	// 其它选项）是极常见形态——不掩码即明文进审计。这里**只认 sshpass**，不泛化为所有 `-p`：
	// `mkdir -p /a`、`ls -p /tmp`、`docker run -p 80:80` 因此逐字不变。
	//
	// 三个捕获组：1=工具名（保留原大小写）、2=`-p` 之前的选项前缀、3=`-p` 与值之间的空白。
	// 前缀用 `[^\n;&|]*?` 且**不跨越命令分隔符**（`; ` `&&` `|`）——避免把同一行里另一条命令的
	// `-p` 误当成本次的取值；`\s-p` 要求 `-p` 前是空白，故 `--password`、`-p80` 这类不受影响
	//（贴值形态已由短路规则掩码）。值支持 `'…'` / `"…"`（可含空格）与不带动空白的裸词。
	scrubSSHPassRe = regexp.MustCompile(`(?i)(\bsshpass\b)([^\n;&|]*?)\s+-p(\s+)(?:'[^']*'|"[^"]*"|\S+)`)
)

// ScrubSecrets 对**文本**做秘密掩码（返回新串，不改入参）。
//
// 覆盖：PEM 私钥块、`Bearer <token>`、`password|passwd|pwd|secret|token|api_key|apikey|
// authorization|private_key` 后跟 `=`/`:`/空白的值、短选项贴值的 `-p<值>`、以及工具感知的
// `sshpass -p <值>` 分隔形态（决策 #520）。
// 已脱敏文本再过一遍结果不变（幂等）——写侧与读侧可能各过一遍。
//
// 规则顺序有讲究：`sshpass -p <值>` **必须早于**短选项贴值规则——否则 `-p` 后的引号值会被
// 短选项规则截成「一截掩码 + 一截明文」（`sshpass -p 'P@ss w0rd'` 只掩到 `'P@ss`，`w0rd'`
// 漏出）。先按 sshpass 规则整值掩码，短路规则便再无分隔形态可动。
func ScrubSecrets(s string) string {
	if s == "" {
		return s
	}
	s = scrubPEMRe.ReplaceAllString(s, RedactedPlaceholder)
	s = scrubBearerRe.ReplaceAllString(s, RedactedPlaceholder)
	s = scrubKeyValueAll(s)
	s = scrubSSHPassRe.ReplaceAllString(s, "${1}${2} -p${3}"+RedactedPlaceholder)
	s = scrubShortPwdRe.ReplaceAllString(s, "${1}-p"+RedactedPlaceholder)
	return s
}

// scrubKeyValueAll 逐处替换「键+分隔符+值」中的值；值已经是占位符时原样保留（幂等）。
func scrubKeyValueAll(s string) string {
	idx := scrubKeyValueRe.FindAllStringSubmatchIndex(s, -1)
	if len(idx) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, m := range idx {
		b.WriteString(s[last:m[0]])
		if s[m[6]:m[7]] == RedactedPlaceholder {
			b.WriteString(s[m[0]:m[1]])
		} else {
			b.WriteString(s[m[2]:m[3]]) // 键
			b.WriteString(s[m[4]:m[5]]) // 分隔符
			b.WriteString(RedactedPlaceholder)
		}
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}
