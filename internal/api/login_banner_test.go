package api

// 决策 #303：登录横幅的 handler 与 CLI 语句测试。
//
// 覆盖：GET /login-banner 未认证可达、只含 banner 字段（不泄露其他信息）、
// 未设置时字段省略（不回空串）；PUT/DELETE /system/login-banner 与用户管理同一
// 一次性事务写模式（取锁直提入审计）；取值校验（512 字节上限与单行）在提交处拒绝；
// PUT /system 的全量替换语义**不误伤** banner（banner 属 login 域，未传 = 不改）；
// CLI `set/delete system login banner` 落库与 `| display set` 反推。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
)

// getLoginBanner GET /login-banner（token 可空：端点未认证也可达）。
// 返回解码后的键值对，用于核「只含 banner」与「未设置省略」。
func getLoginBanner(t *testing.T, tsURL, token string) (int, map[string]any) {
	t.Helper()
	status, _, body := cfgRequest(t, http.MethodGet, tsURL+APIPrefix+"/login-banner", token, nil, nil)
	out := map[string]any{}
	if status == http.StatusOK {
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("响应不是对象: %v %s", err, body)
		}
	}
	return status, out
}

func TestLoginBannerUnauthenticatedReachableAndOmittedWhenUnset(t *testing.T) {
	ts := newTestServer(t)
	// 无 token：登录前就要显示，必须 200。
	status, out := getLoginBanner(t, ts.URL, "")
	if status != http.StatusOK {
		t.Fatalf("未认证 GET /login-banner 应 200: %d", status)
	}
	// 未设置：banner 字段省略（不回空串、不编造），响应为空对象。
	if v, ok := out["banner"]; ok {
		t.Fatalf("未设置横幅时 banner 字段应省略，得到 %v", v)
	}
	if len(out) != 0 {
		t.Fatalf("未设置横幅时响应应为空对象（只回横幅文本，不泄露其他信息）: %v", out)
	}
}

func TestLoginBannerSetEchoAndOnlyBannerField(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/system/login-banner", token,
		map[string]any{"banner": "仅限授权人员访问"}, map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("设置横幅: %d %s", status, body)
	}

	// 未认证回显：有 banner 且只有 banner 一个键。
	status, out := getLoginBanner(t, ts.URL, "")
	if status != http.StatusOK {
		t.Fatalf("回显: %d", status)
	}
	if got, ok := out["banner"].(string); !ok || got != "仅限授权人员访问" {
		t.Fatalf("banner 应为设置的文本: %v", out["banner"])
	}
	if len(out) != 1 {
		t.Fatalf("端点只应返回 banner 本身: %v", out)
	}

	// 清除后再读：字段省略。
	status, _, body = cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/system/login-banner", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("清除横幅: %d %s", status, body)
	}
	status, out = getLoginBanner(t, ts.URL, "")
	if status != http.StatusOK {
		t.Fatalf("清除后回显: %d", status)
	}
	if _, ok := out["banner"]; ok {
		t.Fatalf("清除后 banner 字段应省略: %v", out)
	}
}

func TestLoginBannerManagementRequiresAuth(t *testing.T) {
	ts := newTestServer(t)
	status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/system/login-banner", "",
		map[string]any{"banner": "x"}, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("未认证 PUT 应 401: %d", status)
	}
	status, _, _ = cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/system/login-banner", "", nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("未认证 DELETE 应 401: %d", status)
	}
}

func TestLoginBannerPutValidation(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// 空 banner：明确指路（设置给文本，清除走 DELETE）。
	status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/system/login-banner", token,
		map[string]any{"banner": ""}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("空横幅应 400: %d %s", status, body)
	}

	// 超过 512 字节：commit 校验拒绝，错误文案带上限值与当前字节数。
	tooLong := strings.Repeat("横幅", 200) // 600 字节（UTF-8 每字 3 字节）
	status, _, body = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/system/login-banner", token,
		map[string]any{"banner": tooLong}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("超限横幅应 400: %d %s", status, body)
	}
	if !strings.Contains(string(body), "512") {
		t.Fatalf("错误文案应说明 512 字节上限: %s", body)
	}

	// 决策 #316：每次 commit 校验失败都会把候选留脏（操作者可修），而本族的其它一次性
	// 入口此时会被「候选占用」守卫拒绝——故后续用例先丢弃再试，与操作者的正确做法一致。
	discard := func() {
		if st, _, b := cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/configuration/candidate", token, nil, nil); st != http.StatusNoContent {
			t.Fatalf("丢弃候选应 204: %d %s", st, b)
		}
	}

	// 含换行：拒绝（横幅语义上是单行）。
	discard()
	status, _, body = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/system/login-banner", token,
		map[string]any{"banner": "第一行\n第二行"}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("含换行的横幅应 400: %d %s", status, body)
	}
	if !strings.Contains(string(body), "单行") {
		t.Fatalf("错误文案应说明单行约束: %s", body)
	}

	// 边界内（恰 512 字节）应成功。
	discard()
	ok512 := strings.Repeat("a", 512)
	status, _, body = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/system/login-banner", token,
		map[string]any{"banner": ok512}, nil)
	if status != http.StatusOK {
		t.Fatalf("恰 512 字节应成功: %d %s", status, body)
	}
}

// TestPutSystemPreservesLoginBanner PUT /system 是全量替换语义（未传字段 = 清除），
// 但 banner 属 login 域、随 login 节整体保留——未传 banner 字段 = 不改（决策 #303⑤）。
func TestPutSystemPreservesLoginBanner(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/system/login-banner", token,
		map[string]any{"banner": "保留我"}, nil)
	if status != http.StatusOK {
		t.Fatalf("设置横幅: %d %s", status, body)
	}

	// 全量替换 system：不带 banner 字段（连 login 都不带）。
	status, _, body = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/system", token,
		map[string]any{"hostname": "bn-node"}, nil)
	if status != http.StatusOK {
		t.Fatalf("PUT /system: %d %s", status, body)
	}

	status, out := getLoginBanner(t, ts.URL, token)
	if status != http.StatusOK || out["banner"] != "保留我" {
		t.Fatalf("PUT /system 不应改动横幅（未传 = 不改）: %d %v", status, out)
	}
}

// TestCLISetDeleteSystemLoginBanner CLI 语句落库 + display set 反推 + delete 清除。
func TestCLISetDeleteSystemLoginBanner(t *testing.T) {
	x, eng := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		`set system login banner "Authorized access only"`,
		"commit",
	)

	cfg, err := eng.Committed()
	if err != nil {
		t.Fatalf("Committed: %v", err)
	}
	if cfg.System == nil || cfg.System.Login == nil || cfg.System.Login.Banner != "Authorized access only" {
		t.Fatalf("横幅应落库 system.login.banner: %+v", cfg.System)
	}

	// display set 反推由决策 #155 机制自动覆盖（值叶子机械逆走可达，回放自校验护航）。
	out := execOK(t, x, "show configuration | display set")
	if !strings.Contains(out, `set system login banner "Authorized access only"`) {
		t.Fatalf("display set 应能反推横幅语句:\n%s", out)
	}

	// delete 清除。
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure", "delete system login banner", "commit")
	cfg, err = eng.Committed()
	if err != nil {
		t.Fatalf("Committed: %v", err)
	}
	if cfg.System != nil && cfg.System.Login != nil && cfg.System.Login.Banner != "" {
		t.Fatalf("删除后横幅应清空: %+v", cfg.System.Login)
	}
}

// TestLoginBannerModelValidation model.Validate 的单行/512 字节校验（路径与文案口径）。
func TestLoginBannerModelValidation(t *testing.T) {
	mk := func(banner string) model.Config {
		return model.Config{System: &model.SystemConfig{Login: &model.SystemLogin{
			Users:  []model.LoginUserConfig{{Name: "admin", PasswordHash: testUserHash, Class: aaa.ClassSuperUser}},
			Banner: banner,
		}}}
	}
	errs := model.Validate(mk("ok"))
	if len(errs) != 0 {
		t.Fatalf("合法横幅不应报错: %v", errs)
	}
	errs = model.Validate(mk(strings.Repeat("x", 513)))
	if len(errs) != 1 || !strings.Contains(errs[0].Message, "512") {
		t.Fatalf("超限应报 512 上限: %v", errs)
	}
	if errs[0].Path != "system.login.banner" {
		t.Fatalf("错误路径应为 system.login.banner: %v", errs[0].Path)
	}
	errs = model.Validate(mk("a\nb"))
	if len(errs) != 1 || !strings.Contains(errs[0].Message, "单行") {
		t.Fatalf("含换行应报单行约束: %v", errs)
	}
}
