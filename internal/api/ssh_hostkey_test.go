package api

// 增量 3 剩余缺口 ③（决策 #125）：`request system ssh host-key regenerate` 的 REST 等价物
// `POST /system/ssh-host-key:regenerate` —— 与 CLI 同一实现（`TlsRuntime.RegenerateSSHHostKeys`），
// 审计同码（`system.ssh.hostkey.regenerate`），失败时 500 且不谎报成功。

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/system"
)

// fakeSSHTLS 记录 SSH host key 重生成是否被调用、可注入失败。
type fakeSSHTLS struct {
	testTLS
	called int
	err    error
}

func (f *fakeSSHTLS) RegenerateSSHHostKeys(ctx context.Context) error {
	f.called++
	return f.err
}

func TestSSHHostKeyRegenerateEndpoint(t *testing.T) {
	fake := &fakeSSHTLS{testTLS: testTLS{m: system.NewTLSManager(t.TempDir(), nil)}}
	ts := newTestServerOpts(t, Options{TLS: fake})
	token := loginAdmin(t, ts)

	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/ssh-host-key:regenerate",
		token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("重生成 host key: %d %s", status, data)
	}
	if fake.called != 1 {
		t.Fatalf("实现应被调用一次，实际 %d 次", fake.called)
	}
	if !strings.Contains(string(data), `"regenerated":true`) {
		t.Fatalf("响应应含 regenerated:true: %s", data)
	}
	// 操作者可见说明必须提示"新连接会变、需更新 known_hosts"（不能只说成功）
	if !strings.Contains(string(data), "known_hosts") {
		t.Fatalf("响应应提示客户端需更新 known_hosts: %s", data)
	}
}

// 失败不谎报：实现报错 → 500 + INTERNAL，且不出现 regenerated:true。
func TestSSHHostKeyRegenerateFailureIsReported(t *testing.T) {
	fake := &fakeSSHTLS{testTLS: testTLS{m: system.NewTLSManager(t.TempDir(), nil)}, err: errors.New("ssh-keygen -A: boom")}
	ts := newTestServerOpts(t, Options{TLS: fake})
	token := loginAdmin(t, ts)

	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/ssh-host-key:regenerate",
		token, nil, nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("实现失败应 500，得到 %d %s", status, data)
	}
	if strings.Contains(string(data), `"regenerated":true`) {
		t.Fatalf("失败时不得谎报成功: %s", data)
	}
	if !strings.Contains(string(data), "boom") {
		t.Fatalf("错误原因应透出: %s", data)
	}
}

// 证书模块未接入 → 503（与 /system/tls* 同口径），不静默成功。
func TestSSHHostKeyRegenerateUnavailable(t *testing.T) {
	ts := newTestServer(t) // 不注入 TLS
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/ssh-host-key:regenerate",
		token, nil, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("未接入应 503，得到 %d %s", status, data)
	}
}
