package api

// BUG-22（外部测试报告 docs/REPORT-v3.0.29.md，round46）：REST 写未知配置字段静默成功。
//
// 现场（真机）：
//
//	PUT /configuration/candidate  {"system":{"dns":{"servers":["198.51.100.97"]}}}
//	→ 200 {"committed":true,"revision":125}      ← 真实字段是 system.dns_servers，配置什么都没变
//
// 根因：`handlePutCandidate` 用裸 `json.Unmarshal`（未知键被 encoding/json 静默丢弃），而 CLI 路径
// 经 schema 树回放 + `fromJSONTree` 的 `DisallowUnknownFields` 是严格的——只有 REST 缺这一步。
//
// 本文件钉住四件事：
//  1. **拒绝**：未知字段 → 400 VALIDATION_FAILED、文案点名完整路径、revision 不变、不取锁；
//  2. **放行**：正确字段照旧生效（回归，不能因为加校验把正常写入弄坏）；
//  3. **不误拒自由键值对**：顶层 `annotations` 与容器的 `env` 的键**任意**（它们本就该自由）；
//     以及**产品自己产出的文档**（`show configuration | display json` 与 `GET /configuration`）
//     必须能被原样写回——否则新校验会误杀自己的读视图；
//  4. **CLI 同源路径**：`load override|merge` 也拒绝（同一文案），而 `set` 的既有严格性不回归。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
)

// bug22Revision 读 committed 当前 revision（未知字段被拒后必须**不变**）。
func bug22Revision(t *testing.T, ts *httptest.Server, token string) int {
	t.Helper()
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("读 committed: %d %s", status, data)
	}
	var got struct {
		Revision int `json:"revision"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("解析 committed: %v %s", err, data)
	}
	return got.Revision
}

// TestBug22PutCandidateRejectsUnknownField 主判据：BUG-22 的原始请求必须 **400** 且点名路径。
//
// 同时钉住「失败无副作用」：revision 不变、会话没拿到锁（连 GET candidate 都是 409）、
// 之后的正常写入不受影响。
func TestBug22PutCandidateRejectsUnknownField(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	revBefore := bug22Revision(t, ts, token)

	bad := `{"system":{"dns":{"servers":["198.51.100.97"]}}}`
	status, hdr, data := bug22RawPut(t, ts, token, bad, map[string]string{
		"X-NFVIS-Auto-Commit": "true", "X-NFVIS-Merge": "true",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("未知字段应 400，得到 %d: %s", status, data)
	}
	if got := hdr.Get("X-NFVIS-Committed"); got != "" {
		t.Fatalf("被拒的请求不得回 X-NFVIS-Committed（得到 %q）", got)
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(data, &errResp); err != nil {
		t.Fatalf("错误响应不是 JSON: %v %s", err, data)
	}
	if errResp.Code != "VALIDATION_FAILED" {
		t.Fatalf("错误码应为 VALIDATION_FAILED: %s", data)
	}
	// 文案点名**完整路径**（encoding/json 只给最后一段，这里补足上下文）与相近的正确字段。
	for _, want := range []string{"未知字段", "system.dns.servers", "dns_servers"} {
		if !strings.Contains(errResp.Message, want) {
			t.Fatalf("错误文案应含 %q，得到 %q", want, errResp.Message)
		}
	}
	if revAfter := bug22Revision(t, ts, token); revAfter != revBefore {
		t.Fatalf("被拒的写入不得改变 revision（%d → %d）", revBefore, revAfter)
	}
	// 校验在 engine.Edit 之前：没取锁、没建 candidate。
	if status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/candidate", token, nil, nil); status != http.StatusConflict {
		t.Fatalf("被拒后不该留下 candidate 会话（应 409），得到 %d: %s", status, data)
	}
	// 同一会话随后的正常写入不受影响（若上面漏了释放锁，这里会 409）。
	ok := withSuperUser(model.Config{System: &model.SystemConfig{DNSServers: []string{"192.0.2.53"}}})
	if status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token, ok,
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("被拒后正常写入应仍成功: %d %s", status, data)
	}
}

// TestBug22PutCandidateAcceptsCorrectField 回归：**正确**的字段名照旧提交成功且配置生效
// （报告里同一脚本改用 `dns_servers` 后就是这条路径）。
func TestBug22PutCandidateAcceptsCorrectField(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	good := `{"system":{"dns_servers":["1.1.1.1"]}}`
	status, hdr, data := bug22RawPut(t, ts, token, good, map[string]string{
		"X-NFVIS-Auto-Commit": "true", "X-NFVIS-Merge": "true",
	})
	if status != http.StatusOK {
		t.Fatalf("正确字段应 200，得到 %d: %s", status, data)
	}
	if hdr.Get("X-NFVIS-Committed") != "true" {
		t.Fatalf("应已提交（X-NFVIS-Committed: true），得到 %q", hdr.Get("X-NFVIS-Committed"))
	}
	var res struct {
		Committed bool `json:"committed"`
		Revision  int  `json:"revision"`
	}
	if err := json.Unmarshal(data, &res); err != nil || !res.Committed || res.Revision == 0 {
		t.Fatalf("提交响应不符: %s", data)
	}
	// 配置真的生效（读 committed 的 system.dns_servers，而不是只看 200）
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("读 system: %d %s", status, data)
	}
	var sys struct {
		DNSServers []string `json:"dns_servers"`
	}
	if err := json.Unmarshal(data, &sys); err != nil {
		t.Fatalf("解析 system: %v %s", err, data)
	}
	if len(sys.DNSServers) != 1 || sys.DNSServers[0] != "1.1.1.1" {
		t.Fatalf("dns_servers 应生效为 [1.1.1.1]，得到 %v (%s)", sys.DNSServers, data)
	}
}

// TestBug22FreeFormMapsStillAccepted 自由键值对**不得**被拒：顶层 `annotations`（键=语句路径）
// 与容器的 `env`（键=环境变量名）本就是任意键。这是「一刀切 DisallowUnknownFields」最容易踩的坑。
func TestBug22FreeFormMapsStillAccepted(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	doc := map[string]any{
		"system": map[string]any{
			"hostname": "free-form",
			// 判据：committed 里至少要留一个 super-user（决策 #152）
			"login": map[string]any{"users": []map[string]any{superUserDoc()}},
		},
		// 任意键（含空格、中文、点号）——annotations 的键就是语句路径本身
		"annotations": map[string]any{
			"system hostname":     "主机名说明（任意键都合法）",
			"system dns_servers":  "上游 DNS",
			"a.b.c[0].not-a-real": "看起来像配置路径但不是字段",
		},
		"container_functions": []map[string]any{{
			"name":  "ct-env",
			"image": "alpine:3.20",
			// env 的键是环境变量名，任意（含大小写、点号、破折号）
			"env": map[string]any{"ANY_KEY_X": "v", "MY.DOT.KEY": "1", "with-dash": "2", "中文键": "3"},
		}},
	}
	status, hdr, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token, doc,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("自由键值对（annotations/env）应被接受，得到 %d: %s", status, data)
	}
	if hdr.Get("X-NFVIS-Committed") != "true" {
		t.Fatalf("应已提交: %q", hdr.Get("X-NFVIS-Committed"))
	}
	// 内容真的落地（不是「200 但什么都没写」）——BUG-22 的教训正是不能只看状态码。
	cfg, err := readCommittedConfig(t, ts, token)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if got := cfg.Annotations["system hostname"]; !strings.Contains(got, "主机名说明") {
		t.Fatalf("annotations 应原样保留，得到 %v", cfg.Annotations)
	}
	if len(cfg.ContainerFunctions) != 1 || cfg.ContainerFunctions[0].Env["MY.DOT.KEY"] != "1" {
		t.Fatalf("容器 env 应原样保留，得到 %+v", cfg.ContainerFunctions)
	}
}

// TestBug22ProductOwnDocumentsAccepted 产品**自己产出的**文档不得被新校验误拒：
//   - `show configuration | display json`（CLI 结构化视图）；
//   - `GET /configuration` 的 configuration 段（Web 控制台的「原始 JSON」就是它回填的）。
//
// 这条是「加严格校验」最现实的自伤风险，故与主判据同档守护。
func TestBug22ProductOwnDocumentsAccepted(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// 先建一份覆盖较广的配置（含 map 字段与自定义解码字段 PortSecMAC）。
	seed := withSuperUser(model.Config{
		System: &model.SystemConfig{
			Hostname:   "bug22-seed",
			DNSServers: []string{"192.0.2.53", "2001:db8::53"},
		},
		// 两个接口各带一种「自定义解码 / 指针子结构」字段（风暴抑制与端口安全互斥，不能同口）。
		Interfaces: []model.InterfaceConfig{
			{Name: "ens224", Description: "bug22", MTU: 9000, PortSecurity: []model.PortSecMAC{"AA:BB:CC:DD:EE:FF"}},
			{Name: "ens256", StormControl: &model.StormControl{BroadcastKbps: 1000}},
		},
		VirtualSwitches: []model.VirtualSwitch{{
			Name: "vs-a", Type: "l2",
			Ports: []model.VSwitchPort{{Seq: 1, Interface: "ens224"}},
		}},
		ContainerFunctions: []model.ContainerFunction{{
			Name: "ct-a", Image: "alpine:3.20",
			Env: map[string]string{"FOO": "bar"},
		}},
	})
	if status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token, seed,
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("提交种子配置: %d %s", status, data)
	}
	seedCfg, err := readCommittedConfig(t, ts, token)
	if err != nil {
		t.Fatalf("%v", err)
	}
	seedCfg.Annotations = map[string]string{"system hostname": "种子注释"}

	// ① REST 自己的读视图（Web 控制台回填/回写的正是它）——含 annotations，整体写回。
	status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token, seedCfg,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("GET /configuration 的文档应能被写回，得到 %d: %s", status, data)
	}

	// ② CLI `show configuration | display json` 的输出同样能被写回。
	x, _ := newCLIKit(t)
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration | display json")
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("display json 失败: %s", res.Output)
	}
	var tree map[string]any
	if err := json.Unmarshal([]byte(res.Output), &tree); err != nil {
		t.Fatalf("display json 不是 JSON 对象: %v", err)
	}
	body, _ := json.Marshal(tree)
	status, _, data = bug22RawPut(t, ts, token, string(body), map[string]string{
		"X-NFVIS-Auto-Commit": "true", "X-NFVIS-Merge": "true",
	})
	if status != http.StatusOK {
		t.Fatalf("display json 的输出应能被写回，得到 %d: %s", status, data)
	}
}

// TestBug22CLILoadRejectsUnknownField 同源宽松路径：`load override|merge <file>` 用同一套校验
// （同一文案），且失败**不取锁、不动候选**；正确的文件照旧可用。
func TestBug22CLILoadRejectsUnknownField(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh", "configure")

	badPath := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(badPath, []byte(`{"system":{"dns":{"servers":["198.51.100.97"]}}}`), 0o600); err != nil {
		t.Fatalf("写坏文件: %v", err)
	}
	for _, mode := range []string{"override", "merge"} {
		res := x.Execute("admin", aaaClassSU, "ssh", "load "+mode+" "+badPath)
		for _, want := range []string{"%%", "未知字段", "system.dns.servers", "dns_servers"} {
			if !strings.Contains(res.Output, want) {
				t.Fatalf("load %s 的输出应含 %q，得到 %q", mode, want, res.Output)
			}
		}
	}

	// 正确的文件照旧：load merge 后 show 里出现新值。
	goodPath := filepath.Join(t.TempDir(), "good.json")
	if err := os.WriteFile(goodPath, []byte(`{"system":{"dns_servers":["203.0.113.53"]}}`), 0o600); err != nil {
		t.Fatalf("写好文件: %v", err)
	}
	out := run(t, x, "admin", aaaClassSU, "ssh", "load merge "+goodPath)
	if !strings.Contains(out, "[ok] load merge") {
		t.Fatalf("正确文件应被接受: %s", out)
	}
	res := x.Execute("admin", aaaClassSU, "ssh", "show configuration candidate | display json")
	if !strings.Contains(res.Output, "203.0.113.53") {
		t.Fatalf("load merge 应真的写入候选（不能只看 [ok]）: %s", res.Output)
	}
}

// TestBug22CLISetStrictnessUnchanged CLI 路径的既有严格性不回归（配置语句写错关键字仍报错）。
func TestBug22CLISetStrictnessUnchanged(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh", "configure")
	for _, line := range []string{
		"set virtual-switches vs-a nosuch-field 1",
		"set system nosuch-field 1",
		"set system dns servers 1.1.1.1", // 正确语句是 `set system dns server <ip>`（报告 BUG-21）
	} {
		res := x.Execute("admin", aaaClassSU, "ssh", line)
		if !strings.Contains(res.Output, "%%") {
			t.Fatalf("%q 应被拒绝（CLI 关键字严格性），得到 %q", line, res.Output)
		}
	}
}

// TestBug22UnknownFieldMessageShapes 文案的路径定位（白盒，纯函数）：
// 完整路径、数组下标、顶层、以及「相近字段」提示的取舍。
func TestBug22UnknownFieldMessageShapes(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want []string // 必须出现的片段
		deny []string // 不得出现的片段
	}{
		{
			name: "BUG-22 现场（整棵子树不存在 → 补出叶子）",
			doc:  `{"system":{"dns":{"servers":["198.51.100.97"]}}}`,
			want: []string{"system.dns.servers", `"dns_servers"`},
		},
		{
			name: "顶层未知键",
			doc:  `{"nosuchtop":1}`,
			want: []string{`"nosuchtop"`},
		},
		{
			name: "子对象未知键",
			doc:  `{"system":{"hostname":"h","nosuchsub":1}}`,
			want: []string{"system.nosuchsub"},
		},
		{
			name: "数组元素内未知键（带下标）",
			doc:  `{"virtual_switches":[{"name":"vs-a","type":"l2","nosuch_field":1}]}`,
			want: []string{"virtual_switches[0].nosuch_field"},
		},
		{
			name: "拼写相近的正确字段（单复数）",
			doc:  `{"system":{"dns_server":["1.1.1.1"]}}`,
			want: []string{"system.dns_server", `"dns_servers"`},
		},
		{
			name: "annotations 的任意键不报",
			doc:  `{"annotations":{"system hostname":"note"},"system":{"hostname":"h"}}`,
			deny: []string{"未知字段"},
		},
		{
			name: "容器 env 的任意键不报",
			doc:  `{"container_functions":[{"name":"c1","image":"alpine","env":{"ANY_KEY":"v"}}]}`,
			deny: []string{"未知字段"},
		},
		{
			name: "正确字段不报",
			doc:  `{"system":{"hostname":"h","dns_servers":["1.1.1.1"]}}`,
			deny: []string{"未知字段"},
		},
		{
			name: "空文档不报（T10 套件的空 merge 就是 {}）",
			doc:  `{}`,
			deny: []string{"未知字段"},
		},
	}
	for _, c := range cases {
		got := unknownConfigFieldMessage([]byte(c.doc))
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Fatalf("%s: 文案应含 %q，得到 %q", c.name, w, got)
			}
		}
		for _, d := range c.deny {
			if strings.Contains(got, d) {
				t.Fatalf("%s: 文案不应含 %q，得到 %q", c.name, d, got)
			}
		}
	}
}

// bug22RawPut 直接发**原始字符串**请求体（cfgRequest 会 Marshal，无法表达「字段名写错」）。
func bug22RawPut(t *testing.T, ts *httptest.Server, token, body string, header map[string]string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, data
}

// readCommittedConfig 读 committed 配置的强类型形态（判据用「配置里真有什么」，不是状态码）。
func readCommittedConfig(t *testing.T, ts *httptest.Server, token string) (model.Config, error) {
	t.Helper()
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("读 committed: %d %s", status, data)
	}
	var got struct {
		Configuration model.Config `json:"configuration"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		return model.Config{}, err
	}
	return got.Configuration, nil
}
