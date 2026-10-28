package system

// round51 R51-M11（系统侧）：证书/私钥**成对**替换与半残自愈。
//
// 由来：`Install` 与 `regenerate` 各是两次独立的 `os.WriteFile`——换证窗口内一个正在建连的读者
// 会读到「新证书 + 旧私钥」或半写文件（控制台的 GetCertificate 就是这样读的，round47 观测到一次
// `x509: certificate signed by unknown authority`）。本文件钉住替换的原子性与失败语义：
//   - 两个文件**都**先写成同目录临时件，写不成即返回（旧的一对原封不动）；
//   - 进位顺序是**先私钥、后证书**（证书是身份基准，放最后一步）；
//   - 成功后不留临时件、权限仍是 0644/0600。
//
// 半残（私钥丢失）的自愈由 EnsureSelfSigned 承担（判据从 Info 升级为 Usable）。

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ① 写临时件阶段失败 ⇒ 直接返回、正式文件一个都没动（崩溃安全的关键）。
func TestFixR51WritePairAtomicTempFailureLeavesOldPair(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "server.crt")
	oldCert := []byte("OLD CERT\n")
	if err := os.WriteFile(certPath, oldCert, 0o644); err != nil {
		t.Fatal(err)
	}
	// 私钥落在**不存在的目录**里 ⇒ 写临时件必失败。
	keyPath := filepath.Join(dir, "missing-dir", "server.key")

	err := writePairAtomic(certPath, keyPath, []byte("NEW CERT\n"), []byte("NEW KEY\n"))
	if err == nil {
		t.Fatal("临时件写不成时必须返回错误")
	}
	got, rerr := os.ReadFile(certPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != string(oldCert) {
		t.Fatalf("失败时不得动正式文件，证书已被改写:\n%s", got)
	}
	assertNoTLSTemp(t, dir)
}

// ② 进位阶段失败 ⇒ 正式文件同样未被改写——由此**证明顺序是先私钥、后证书**（证书是身份基准，
// 放最后一步；若反过来先换证书，本用例会看到证书已被改写）。
//
// 造法：把「私钥路径」占成一个目录，`os.Rename(file → dir)` 必然失败（Linux ENOTDIR/EISDIR、
// Windows 不允许用文件替换目录）。
func TestFixR51WritePairAtomicKeyRenamedBeforeCert(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "server.crt")
	oldCert := []byte("OLD CERT\n")
	if err := os.WriteFile(certPath, oldCert, 0o644); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "server.key")
	if err := os.Mkdir(keyPath, 0o755); err != nil {
		t.Fatal(err)
	}

	err := writePairAtomic(certPath, keyPath, []byte("NEW CERT\n"), []byte("NEW KEY\n"))
	if err == nil {
		t.Fatal("私钥进位失败时必须返回错误（不能只换证书就宣称换好了）")
	}
	got, rerr := os.ReadFile(certPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != string(oldCert) {
		t.Fatalf("私钥还没进位就换了证书——顺序错了（证书必须最后换）:\n%s", got)
	}
	assertNoTLSTemp(t, dir)
}

// ③ 成功替换：两个文件一起变新、权限照旧、不留临时件。
func TestFixR51WritePairAtomicSuccessKeepsModesAndCleansTemps(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")

	m := NewTLSManager(dir, nil)
	if _, err := m.RegenerateSelfSigned("nfvis-test"); err != nil {
		t.Fatal(err)
	}
	if !m.PairUsable(certPath, keyPath) {
		t.Fatal("重签后应成对可用")
	}
	assertNoTLSTemp(t, dir)
	if runtime.GOOS != "windows" { // Windows 无 POSIX 权限语义
		for path, want := range map[string]os.FileMode{certPath: 0o644, keyPath: 0o600} {
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode().Perm() != want {
				t.Fatalf("%s 权限应为 %o，实际 %o", filepath.Base(path), want, st.Mode().Perm())
			}
		}
	}
}

// ④ 半残（私钥丢失）⇒ 判为不可用并**重建**（此前 Info 只解析证书 ⇒ 原样返回那张永远握不上手的
// 证书：管理面每一次握手都错且不自愈）。正控：可用的一对不得被重签。
func TestFixR51EnsureSelfSignedRepairsHalfPair(t *testing.T) {
	m := NewTLSManager(t.TempDir(), nil)
	if _, err := m.RegenerateSelfSigned("nfvis-test"); err != nil {
		t.Fatal(err)
	}
	before, ok := m.Info()
	if !ok {
		t.Fatal("前提失效：证书本身可解析（旧判据的回答）")
	}
	if err := os.Remove(m.KeyPath()); err != nil {
		t.Fatal(err)
	}
	if _, usable := m.Usable(); usable {
		t.Fatal("前提失效：私钥没了不该判可用")
	}
	if _, ok := m.Info(); !ok {
		t.Fatal("前提失效：旧判据（Info）对半残证书仍答 ok——这正是缺陷本体")
	}
	if before.Fingerprint == "" {
		t.Fatal("前提失效：证书应有指纹")
	}

	info, generated, err := m.EnsureSelfSigned("nfvis-test")
	if err != nil {
		t.Fatalf("半残应重建自签证书而不是报错（既有口径）：%v", err)
	}
	if !generated {
		t.Fatal("半残的一对必须被重建")
	}
	if !m.PairUsable(m.CertPath(), m.KeyPath()) {
		t.Fatal("重建后应成对可用")
	}
	if _, usable := m.Usable(); !usable {
		t.Fatal("重建后 Usable 应为真")
	}

	// 正控：可用的一对原样复用（指纹不变、不再重签）。
	info2, generated2, err := m.EnsureSelfSigned("nfvis-test")
	if err != nil || generated2 {
		t.Fatalf("可用证书不应重复生成: generated=%v err=%v", generated2, err)
	}
	if info2.Fingerprint != info.Fingerprint {
		t.Fatalf("复用的证书指纹应一致: %s vs %s", info.Fingerprint, info2.Fingerprint)
	}
}

// assertNoTLSTemp 目录里不得留写临时件（换证失败时读者若看到半个文件就会被它骗到）。
func assertNoTLSTemp(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("目录里残留临时件 %s", e.Name())
		}
	}
}
