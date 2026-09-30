package api

// 决策 #304：REST 等价端点 GET /configuration/permissions?class=<name> 的 handler 测试。
//
// 与 CLI 同一实现单源；边界：无参 = 调用者自己 class、非 super 查他人 403（不泄露存在性）、
// 未知 class 404。响应形状由 shape_contract_test.go 兜底，这里核语义与边界。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
)

// getPermissions GET /configuration/permissions（可带 class 查询参数）。
func getPermissions(t *testing.T, ts *httptest.Server, token, class string) (int, map[string]any) {
	t.Helper()
	url := ts.URL + APIPrefix + "/configuration/permissions"
	if class != "" {
		url += "?class=" + class
	}
	status, body := getWithToken(t, url, token)
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	return status, m
}

func TestGetPermissionsDefaultOwnClass(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	// 无参 = 调用者自己 class（admin=super-user）：全部路径允许，拒绝为 0
	status, m := getPermissions(t, ts, token, "")
	if status != http.StatusOK {
		t.Fatalf("GET 应 200: %d %v", status, m)
	}
	if m["class"] != aaa.ClassSuperUser || m["source"] != "preset" {
		t.Fatalf("无参应取调用者自己 class（super-user）: %v", m)
	}
	if m["denied_count"].(float64) != 0 {
		t.Fatalf("super-user 拒绝条数应为 0: %v", m["denied_count"])
	}
	if m["allowed_count"].(float64) <= 0 {
		t.Fatalf("super-user 应有允许路径: %v", m["allowed_count"])
	}
	if paths, _ := m["allowed_paths"].([]any); len(paths) == 0 {
		t.Fatalf("allowed_paths 不应为空: %v", m)
	}
}

func TestGetPermissionsOtherClassSuperUser(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	status, m := getPermissions(t, ts, token, aaa.ClassReadOnly)
	if status != http.StatusOK {
		t.Fatalf("super-user 查 read-only 应 200: %d %v", status, m)
	}
	// read-only 只能 show：应同时存在允许与拒绝
	if m["allowed_count"].(float64) <= 0 || m["denied_count"].(float64) <= 0 {
		t.Fatalf("read-only 应既有允许又有拒绝: %v", m)
	}
}

func TestGetPermissionsNonSuperOtherForbidden(t *testing.T) {
	ts := newTestServer(t)
	_, vresp := login(t, ts, "viewer", "s3cret-Passw0rd!") // read-only
	// 查自己：可以
	if status, m := getPermissions(t, ts, vresp.Token, aaa.ClassReadOnly); status != http.StatusOK {
		t.Fatalf("read-only 查自己应 200: %d %v", status, m)
	}
	// 查他人：403（不泄露他人规则）
	status, m := getPermissions(t, ts, vresp.Token, aaa.ClassOperator)
	if status != http.StatusForbidden {
		t.Fatalf("read-only 查他人应 403: %d %v", status, m)
	}
	if _, leaked := m["allowed_paths"]; leaked {
		t.Fatalf("403 不得泄露他人 class 的规则: %v", m)
	}
}

func TestGetPermissionsUnknownClass404(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	status, m := getPermissions(t, ts, token, "ghost")
	if status != http.StatusNotFound {
		t.Fatalf("未知 class 应 404: %d %v", status, m)
	}
}

func TestGetPermissionsCustomClass(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	// 建自定义 class：allow show + deny show configuration（deny 优先）
	status, _, body := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/login-users", token,
		map[string]any{"name": "ops", "kind": "class",
			"allow": []string{"show"}, "deny": []string{"show configuration"}}, nil)
	if status != http.StatusCreated {
		t.Fatalf("建 class: %d %s", status, body)
	}
	status, m := getPermissions(t, ts, token, "ops")
	if status != http.StatusOK {
		t.Fatalf("查自定义 class 应 200: %d %v", status, m)
	}
	if m["source"] != "custom" {
		t.Fatalf("自定义 class 的 source 应为 custom: %v", m)
	}
	if allow, _ := m["allow_rules"].([]any); len(allow) != 1 {
		t.Fatalf("allow_rules 应回自定义规则: %v", m["allow_rules"])
	}
	paths, _ := m["allowed_paths"].([]any)
	foundVersion := false
	for _, p := range paths {
		s, _ := p.(string)
		if strings.HasPrefix(s, "show configuration") {
			t.Fatalf("deny 前缀应把 show configuration 排除在允许之外: %v", paths)
		}
		if s == "show version" {
			foundVersion = true
		}
	}
	if !foundVersion {
		t.Fatalf("allow show 应含 show version: %v", paths)
	}
}
