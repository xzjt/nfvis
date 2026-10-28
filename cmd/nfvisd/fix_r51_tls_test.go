package main

// round51 R51-M11：一对「不匹配/半残」的证书被当成可用，且替换不是原子的。
//
// 由来：判定口径是「证书能解析 **且** 两个路径都存在」（Info + os.Stat），而私钥是否真的配上对
// 谁都没验——私钥从未写出（Install/regenerate 那两次独立 os.WriteFile 之间的中间态）、私钥被换、
// 证书与私钥本就不匹配时，装配层都判「有已装证书、用它」，日志还写「使用已安装的管理证书启用
// HTTPS」；随后 API 侧 LoadX509KeyPair 失败 ⇒ **每一次 TLS 握手都错**，管理面彻底不可达，且没有
// 明文回退、也没有自愈（只能旁路救场）。
//
// 修复口径：判**可用性**（tls.LoadX509KeyPair 成对加载，与 API 侧同一判据）而不是存在性；
// 半残一律按「没有可用证书」处理（走既有的自签重建自愈，与 #512 的 fail-closed 一致）；
// 显式 `-tls-cert/-tls-key` 分支同样验成对（既有两条「不可读」文案逐字不变，新增的只在
// 「读得到但不配对」时出现）。
//
// 本文件用**真实** *system.TLSManager（临时目录里真实的证书/私钥文件）跑，假件测不到这条路径。

import (
	"os"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/system"
)

// r51TLSManager 造一个临时目录里的真实证书管理器。
func r51TLSManager(t *testing.T, hostname string) *system.TLSManager {
	t.Helper()
	m := system.NewTLSManager(t.TempDir(), nil)
	if _, err := m.RegenerateSelfSigned(hostname); err != nil {
		t.Fatalf("造自签证书: %v", err)
	}
	return m
}

// r51BreakKeyPair 把 m 的私钥换成**另一把**（证书不动）——等价于「证书与私钥不匹配」。
func r51BreakKeyPair(t *testing.T, m *system.TLSManager) {
	t.Helper()
	other := r51TLSManager(t, "nfvis-other")
	key, err := os.ReadFile(other.KeyPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.KeyPath(), key, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, usable := m.Usable(); usable {
		t.Fatal("前提失效：换成另一把私钥后这一对不该被判可用")
	}
}

// ① 不匹配的一对不得被当成「已装证书」；按既有口径落到自签重建，且**真的修好了**（自愈）。
func TestFixR51TLSMismatchedPairIsNotUsableAndSelfHeals(t *testing.T) {
	m := r51TLSManager(t, "nfvis-test")
	before, ok := m.Info()
	if !ok {
		t.Fatal("前提失效：证书本身应可解析（这正是旧判据的回答）")
	}
	r51BreakKeyPair(t, m)

	got, err := ResolveTLS("", "", false, m, "nfvis-test", fileExists)
	if err != nil {
		t.Fatalf("半残证书应自愈（重建自签），不应报错: %v", err)
	}
	if got.Source == tlsSourceInstalled {
		t.Fatal("不匹配的一对不得当成已装证书（修复前正是这条：管理面每次握手都错）")
	}
	if got.Source != tlsSourceSelfSigned {
		t.Fatalf("应走自签重建，实际来源 %q", got.Source)
	}
	if got.Plaintext {
		t.Fatal("半残证书不得降级为明文")
	}
	// 返回的那一对必须真的能加载（与 API 侧同一判据）。
	if !m.PairUsable(got.CertPath, got.KeyPath) {
		t.Fatal("返回的证书/私钥应成对可用")
	}
	// 磁盘上也已修好（下次启动不会再判成半残）。
	if _, usable := m.Usable(); !usable {
		t.Fatal("重建后磁盘上应留下一对可用证书")
	}
	after, _ := m.Info()
	if after.Fingerprint == before.Fingerprint {
		t.Fatal("自愈应产生新证书（指纹应变化）")
	}
}

// ② 成对可用的一对：照旧接受、路径不变、**不重签**（正控——别把正常实例的证书动掉）。
func TestFixR51TLSMatchingPairAcceptedUnchanged(t *testing.T) {
	m := r51TLSManager(t, "nfvis-test")
	before, ok := m.Info()
	if !ok {
		t.Fatal("前提失效：证书应可解析")
	}
	got, err := ResolveTLS("", "", false, m, "nfvis-test", fileExists)
	if err != nil {
		t.Fatalf("可用证书不应报错: %v", err)
	}
	if got.Source != tlsSourceInstalled {
		t.Fatalf("可用证书来源应为 installed，实际 %q", got.Source)
	}
	if got.CertPath != m.CertPath() || got.KeyPath != m.KeyPath() {
		t.Fatalf("应返回已装证书路径，实际 %+v", got)
	}
	after, _ := m.Info()
	if after.Fingerprint != before.Fingerprint {
		t.Fatal("可用的已装证书不得被重签")
	}
}

// ③ 显式 -tls-cert/-tls-key：既有的「不可读」文案逐字不变，新增「读得到但不配对」的如实拒绝。
func TestFixR51TLSExplicitPairValidated(t *testing.T) {
	good := r51TLSManager(t, "nfvis-explicit")
	got, err := ResolveTLS(good.CertPath(), good.KeyPath(), false, good, "nfvis", fileExists)
	if err != nil {
		t.Fatalf("成对的显式证书不应报错: %v", err)
	}
	if got.Source != tlsSourceExplicit || got.CertPath != good.CertPath() {
		t.Fatalf("显式证书路径应原样采用，实际 %+v", got)
	}

	bad := r51TLSManager(t, "nfvis-explicit")
	r51BreakKeyPair(t, bad)
	_, err = ResolveTLS(bad.CertPath(), bad.KeyPath(), false, bad, "nfvis", fileExists)
	if err == nil {
		t.Fatal("不成对的显式证书必须拒绝（否则 HTTPS 全新握手都会失败）")
	}
	// `%q` 会把 Windows 路径里的反斜杠转义成 `\\`，比对前先还原（Linux 上是空操作）。
	msg := strings.ReplaceAll(err.Error(), `\\`, `\`)
	if !strings.Contains(msg, bad.CertPath()) || !strings.Contains(msg, bad.KeyPath()) {
		t.Fatalf("拒绝文案要点名两个路径，实际: %v", err)
	}

	// 既有两条「不可读」文案不受影响（仍旧只 stat，不该被成对校验改掉）。
	_, err = ResolveTLS("C:/nope/missing.crt", "C:/nope/missing.key", false, bad, "nfvis", fileExists)
	if err == nil || !strings.Contains(err.Error(), "不可读") {
		t.Fatalf("路径不可读仍应报「不可读」: %v", err)
	}
}
