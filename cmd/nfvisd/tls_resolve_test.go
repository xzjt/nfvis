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
//
// 能力面与 tlsCertSource 逐字对齐（硬成员，修 R51-M11 收口）：真实实现漏一个方法就编译不过，
// 假件同理——「可用性」由 usable/pairUsable 两个字段**显式**表达（默认 false = 这一对不可用，
// 要「已装可用证书」那条格子时显式置 true），免得沿用「存在即可用」的旧口径把缺陷测绿。
type fakeTLSManager struct {
	info   system.TlsInfo
	infoOK bool
	usable bool // Usable() 的回答：证书可解析且与私钥成对
	// pairUsable PairUsable() 的回答（显式 -tls-cert/-tls-key 分支用它）。
	pairUsable bool
	genInfo    system.TlsInfo
	genErr     error

	cert, key string

	infoCalls   int
	usableCalls int
	pairCalls   int
	genCalls    int
}

func (f *fakeTLSManager) Info() (system.TlsInfo, bool) {
	f.infoCalls++
	return f.info, f.infoOK
}

// Usable 只在 usable 为真时答「可用」；为真时带回 info（与真实实现「可用 ⇒ 证书可解析」一致）。
func (f *fakeTLSManager) Usable() (system.TlsInfo, bool) {
	f.usableCalls++
	if !f.usable {
		return system.TlsInfo{}, false
	}
	return f.info, true
}

func (f *fakeTLSManager) PairUsable(string, string) bool {
	f.pairCalls++
	return f.pairUsable
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
		usable: true, // 已装**可用**：证书可解析且与私钥成对（硬读口）
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
	if mgr.usableCalls != 1 {
		t.Fatalf("该格子应由可用性读口回答（Usable 调用两次即判据漂移），实际 usableCalls=%d", mgr.usableCalls)
	}
}

// ②b 半残的已装证书（Info 答 ok、可用读口答否）**绝不能**当成已装证书——落到自签重建。
//
// 这是 R51-M11 的判据格子：旧口径只看「证书能解析 + 两个路径都在」，于是私钥丢失/不匹配时
// 返回 installed，管理面从此每一次握手都错且不自愈。可用性读口是**硬成员**（tlsCertSource），
// 这里钉住它确实被用在 ③ 这条主路径上（没有任何「断言不上就回退旧口径」的旁路）。
func TestResolveTLSHalfBrokenInstalledCertIsNotInstalled(t *testing.T) {
	mgr := &fakeTLSManager{
		infoOK:  true,  // 证书本身可解析（旧判据正是靠这一条误判）
		usable:  false, // 但与私钥不成对/私钥丢失
		cert:    "/var/lib/nfvis/tls/server.crt",
		key:     "/var/lib/nfvis/tls/server.key",
		genInfo: system.TlsInfo{SelfSigned: true, Fingerprint: "AABBCCDD"},
	}
	got, err := ResolveTLS("", "", false, mgr, "nfvis", allFiles)
	if err != nil {
		t.Fatalf("半残应重建自签而不是报错（既有口径）：%v", err)
	}
	if got.Source == tlsSourceInstalled {
		t.Fatalf("Info 答 ok 但可用读口答否时不得判 available：%+v", got)
	}
	if got.Source != tlsSourceSelfSigned {
		t.Fatalf("应走自签重建，实际 %q", got.Source)
	}
	if got.Plaintext {
		t.Fatalf("半残证书不得降级为明文: %+v", got)
	}
	if mgr.genCalls != 1 {
		t.Fatalf("应恰好触发一次自签重建，实际 genCalls=%d", mgr.genCalls)
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
	// pairUsable：显式分支同样验成对（R51-M11）；这里给「成对可用」，与 allFiles 一致。
	mgr := &fakeTLSManager{cert: "/tls/a", key: "/tls/b", pairUsable: true}
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
	if mgr.infoCalls != 0 || mgr.genCalls != 0 || mgr.usableCalls != 0 {
		t.Fatalf("显式证书路径下不得碰 TLSManager，实际 info=%d gen=%d usable=%d",
			mgr.infoCalls, mgr.genCalls, mgr.usableCalls)
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
	mgr.infoCalls, mgr.genCalls, mgr.usableCalls, mgr.pairCalls = 0, 0, 0, 0
}

// ⑤ 只有显式 -allow-plaintext 才进入明文分支；且该分支不碰 TLSManager。
func TestResolveTLSPlaintextOnlyWhenExplicit(t *testing.T) {
	// usable：反向对照组要的是「磁盘上有一份**可用**的已装证书」这个前提。
	mgr := &fakeTLSManager{infoOK: true, usable: true, cert: "/tls/a", key: "/tls/b"}
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
	if mgr.infoCalls != 0 || mgr.genCalls != 0 || mgr.usableCalls != 0 {
		t.Fatalf("即使已装证书存在，-allow-plaintext 也不得碰 TLSManager，实际 info=%d gen=%d usable=%d",
			mgr.infoCalls, mgr.genCalls, mgr.usableCalls)
	}
	// 反向：不给 -allow-plaintext 时，即使有已装证书也必须走证书（不得明文）——对照组。
	notPlain, err := ResolveTLS("", "", false, mgr, "nfvis", allFiles)
	if err != nil {
		t.Fatalf("已装证书下不应报错: %v", err)
	}
	if notPlain.Plaintext {
		t.Fatal("未显式 -allow-plaintext 时绝不允许明文")
	}
	if notPlain.Source != tlsSourceInstalled || notPlain.CertPath != mgr.cert {
		t.Fatalf("对照组前提是「磁盘上有可用的已装证书」，应原样采用：%+v", notPlain)
	}
}
