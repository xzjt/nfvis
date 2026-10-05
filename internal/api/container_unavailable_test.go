package api

// 决策 #375（R142 B8）：exec handler 与 shell ticket handler 对「Docker 不可用」如实映射 503。
//
// 红绿口径：旧实现下 exec 的 ErrContainerUnavailable 落 500、shell ticket 对非 deadline 的
// State 错误**照发 ticket**（200）——下列断言逐项按预期失败。

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/xzjt/nfvis/internal/orchestrator"
)

// exec 端点：底座不可用 ⇒ 503（旧实现落 500 INTERNAL）。
func TestContainerExecUnavailableMaps503(t *testing.T) {
	ct := &execFakeCT{fakeCLIContainer: newFakeCLIContainer()}
	ct.execErr = orchestrator.ErrContainerUnavailable
	ts := newTestServerOpts(t, Options{Containers: ct})
	token := loginAdmin(t, ts)
	seedContainerCommit(t, ts, token, "ct-a")

	status, _, raw := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/container-functions/ct-a:exec", token,
		map[string]any{"command": "echo hi"}, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("底座不可用应 503 UNAVAILABLE，得 %d %s", status, raw)
	}
}

// errStateCT ContainerState 返回注入错误（非 deadline），模拟 dockerd 连接失败。
type errStateCT struct {
	*fakeCLIContainer
	err error
}

func (e *errStateCT) ContainerState(context.Context, string) (string, error) { return "", e.err }

// shell ticket 端点：State 非 deadline 错误 ⇒ 503（旧实现照发 ticket 落 200）。
func TestContainerShellTicketUnavailableMaps503(t *testing.T) {
	ct := &errStateCT{fakeCLIContainer: newFakeCLIContainer(), err: errors.New("docker down")}
	ts := newTestServerOpts(t, Options{Containers: ct})
	token := loginAdmin(t, ts)
	seedContainerCommit(t, ts, token, "ct-a")

	status, _, raw := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/container-functions/ct-a/shell", token, nil, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("State 非 deadline 错误应 503（不照发 ticket），得 %d %s", status, raw)
	}
}
