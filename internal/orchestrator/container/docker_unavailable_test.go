package container

// 决策 #375（R142 B8）：Provider 把底座错误归一为编排层 sentinel——
// 底座不可达/无响应/其它 dockerd 异常 ⇒ ErrContainerUnavailable（API 503）；
// State 与 Exec 之间的竞态（docker 404）⇒ ErrVMNotFound（API 404）；
// 调用方取消 ⇒ 原样透传（不是 503）。
//
// 红绿口径：摘掉 normalizeDockerErr（改回原样返回），本文件断言逐项按预期失败。

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/orchestrator"
)

func TestProviderNormalizesDockerUnavailable(t *testing.T) {
	// ① State 报底座不可达（errDockerUnavailable）⇒ ErrContainerUnavailable
	m := newMockDocker()
	m.stateErr = fmt.Errorf("docker GET /containers/ct/json: %w", errDockerUnavailable)
	p := NewProvider(DefaultConfig(), m)
	if _, err := p.ContainerExec(context.Background(), "ct", "echo x", time.Second); !errors.Is(err, orchestrator.ErrContainerUnavailable) {
		t.Fatalf("State 底座不可达应归 ErrContainerUnavailable，得 %v", err)
	}

	// ② State 报 dockerd 侧其它异常 ⇒ 同样归 ErrContainerUnavailable（契约一律 503）
	m2 := newMockDocker()
	m2.stateErr = fmt.Errorf("docker GET /containers/ct/json: 500 internal")
	p2 := NewProvider(DefaultConfig(), m2)
	if _, err := p2.ContainerExec(context.Background(), "ct", "echo x", time.Second); !errors.Is(err, orchestrator.ErrContainerUnavailable) {
		t.Fatalf("dockerd 侧异常应归 ErrContainerUnavailable，得 %v", err)
	}

	// ③ State 与 Exec 之间的竞态（Exec 报 docker 404）⇒ ErrVMNotFound
	m3 := newMockDocker()
	m3.states["ct"] = orchestrator.CTStateRunning
	m3.execErr = errDockerNotFound
	p3 := NewProvider(DefaultConfig(), m3)
	if _, err := p3.ContainerExec(context.Background(), "ct", "echo x", time.Second); !errors.Is(err, orchestrator.ErrVMNotFound) {
		t.Fatalf("竞态 404 应归 ErrVMNotFound，得 %v", err)
	}

	// ④ ExecShell 拨号失败（errDockerUnavailable）⇒ ErrContainerUnavailable
	m4 := newMockDocker()
	m4.states["ct"] = orchestrator.CTStateRunning
	m4.shellErr = fmt.Errorf("连接 Docker socket: %w", errDockerUnavailable)
	p4 := NewProvider(DefaultConfig(), m4)
	if _, err := p4.ContainerShell(context.Background(), "ct"); !errors.Is(err, orchestrator.ErrContainerUnavailable) {
		t.Fatalf("shell 拨号失败应归 ErrContainerUnavailable，得 %v", err)
	}

	// ⑤ 调用方取消 ⇒ 原样透传（不归 503）
	m5 := newMockDocker()
	m5.states["ct"] = orchestrator.CTStateRunning
	m5.execErr = context.Canceled
	p5 := NewProvider(DefaultConfig(), m5)
	if _, err := p5.ContainerExec(context.Background(), "ct", "echo x", time.Second); !errors.Is(err, context.Canceled) || errors.Is(err, orchestrator.ErrContainerUnavailable) {
		t.Fatalf("调用方取消应原样透传（不 503），得 %v", err)
	}
}
