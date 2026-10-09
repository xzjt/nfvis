package config

// 决策 #400：vpp「需重启」提交提示按 startup-affecting 子集判定（R176-4）。
// 决策 #439：内核数据面下「vpp 段配置不生效」的提示同样按该子集收窄——DNSProxyServers 是
// 数据面中立配置（全局上游在内核侧同样生效），只有存在别的字段才提示不生效。

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

// 决策 #439：内核数据面下「vpp 段配置不生效」的提示按「vpp 段里存在 DNS 代理上游之外的字段」
// 判定——DNSProxyServers 是数据面中立配置（内核侧作为全局上游同样生效）。
// 红-绿：把 dataPlaneWarnings 的条件恢复成 `new.Vpp != nil`，本用例第一段即失败。
func TestEngineCommitKernelNoVppSectionWarningForDNSProxyOnly(t *testing.T) {
	k := newEngineKit(t)
	sess := Session{User: "admin", Source: "console"}
	kernel := func(vpp *model.VppConfig) model.Config {
		cfg := baseCommitted()
		cfg.System.DataPlane = model.DataPlaneKernel
		cfg.Vpp = vpp
		return cfg
	}
	commit := func(cfg model.Config) CommitResult {
		t.Helper()
		k.edit(t, "admin", "console")
		if err := k.engine.UpdateCandidate(sess, cfg); err != nil {
			t.Fatalf("UpdateCandidate: %v", err)
		}
		res, err := k.engine.Commit(context.Background(), sess, CommitOpts{})
		if err != nil {
			var ve *ValidationError
			if errors.As(err, &ve) {
				t.Fatalf("Commit: %v: %+v", err, ve.Errors)
			}
			t.Fatalf("Commit: %v", err)
		}
		return res
	}

	// ① 内核 + vpp 段仅含 DNS 代理全局上游 ⇒ 无「vpp 段配置不生效」提示
	//（数据面切换本身的「需重启」提示不受影响）。
	res := commit(kernel(&model.VppConfig{DNSProxyServers: []string{"8.8.8.8"}}))
	if warningsContain(res.Warnings, "vpp 段配置不生效") {
		t.Fatalf("仅 DNS 代理上游不应提示 vpp 段不生效: %+v", res.Warnings)
	}
	if !warningsContain(res.Warnings, "需重启服务") {
		t.Fatalf("数据面切换仍应提示需重启: %+v", res.Warnings)
	}

	// ② 叠加真实 vpp 段字段（plugins）⇒ 提示照旧。
	res2 := commit(kernel(&model.VppConfig{DNSProxyServers: []string{"8.8.8.8"},
		Plugins: []model.VppPlugin{{Name: "acl", State: "enable"}}}))
	if !warningsContain(res2.Warnings, "vpp 段配置不生效") {
		t.Fatalf("vpp 段存在插件时仍应提示不生效: %+v", res2.Warnings)
	}

	// ③ vpp 段整体删除（nil）⇒ 无该提示。
	res3 := commit(kernel(nil))
	if warningsContain(res3.Warnings, "vpp 段配置不生效") {
		t.Fatalf("vpp 段为空不应提示 vpp 段不生效: %+v", res3.Warnings)
	}

	// ④ 真机形状：CLI 删完 vpp 调参叶子后，段里留下**非 nil 的空壳**（cpu:{}/memory:{}/dpdk:{dev:{}}）
	// ⇒ 内容上仍只有 DNS 代理上游，不应提示（判据必须按内容——逐字节比较会被空壳骗过；
	// 这是真机当场抓到的形状）。
	res4 := commit(kernel(&model.VppConfig{
		DNSProxyServers: []string{"8.8.8.8"},
		CPU:             &model.VppCPU{},
		Memory:          &model.VppMemory{},
		DPDK:            &model.VppDPDK{},
	}))
	if warningsContain(res4.Warnings, "vpp 段配置不生效") {
		t.Fatalf("空壳 vpp 段（无实际字段）不应提示 vpp 段不生效: %+v", res4.Warnings)
	}

	// ⑤ 空壳之上有**真值**（dpdk 全局参数）⇒ 提示照旧。
	res5 := commit(kernel(&model.VppConfig{
		DNSProxyServers: []string{"8.8.8.8"},
		DPDK:            &model.VppDPDK{Dev: model.VppDevDefault{RxDescriptors: 1024}},
	}))
	if !warningsContain(res5.Warnings, "vpp 段配置不生效") {
		t.Fatalf("vpp 段有实际字段（dpdk 全局参数）时仍应提示不生效: %+v", res5.Warnings)
	}
}
