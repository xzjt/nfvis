package config

// 决策 #400：vpp「需重启」提交提示按 startup-affecting 子集判定（R176-4）。

import (
	"context"
	"errors"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

func TestEngineCommitNoVppRestartWarningForDNSProxyOnly(t *testing.T) {
	k := newEngineKit(t)
	sess := Session{User: "admin", Source: "console"}

	// 仅改 DNS 代理上游：不进 startup.conf ⇒ 不应有「需 request vpp restart」提示。
	k.edit(t, "admin", "console")
	cfg := baseCommitted()
	cfg.Vpp = &model.VppConfig{DNSProxyServers: []string{"8.8.8.8"}}
	if err := k.engine.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	res, err := k.engine.Commit(context.Background(), sess, CommitOpts{})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if warningsContain(res.Warnings, "request vpp restart") {
		t.Fatalf("仅改 DNS 代理不应有 vpp 重启提示: %+v", res.Warnings)
	}

	// 对照：在 DNS 代理之上叠加真实 vpp 段变更（plugins）仍应提示。
	k.edit(t, "admin", "console")
	cfg2 := baseCommitted()
	cfg2.Vpp = &model.VppConfig{DNSProxyServers: []string{"8.8.8.8"},
		Plugins: []model.VppPlugin{{Name: "acl", State: "enable"}}}
	if err := k.engine.UpdateCandidate(sess, cfg2); err != nil {
		t.Fatalf("UpdateCandidate2: %v", err)
	}
	res2, err := k.engine.Commit(context.Background(), sess, CommitOpts{})
	if err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			t.Fatalf("Commit2: %v: %+v", err, ve.Errors)
		}
		t.Fatalf("Commit2: %v", err)
	}
	if !warningsContain(res2.Warnings, "request vpp restart") {
		t.Fatalf("cpu 变更仍应有 vpp 重启提示: %+v", res2.Warnings)
	}
}
