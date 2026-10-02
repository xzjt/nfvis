package api

// 决策 #325（收口 v2 待做 二.4）：**秘密出口全量复查**的分类守护。
//
// 决策 #149 只脱敏了诊断归档；同类「秘密出口」（配置读视图、配置导出、日志、审计、REST、
// Web、抓包/core dump 导出等）此前未系统扫过。本文件把「出口 × 敏感项」矩阵**做成可执行
// 的守护**：每个出口要么断言**已脱敏**（不含任何哨兵秘密），要么进**例外清单**并写明理由
// （为什么允许这条出口带出秘密）；两者都不是即测试失败。新增出口（GET 路由）若未分类，
// TestEverySecretEgressRouteClassified 会失败。
//
// 本轮由此抓出并修掉的两条**真泄漏**（修前本文件会红）：
//  1. CLI `show configuration | display json|xml` —— 结构化快照取自**原始配置树**
//     （含 `password_hash`），只读账号即可读到全部用户口令哈希。修：渲染前经
//     model.RedactSensitive（pipes.go）。
//  2. CLI `show log system` 与 REST `GET /system/logs` —— nfvisd 日志原文里含首启引导
//     打印的**一次性口令**（super-user 级明文），而 #149 只对诊断归档剥掉了它。修：装配期
//     包裹日志来源（server.go 的 scrubLogSource → internal/system.ScrubBootstrapCredential）。
//
// 已知的**有意例外**（每条写明理由）见 secretEgressExceptions。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/system"
)

// 哨兵秘密：值由本测试给定，故可精确断言「这一串没有出现在出口里」。
const (
	egressHashSentinel = "pbkdf2$sha256$600000$EGRESSSALT$EGRESSHASH"
	egressBootSentinel = "Egress-Boot@5555"
	egressUserDataSent = "cloud-init 里塞的私钥 EGRESS-USERDATA-SECRET"
)

// egressSecretProbe 一个出口的探测与分类。
type egressSecretProbe struct {
	name     string
	expected string // "redacted" | "exception"
	reason   string // 例外理由（expected=="exception" 时必填）
	// carriesMarker 非空时断言：该出口**确应**带出此标记（证明例外不是过期条目）。
	carriesMarker string
	probe         func(t *testing.T) string
}

// TestSecretEgressMatrixClassified 逐条探测出口，断言「已脱敏」或「在例外清单里有理由」。
func TestSecretEgressMatrixClassified(t *testing.T) {
	// —— REST 侧：一台提交了哨兵哈希 + 哨兵 user_data 的服务器 ——
	// LogSource 注入含一次性口令的日志来源（GET /system/logs 的脱敏由此核实）。
	bootLogs := func() ([]byte, error) {
		return []byte("%% 首次启动已创建用户 admin (super-user)。一次性口令（仅显示一次，请立即修改）: " + egressBootSentinel + "\n普通日志行\n"), nil
	}
	ts := newTestServerOpts(t, Options{LogSource: bootLogs})
	token := loginAdmin(t, ts)
	rootCfg := withSuperUser(model.Config{
		System: &model.SystemConfig{Login: &model.SystemLogin{Users: []model.LoginUserConfig{
			{Name: "opssec", Class: "operator", PasswordHash: egressHashSentinel},
		}}},
		VirtualMachineFunctions: []model.VMFunction{{
			Name: "sec-vm", Image: "base.qcow2",
			VCPU: model.VMCpu{Count: 1}, Memory: model.VMMemory{SizeMB: 512, HugepageSize: "1G"},
			CloudInit: &model.CloudInit{UserData: "#cloud-config\nx: " + egressUserDataSent},
		}},
		ResourcePools: &model.ResourcePool{
			Hugepages: []model.HPool{{PageSize: "1G", Count: 4}},
			CPU:       &model.CPUSetup{IsolatedCores: []int{4, 5, 6, 7}},
		},
	})
	if status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		rootCfg, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("提交哨兵配置: %d %s", status, data)
	}
	if status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		rootCfg, nil); status != http.StatusOK {
		t.Fatalf("写候选（编辑态）: %d %s", status, data)
	}

	// —— CLI 侧：种子哈希 + 注入含一次性口令的日志来源 ——
	x, eng := newCLIKit(t)
	seedCLIUserWithHash(t, x, eng)
	x.setLogSource(bootLogs)
	cli := func(line string) string { return x.Execute("admin", aaa.ClassSuperUser, "ssh", line).Output }
	rest := func(t *testing.T, path string) string {
		status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+path, token, nil, nil)
		if status != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, status, data)
		}
		return string(data)
	}
	// restAny 容忍非 200（用于分类核实：占位路径本就该 404，但要看到响应体）。
	restAny := func(t *testing.T, path string) string {
		_, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+path, token, nil, nil)
		return string(data)
	}

	// —— 配置 save 导出（例外：super-user 专属、0600、用途是导出后可 load 回灌）——
	savePath := filepath.Join(t.TempDir(), "cfg.json")
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", "configure", "save "+savePath, "discard")
	saveBody, _ := os.ReadFile(savePath)

	matrix := []egressSecretProbe{
		{name: "cli show configuration", expected: "redacted", probe: func(t *testing.T) string { return cli("show configuration") }},
		{name: "cli show configuration | display json", expected: "redacted", probe: func(t *testing.T) string { return cli("show configuration | display json") }},
		{name: "cli show configuration | display xml", expected: "redacted", probe: func(t *testing.T) string { return cli("show configuration | display xml") }},
		{name: "cli show configuration | display set", expected: "redacted", probe: func(t *testing.T) string { return cli("show configuration | display set") }},
		{name: "cli show log system", expected: "redacted", probe: func(t *testing.T) string { return cli("show log system") }},
		{name: "cli show log audit", expected: "redacted", probe: func(t *testing.T) string { return cli("show log audit last 20") }},
		{name: "GET /configuration", expected: "redacted", probe: func(t *testing.T) string { return rest(t, "/configuration") }},
		{name: "GET /configuration/candidate", expected: "redacted", probe: func(t *testing.T) string { return rest(t, "/configuration/candidate") }},
		{name: "GET /configuration/diff", expected: "redacted", probe: func(t *testing.T) string { return rest(t, "/configuration/diff") }},
		{name: "GET /system", expected: "redacted", probe: func(t *testing.T) string { return rest(t, "/system") }},
		{name: "GET /system/login-users", expected: "redacted", probe: func(t *testing.T) string { return rest(t, "/system/login-users") }},
		{name: "GET /audit-logs", expected: "redacted", probe: func(t *testing.T) string { return rest(t, "/audit-logs") }},
		{name: "GET /system/logs", expected: "redacted", probe: func(t *testing.T) string { return rest(t, "/system/logs") }},
		{name: "GET /virtual-machine-functions", expected: "redacted", probe: func(t *testing.T) string { return rest(t, "/virtual-machine-functions") }},
		{
			name: "cli save <file>（candidate 导出）", expected: "exception", carriesMarker: "CLISALT",
			reason: "super-user 专属（configure 模式，ClassSuperUser）、0600 落盘；用途就是**导出后可 load 回灌**" +
				"（脱敏则回灌会毁掉账号——与决策 #143 的配置备份下载同一口径）。非 super-user 无法触达。",
			probe: func(t *testing.T) string { return string(saveBody) },
		},
		{
			name: "配置 user_data（cloud-init 自由文本）", expected: "exception", carriesMarker: egressUserDataSent,
			reason: "决策 #149 已如实登记的残留：user_data 是自由文本，产品无法判定其中是否含秘密，与配置视图同口径**不脱敏**。" +
				"操作者应改用 ssh_keys 公钥、并在 VM 之外注入凭据。",
			probe: func(t *testing.T) string { return rest(t, "/configuration") },
		},
		{
			name: "技术诊断归档 tech-support（config.json + logs.txt）", expected: "redacted",
			probe: func(t *testing.T) string { return techSupportArchive(t) },
		},
		{
			name: "GET /system/backup/{file}（配置备份下载）", expected: "exception",
			reason: "决策 #143 的既有例外：备份要能恢复，脱敏即无法恢复；端点 ClassSuperUser、归档 0600。" +
				"本测试用占位文件名无法实际取到归档正文，故不作 carries 断言（内容由 internal/system 与 backup 测试覆盖）。",
			probe: func(t *testing.T) string { return restAny(t, "/system/backup/nosuch.json") },
		},
		{
			name: "GET /vpp/capture/{file}（抓包导出）", expected: "exception",
			reason: "抓包是**数据面明文载荷**的按需导出（操作者自己选的时窗与接口），其中可能含未加密业务凭据；" +
				"这是抓包本身的用途，产品脱敏会破坏其证据价值。权限 show vpp capture（read-only），导出件在受管目录。",
			probe: func(t *testing.T) string { return "" }, // 无抓包运行时，占位（分类由路由守护保证）
		},
		{
			name: "GET /container-functions/{name}/logs", expected: "exception",
			reason: "容器 stdout/stderr 原文（request container-functions <n> log）——**guest 内容**，产品不往里写凭据；" +
				"过滤 guest 输出会破坏日志用途（同 #149 对日志分节的取舍）。",
			probe: func(t *testing.T) string { return "" },
		},
	}

	for _, p := range matrix {
		p := p
		t.Run(p.name, func(t *testing.T) {
			body := p.probe(t)
			if p.expected != "redacted" && p.expected != "exception" {
				t.Fatalf("expected 只能是 redacted|exception，实得 %q", p.expected)
			}
			// 「凭据」判据只取**产品自己的秘密**（口令哈希、一次性口令）与 hash 键名；
			// user_data 是**已知例外**（自由文本，见决策 #149 残留），单列一行核实，不进此判据。
			carriesCredential := strings.Contains(body, egressHashSentinel) ||
				strings.Contains(body, "password_hash") || strings.Contains(body, egressBootSentinel)
			switch p.expected {
			case "redacted":
				if carriesCredential {
					t.Fatalf("出口本应已脱敏，却含凭据哨兵：\n%s", body)
				}
			case "exception":
				if p.reason == "" {
					t.Fatal("例外清单条目必须写明理由")
				}
			}
			// 声称「应带出秘密」的例外：核实它**确实**带出（证明例外不是过期条目）。
			if p.carriesMarker != "" && !strings.Contains(body, p.carriesMarker) {
				t.Fatalf("例外条目声称会带出 %q，实际未带出——例外可能已过期，请复核分类：\n%s", p.carriesMarker, body)
			}
		})
	}
}

// techSupportArchive 生成一份诊断归档并返回其中**全部可读文本分节**的拼接——
// 逐成员核实「归档这个出口」是否带出哨兵哈希/一次性口令（决策 #149/#325）。
func techSupportArchive(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	sentCfg := model.Config{System: &model.SystemConfig{Login: &model.SystemLogin{Users: []model.LoginUserConfig{
		{Name: "admin", Class: "super-user", PasswordHash: egressHashSentinel},
	}}}}
	tsu := system.NewTechSupport(dir, system.TechSupportSources{
		Config: func() (any, error) { return sentCfg, nil },
		Logs: func() ([]byte, error) {
			return []byte("%% 首次启动已创建用户 admin (super-user)。一次性口令（仅显示一次，请立即修改）: " + egressBootSentinel + "\n"), nil
		},
	}, "test")
	f, err := tsu.Generate()
	if err != nil {
		t.Fatalf("生成诊断归档: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, f.File))
	if err != nil {
		t.Fatalf("读归档: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("解 gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	var b strings.Builder
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("读 tar: %v", err)
		}
		body, _ := io.ReadAll(tr)
		b.WriteString("== " + h.Name + " ==\n")
		b.Write(body)
	}
	// 脱敏 ≠ 掏空：结构仍在、标记行仍在（换成占位符），否则「没泄露」只是「什么都没回」。
	out := b.String()
	if !strings.Contains(out, "«已隐藏»") {
		t.Fatalf("诊断归档应保留脱敏占位符（脱敏≠掏空）：\n%s", out)
	}
	return out
}

// secretEgressRoutes 逐条 GET 路由的分类：key = 路径，value = 分类说明。
//
// 这是一份**穷尽**清单：TestEverySecretEgressRouteClassified 断言 server.go 注册的每条
// GET 路由都在这里——新增出口若未分类，测试即失败（逼作者想清楚「它会不会带出秘密」）。
// 分类值只需说明清楚，不做机器判定；判定由上面的矩阵与既有 TestSecretsNeverEchoed 兜。
var secretEgressRoutes = map[string]string{
	"/acls":                             "配置段视图（model→JSON，无秘密字段）",
	"/acls/{name}":                      "同上",
	"/alarms":                           "告警元数据（消息/来源），产品不写凭据",
	"/audit-logs":                       "审计：diff 经 model 渲染层脱敏（口令/令牌掩码）",
	"/bonds":                            "配置段视图",
	"/bonds/{name}":                     "同上",
	"/cli/candidates":                   "候选名清单（只回名字）",
	"/configuration":                    "整配置视图：redactConfigView 脱敏（#325 矩阵核实）",
	"/configuration/candidate":          "候选视图：同上",
	"/configuration/diff":               "diff 文本：model.Diff 掩码敏感叶子",
	"/configuration/history":            "提交历史：**有意**只回元数据、不含配置正文",
	"/configuration/permissions":        "生效权限视图（路径与判定），无配置正文/秘密",
	"/dns/proxy":                        "数据面 DNS 代理配置声明（上游地址/交换机名），无秘密字段",
	"/container-functions":              "资源列表（配置视图），无秘密字段",
	"/container-functions/{name}":       "同上",
	"/container-functions/{name}/logs":  "容器日志原文（guest 内容，例外——见矩阵）",
	"/events":                           "SSE：事件元数据（revision/user），不含配置正文",
	"/images":                           "镜像清单元数据",
	"/images/{name}":                    "同上",
	"/interfaces":                       "接口视图 + 运行态，无秘密字段",
	"/interfaces/{name}":                "同上（含 statistics）",
	"/metrics":                          "Prometheus 宿主指标（无鉴权但只发运行态计数/容量）",
	"/nat":                              "NAT 配置段视图（无秘密字段）",
	"/nat/sessions":                     "NAT 会话运行态（五元组/计数）",
	"/openapi.json":                     "规范文本（无秘密）",
	"/port-mirroring":                   "配置段视图",
	"/protocols/lldp":                   "配置段视图",
	"/protocols/lldp/neighbors":         "LLDP 邻居运行态",
	"/qos/policies":                     "配置段视图",
	"/resource-pools":                   "资源池配置/运行态，无秘密字段",
	"/system":                           "system 配置段视图：脱敏（#325 矩阵核实）",
	"/system/api-tokens":                "活动会话清单：只回 token_id（随机 UUID，非凭据本体）+ user/class/时间",
	"/system/backup":                    "备份归档**清单**（文件元数据），不含正文",
	"/system/backup/{file}":             "备份归档下载（例外：#143，super-user + 0600，恢复所必需）",
	"/system/configuration/sessions":    "持锁会话列表（标识/用户/时间），无配置正文",
	"/system/core-dumps":                "core dump 清单（文件名/大小/时间），不含转储正文",
	"/system/hardware":                  "硬件健康运行态",
	"/system/health/thresholds":         "阈值配置段视图",
	"/system/kernel":                    "内核基线配置段视图",
	"/system/hugepages":                 "大页池读视图（#329）：声明/内核实际/在用页数，无秘密字段",
	"/system/login-users":               "用户列表：只投影 name/class（口令哈希在 model 层就被移除）",
	"/system/logs":                      "系统日志：scrubLogSource 剥掉一次性口令（#325 修复）",
	"/system/status":                    "运行态摘要（uptime/主机名/容量）",
	"/system/tech-support":              "诊断归档**清单**，不含正文",
	"/system/tech-support/{file}":       "诊断归档下载：config 分节脱敏 + 日志分节剥口令（#149）；class 保持 read-only",
	"/system/tls":                       "证书信息（subject/issuer/指纹），**不含私钥**",
	"/system/version":                   "组件版本",
	"/ui":                               "Web 控制台静态资源（无秘密；数据经上述 REST 端点带 Bearer 取）",
	"/ui/":                              "同上",
	"/virtual-machine-functions":        "资源列表（配置视图），无秘密字段",
	"/virtual-machine-functions/{name}": "同上（cloud_init.user_data 属已知例外）",
	"/virtual-machine-functions/{name}/console/ws": "串口 console（一次性 ticket 鉴权；guest 内容，例外同容器日志）",
	"/virtual-machine-functions/{name}/snapshots":  "快照清单元数据",
	"/virtual-switches":                            "配置段视图",
	"/virtual-switches/{name}":                     "同上（含 statistics 运行态）",
	"/virtual-switches/{name}/mac-table":           "MAC 学习表运行态",
	"/virtual-switches/{name}/ports":               "端口读视图（#326）：配置/vnf/container/runtime，无秘密字段",
	"/vpp/capture":                                 "抓包会话/文件清单元数据",
	"/vpp/capture/{file}":                          "抓包导出（例外：数据面明文载荷，抓包本身的用途）",
	"/vpp/config":                                  "vpp 配置段视图",
	"/vpp/status":                                  "VPP 运行态（版本/连接/线程/内存）",
	"/vrfs":                                        "配置段视图",
	"/vrfs/{name}":                                 "同上",
	"/vrfs/{name}/routes":                          "FIB 路由运行态",
}

// TestEverySecretEgressRouteClassified 每条 GET 路由都必须在 secretEgressRoutes 里有分类；
// 未分类（新增出口）即失败——逼作者显式判断「它会不会带出秘密」。
func TestEverySecretEgressRouteClassified(t *testing.T) {
	n := 0
	for _, rt := range registeredRoutes(t) {
		if rt.Method != http.MethodGet {
			continue
		}
		n++
		if _, ok := secretEgressRoutes[rt.Path]; !ok {
			t.Errorf("GET %s 未在 secretEgressRoutes 中分类——新增出口必须显式判断是否带出秘密（决策 #325）", rt.Path)
		}
	}
	if n < 50 {
		t.Fatalf("只解析到 %d 条 GET 路由——路由解析可能失效，本守护等于没跑", n)
	}
	// 反向：清单里的路由若已从 server.go 删除，提示清理（避免清单变成过期文档）。
	live := map[string]bool{}
	for _, rt := range registeredRoutes(t) {
		if rt.Method == http.MethodGet {
			live[rt.Path] = true
		}
	}
	for path := range secretEgressRoutes {
		if !live[path] {
			t.Errorf("secretEgressRoutes 里的 %s 已不在 server.go 注册——请从清单删除（避免分类表漂移）", path)
		}
	}
}

// 令牌本体不进任何 REST 响应：GET /system/api-tokens 只回 token_id（随机 UUID）。
func TestAPITokenValueNeverEchoed(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/api-tokens", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET api-tokens: %d %s", status, data)
	}
	if strings.Contains(string(data), token) {
		t.Fatalf("令牌本体不得回显（凭据泄露）：\n%s", data)
	}
	var wrap struct {
		Tokens []map[string]any `json:"tokens"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil || len(wrap.Tokens) == 0 {
		t.Fatalf("应至少列出本会话一条：%v %s", err, data)
	}
	for _, tk := range wrap.Tokens {
		if id, _ := tk["token_id"].(string); id == "" {
			t.Errorf("清单条目缺 token_id（供逐 token 吊销的稳定 ID）：%+v", tk)
		}
	}
}
