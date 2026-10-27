package main

// 决策 #512：管理面证书解析的语义矩阵守护。
//
// 每条用例对应决策里的一个格子；「不碰 TLSManager」的格子用调用计数断言（真实实现要建目录、
// 写文件，一旦被意外调用就会在只读环境里失败——这正是要防的路径）。

import (
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/system"
)

// fakeTLSManager 记录被调用的次数，用于断言「某条路径下不应碰 TLSManager」。
type fakeTLSManager struct {
	info    system.TlsInfo
	infoOK  bool
	genInfo system.TlsInfo
	genErr  error

	cert, key string

	infoCalls int
	genCalls  int
}

func (f *fakeTLSManager) Info() (system.TlsInfo, bool) {
	f.infoCalls++
	return f.info, f.infoOK
}

func (f *fakeTLSManager) EnsureSelfSigned(hostname string) (system.TlsInfo, bool, error) {
	f.genCalls++
	return f.genInfo, true, f.genErr
}

func (f *fakeTLSManager) CertPath() string { return f.cert }
func (f *fakeTLSManager) KeyPath() string  { return f.key }

// noFiles 表示所有路径都不可读（模拟 TLS 目录不可建/磁盘满/权限被改）。
func noFiles(string) bool { return false }

// allFiles 表示所有路径都可读（用于已装/自签成功后，复核 cert+key 在盘上）。
func allFiles(string) bool { return true }

// ① 未显式给证书、未显式 -allow-plaintext、自签又失败 ⇒ 必须返回错误（绝不静默降级为明文）。
func TestResolveTLSFailClosedWithoutPlaintext(t *testing.T) {
	mgr := &fakeTLSManager{
		infoOK: false,
		genErr: errors.New("mkdir /var/lib/nfvis/tls: permission denied"),
		cert:   "/var/lib/nfvis/tls/server.crt",
		key:    "/var/lib/nfvis/tls/server.key",
	}
	got, err := ResolveTLS("", "", false, mgr, "nfvis", noFiles)
	if err == nil {
		t.Fatalf("证书准备失败时必须返回错误（fail-closed），实际得到 %+v", got)
	}
	if got.Plaintext {
		t.Fatalf("证书准备失败不得降级为明文，实际 %+v", got)
	}
	if got.CertPath != "" || got.KeyPath != "" {
		t.Fatalf("失败时不得返回任何证书路径，实际 %+v", got)
	}
	// 错误文案要点名根因与出路：安装证书，或显式 -allow-plaintext。
	if !strings.Contains(err.Error(), "证书") || !strings.Contains(err.Error(), "-allow-plaintext") {
		t.Fatalf("错误文案要点名根因与出路（安装证书 / -allow-plaintext），实际: %v", err)
	}
}

// ② 已装证书 → 直接用，且不触发自签；证书来源标记为 installed。
func TestResolveTLSUsesInstalledCert(t *testing.T) {
	mgr := &fakeTLSManager{
		infoOK: true,
		cert:   "/etc/nfvis/installed.crt",
		key:    "/etc/nfvis/installed.key",
	}
	got, err := ResolveTLS("", "", false, mgr, "nfvis", allFiles)
	if err != nil {
		t.Fatalf("已装证书可用时不应报错: %v", err)
	}
	if got.CertPath != mgr.cert || got.KeyPath != mgr.key {
		t.Fatalf("应使用已装证书 %q/%q，实际 %+v", mgr.cert, mgr.key, got)
	}
	if got.Source != tlsSourceInstalled {
		t.Fatalf("来源应为 installed，实际 %q", got.Source)
	}
	if got.Plaintext {
		t.Fatalf("已装证书路径不应标记为明文: %+v", got)
	}
	if mgr.genCalls != 0 {
		t.Fatalf("已装证书可用时不得触发自签，实际 genCalls=%d", mgr.genCalls)
	}
}

// ③ 没有可用证书、自签成功 → 用返回的自签路径（含指纹）。
func TestResolveTLSUsesSelfSignedWhenGenerated(t *testing.T) {
	mgr := &fakeTLSManager{
		infoOK:  false,
		genInfo: system.TlsInfo{SelfSigned: true, Fingerprint: "AABBCCDD"},
		cert:    "/var/lib/nfvis/tls/server.crt",
		key:     "/var/lib/nfvis/tls/server.key",
	}
	got, err := ResolveTLS("", "", false, mgr, "nfvis", allFiles)
	if err != nil {
		t.Fatalf("自签成功后不应报错: %v", err)
	}
	if got.CertPath != mgr.cert || got.KeyPath != mgr.key {
		t.Fatalf("应使用自签证书路径 %q/%q，实际 %+v", mgr.cert, mgr.key, got)
	}
	if got.Source != tlsSourceSelfSigned {
		t.Fatalf("来源应为 self-signed，实际 %q", got.Source)
	}
	if got.Fingerprint != "AABBCCDD" {
		t.Fatalf("应带回自签证书指纹，实际 %q", got.Fingerprint)
	}
	if mgr.genCalls != 1 {
		t.Fatalf("应恰好触发一次自签，实际 genCalls=%d", mgr.genCalls)
	}
}

// ④ 显式给了 cert/key → 用给定的，完全不碰 TLSManager（即使同时给了 -allow-plaintext）。
func TestResolveTLSExplicitCertWins(t *testing.T) {
	mgr := &fakeTLSManager{cert: "/tls/a", key: "/tls/b"} // info/gen 一旦被调用即计数
	got, err := ResolveTLS("/opt/cert.pem", "/opt/key.pem", false, mgr, "nfvis", allFiles)
	if err != nil {
		t.Fatalf("显式证书路径可用时不应报错: %v", err)
	}
	if got.CertPath != "/opt/cert.pem" || got.KeyPath != "/opt/key.pem" {
		t.Fatalf("应使用显式证书路径，实际 %+v", got)
	}
	if got.Source != tlsSourceExplicit {
		t.Fatalf("来源应为 explicit，实际 %q", got.Source)
	}
	if mgr.infoCalls != 0 || mgr.genCalls != 0 {
		t.Fatalf("显式证书路径下不得碰 TLSManager，实际 info=%d gen=%d", mgr.infoCalls, mgr.genCalls)
	}
	// 显式证书与 -allow-plaintext 同时给出：明文只忽略「已装/自签」，不忽略显式证书。
	got2, err := ResolveTLS("/opt/cert.pem", "/opt/key.pem", true, mgr, "nfvis", allFiles)
	if err != nil {
		t.Fatalf("显式证书 + -allow-plaintext 不应报错: %v", err)
	}
	if got2.Plaintext || got2.CertPath != "/opt/cert.pem" {
		t.Fatalf("显式证书应压过 -allow-plaintext，实际 %+v", got2)
	}
	// 半对配置（只给 cert 或只给 key）必须拒绝——半残证书会让 API 起不来。
	if _, err := ResolveTLS("/opt/cert.pem", "", false, mgr, "nfvis", allFiles); err == nil {
		t.Fatal("只给 -tls-cert 不给 -tls-key 必须报错")
	}
	if _, err := ResolveTLS("", "/opt/key.pem", false, mgr, "nfvis", allFiles); err == nil {
		t.Fatal("只给 -tls-key 不给 -tls-cert 必须报错")
	}
	mgr.infoCalls, mgr.genCalls = 0, 0
}

// ⑤ 只有显式 -allow-plaintext 才进入明文分支；且该分支不碰 TLSManager。
func TestResolveTLSPlaintextOnlyWhenExplicit(t *testing.T) {
	mgr := &fakeTLSManager{infoOK: true, cert: "/tls/a", key: "/tls/b"}
	got, err := ResolveTLS("", "", true, mgr, "nfvis", allFiles)
	if err != nil {
		t.Fatalf("-allow-plaintext 下不应报错: %v", err)
	}
	if !got.Plaintext {
		t.Fatalf("应标记为明文，实际 %+v", got)
	}
	if got.CertPath != "" || got.KeyPath != "" {
		t.Fatalf("明文分支不得设置证书路径，实际 %+v", got)
	}
	if got.Source != tlsSourcePlaintext {
		t.Fatalf("来源应为 plaintext，实际 %q", got.Source)
	}
	if mgr.infoCalls != 0 || mgr.genCalls != 0 {
		t.Fatalf("即使已装证书存在，-allow-plaintext 也不得碰 TLSManager，实际 info=%d gen=%d", mgr.infoCalls, mgr.genCalls)
	}
	// 反向：不给 -allow-plaintext 时，即使有已装证书也必须走证书（不得明文）——对照组。
	notPlain, err := ResolveTLS("", "", false, mgr, "nfvis", allFiles)
	if err != nil {
		t.Fatalf("已装证书下不应报错: %v", err)
	}
	if notPlain.Plaintext {
		t.Fatal("未显式 -allow-plaintext 时绝不允许明文")
	}
}
