package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// ---------- 配置事务 API（/configuration/*，决策 #22） ----------

func loginAdmin(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	status, resp := login(t, ts, "admin", "s3cret-Passw0rd!")
	if status != http.StatusOK {
		t.Fatalf("登录失败: %d", status)
	}
	return resp.Token
}

func loginViewer(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	status, resp := login(t, ts, "viewer", "s3cret-Passw0rd!")
	if status != http.StatusOK {
		t.Fatalf("viewer 登录失败: %d", status)
	}
	return resp.Token
}

func cfgRequest(t *testing.T, method, url, token string, body any, header map[string]string) (int, http.Header, []byte) {
	t.Helper()
	var buf io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化: %v", err)
		}
		buf = strings.NewReader(string(data))
	}
	req, err := http.NewRequest(method, url, buf)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
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

func sampleCandidate() model.Config {
	return model.Config{
		System: &model.SystemConfig{Hostname: "api-node"},
		ResourcePools: &model.ResourcePool{
			Hugepages: []model.HPool{{PageSize: "1G", Count: 32}},
			CPU:       &model.CPUSetup{IsolatedCores: []int{4, 5, 6, 7}},
		},
	}
}

func TestConfigurationTransactionFlow(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// 无会话时 GET candidate → 409
	status, _, _ := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/candidate", token, nil, nil)
	if status != http.StatusConflict {
		t.Fatalf("无会话 GET candidate 应 409: %d", status)
	}

	// PUT candidate（默认写 candidate 不提交，决策 #22）
	status, hdr, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token, sampleCandidate(), nil)
	if status != http.StatusOK || hdr.Get("X-NFVIS-Committed") != "false" {
		t.Fatalf("PUT candidate 应 200 且未提交: %d %s", status, data)
	}

	// GET candidate 回读一致
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/candidate", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET candidate: %d %s", status, data)
	}
	var got struct {
		Candidate model.Config `json:"candidate"`
		Dirty     bool         `json:"dirty"`
	}
	if err := json.Unmarshal(data, &got); err != nil || !got.Dirty || got.Candidate.System.Hostname != "api-node" {
		t.Fatalf("candidate 回读不符: %s", data)
	}

	// diff 非空（JunOS 风格文本）
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/diff", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "api-node") {
		t.Fatalf("diff 应包含变更: %d %s", status, data)
	}

	// commit → revision 2
	status, _, data = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/configuration/commit", token, map[string]any{"message": "api 提交"}, nil)
	if status != http.StatusOK {
		t.Fatalf("commit 应成功: %d %s", status, data)
	}
	var res struct {
		Committed bool `json:"committed"`
		Revision  int  `json:"revision"`
	}
	if err := json.Unmarshal(data, &res); err != nil || !res.Committed || res.Revision != 4 { // rev1 初始+rev2 引导+rev3 viewer+本次
		t.Fatalf("commit 响应不符: %s", data)
	}

	// 提交后 diff 为空、candidate 干净
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/diff", token, nil, nil)
	if status != http.StatusOK || strings.TrimSpace(string(data)) != "" {
		t.Fatalf("提交后 diff 应为空: %q", data)
	}

	// DELETE candidate（discard）→ 204，之后会话释放
	status, _, _ = cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/configuration/candidate", token, nil, nil)
	if status != http.StatusNoContent {
		t.Fatalf("discard 应 204: %d", status)
	}
	status, _, _ = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/candidate", token, nil, nil)
	if status != http.StatusConflict {
		t.Fatalf("discard 后 GET candidate 应 409: %d", status)
	}
}

func TestConfigurationAutoCommit(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// X-NFVIS-Auto-Commit: true → 校验+下发+落库一次完成（决策 #22）
	status, hdr, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		sampleCandidate(), map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK || hdr.Get("X-NFVIS-Committed") != "true" {
		t.Fatalf("直提应 200 且 X-NFVIS-Committed: true: %d %s %s", status, hdr.Get("X-NFVIS-Committed"), data)
	}
	var res struct {
		Revision int `json:"revision"`
	}
	_ = json.Unmarshal(data, &res)
	if res.Revision != 4 { // rev1+引导+viewer+本次
		t.Fatalf("直提 revision 应为 4: %s", data)
	}
}

func TestConfigurationValidationErrorsDetail(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	bad := sampleCandidate()
	bad.VirtualSwitches = []model.VirtualSwitch{{Name: "vs1", Type: "l2", VlanAccess: 4095}}
	status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		bad, map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusBadRequest {
		t.Fatalf("校验失败应 400: %d %s", status, data)
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(data, &errResp); err != nil || errResp.Code != "VALIDATION_FAILED" {
		t.Fatalf("错误码应为 VALIDATION_FAILED: %s", data)
	}
	found := false
	for _, d := range errResp.Detail {
		// FR-API-005：校验错误逐条列出
		if strings.Contains(d.Path, "vs1") && strings.Contains(d.Message, "vlan") {
			found = true
		}
	}
	if !found {
		t.Fatalf("detail 应含逐条校验错误: %+v", errResp.Detail)
	}

	// 失败后 candidate 保留（FR-CFG-002）
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/candidate", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "vs1") {
		t.Fatalf("校验失败后 candidate 应保留: %d %s", status, data)
	}
}

func TestConfigurationConfirmedAndConfirm(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	if status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token, sampleCandidate(), nil); status != http.StatusOK {
		t.Fatalf("PUT candidate: %d", status)
	}
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/configuration/commit", token,
		map[string]any{"confirmed_minutes": 10, "message": "confirmed via api"}, nil)
	if status != http.StatusOK {
		t.Fatalf("confirmed commit: %d %s", status, data)
	}
	var res struct {
		ConfirmedUntil string `json:"confirmed_until"`
	}
	if err := json.Unmarshal(data, &res); err != nil || res.ConfirmedUntil == "" {
		t.Fatalf("应返回 confirmed_until: %s", data)
	}

	// 会话列表展示 confirmed 状态（决策 #26）
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/configuration/sessions", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "admin@api") || !strings.Contains(string(data), "confirmed_until") {
		t.Fatalf("sessions 应含持有者与 confirmed 状态: %d %s", status, data)
	}

	// 确认
	status, _, data = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/configuration/commit:confirm", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "confirmed") {
		t.Fatalf("确认应成功: %d %s", status, data)
	}
}

func TestConfigurationRollbackAndLockConflict(t *testing.T) {
	ts := newTestServer(t)
	adminToken := loginAdmin(t, ts)

	// 两次提交产生历史
	for _, name := range []string{"v2", "v3"} {
		cfg := sampleCandidate()
		cfg.System.Hostname = name
		if status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", adminToken, cfg,
			map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
			t.Fatalf("直提 %s: %d", name, status)
		}
	}

	// rollback 需要编辑会话：直提是一次性事务、提交后不再持锁（决策 #151），
	// 故先按客户端流程取锁（控制台 cfghTakeCandidate 同款：以当前生效配置为底稿写一次候选）。
	if status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", adminToken, sampleCandidate(), nil); status != http.StatusOK {
		t.Fatalf("取编辑锁: %d", status)
	}

	// rollback 1 → candidate 回到 v2，需再 commit 生效（FR-CFG-005）
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/configuration/rollback/1", adminToken, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "v2") {
		t.Fatalf("rollback 1 应取 v2 为 candidate: %d %s", status, data)
	}
	status, _, _ = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/configuration/commit", adminToken, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("rollback 后 commit: %d", status)
	}

	// rollback 越界 → 404
	status, _, _ = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/configuration/rollback/99", adminToken, nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("rollback 99 应 404: %d", status)
	}

	// 会话锁互斥（FR-CFG-009）：viewer 经 W5 端点重建后（read-only）→ 403
	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/login-users", adminToken,
		map[string]any{"name": "viewer", "kind": "user", "password": "s3cret-Passw0rd!", "class": "read-only"},
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("重建 viewer")
	}
	cfg := sampleCandidate()
	cfg.System.Hostname = "other"
	status, data = func() (int, []byte) {
		st, _, d := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", adminToken, cfg, nil)
		return st, d
	}()
	if status != http.StatusOK {
		t.Fatalf("同持有者重复 PUT 应幂等接管: %d %s", status, data)
	}
	// 用同一 token 但模拟另一用户：viewer 无 configure 权限 → 403（等级隔离）
	viewerToken := func() string {
		status, resp := login(t, ts, "viewer", "s3cret-Passw0rd!")
		if status != http.StatusOK {
			t.Fatalf("viewer 登录: %d", status)
		}
		return resp.Token
	}()
	status, _, data = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", viewerToken, cfg, nil)
	if status != http.StatusForbidden {
		t.Fatalf("read-only 访问配置端点应 403: %d %s", status, data)
	}
}
