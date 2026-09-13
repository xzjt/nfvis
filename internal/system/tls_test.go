package system

import (
	"crypto/tls"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestTLSRegenerateAndInfo(t *testing.T) {
	dir := t.TempDir()
	m := NewTLSManager(dir, (&fakeRunner{}).run)

	info, err := m.Regenerate("nfvis-vm", []string{"192.168.155.129"})
	if err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	if !info.SelfSigned || info.Fingerprint == "" || info.NotAfter.Before(info.NotBefore) {
		t.Fatalf("自签信息不符: %+v", info)
	}
	if info.Subject == "" || info.Issuer != info.Subject {
		t.Fatalf("subject/issuer 应相同: %+v", info)
	}
	// 落盘可被 tls 库加载（证书与私钥配对）
	if _, err := tls.LoadX509KeyPair(m.CertPath(), m.KeyPath()); err != nil {
		t.Fatalf("证书/私钥应可加载: %v", err)
	}
	if runtime.GOOS != "windows" { // Windows 无 POSIX 权限语义
		st, err := os.Stat(m.KeyPath())
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("私钥权限应为 0600，实际 %o", st.Mode().Perm())
		}
	}
	// 重签后指纹应变化（换证书判定口径）
	first := info.Fingerprint
	info2, err := m.Regenerate("nfvis-vm", []string{"192.168.155.129"})
	if err != nil {
		t.Fatal(err)
	}
	if info2.Fingerprint == first {
		t.Fatal("重签后指纹应变化")
	}
	got, ok := m.Info()
	if !ok || got.Fingerprint != info2.Fingerprint {
		t.Fatalf("Info 读回不符: %+v", got)
	}
}

func TestTLSInstallValidatesPair(t *testing.T) {
	dir := t.TempDir()
	m := NewTLSManager(dir, (&fakeRunner{}).run)
	if _, err := m.Regenerate("h", nil); err != nil {
		t.Fatal(err)
	}
	certPEM, _ := os.ReadFile(m.CertPath())
	keyPEM, _ := os.ReadFile(m.KeyPath())

	dir2 := t.TempDir()
	m2 := NewTLSManager(dir2, (&fakeRunner{}).run)
	info, err := m2.Install(string(certPEM), string(keyPEM))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if info.Fingerprint == "" {
		t.Fatalf("安装后应可解析: %+v", info)
	}
	// 证书/私钥不匹配应拒绝
	if _, err := m.Regenerate("h2", nil); err != nil {
		t.Fatal(err)
	}
	otherKey, _ := os.ReadFile(m.KeyPath())
	if _, err := m2.Install(string(certPEM), string(otherKey)); err == nil ||
		!strings.Contains(err.Error(), "不匹配") {
		t.Fatalf("证书/私钥不匹配应拒绝: %v", err)
	}
	if _, err := m2.Install("not a cert", string(keyPEM)); err == nil {
		t.Fatal("非 PEM 证书应拒绝")
	}
}

func TestCertExpiryAlarm(t *testing.T) {
	dir := t.TempDir()
	m := NewTLSManager(dir, (&fakeRunner{}).run)
	if _, ok := m.ExpiryAlarm(); ok {
		t.Fatal("未配置证书不应告警")
	}
	if _, err := m.Regenerate("h", nil); err != nil {
		t.Fatal(err)
	}
	days, warn := m.ExpiryAlarm()
	if warn || days < 300 {
		t.Fatalf("新签证书 1 年内不应告警: days=%d warn=%v", days, warn)
	}
}

func TestApplyLogRetentionWritesDropIn(t *testing.T) {
	old := journaldDropInDir
	journaldDropInDir = t.TempDir()
	defer func() { journaldDropInDir = old }()

	fr := &fakeRunner{}
	path, err := ApplyLogRetention(t.Context(), fr.run, 30, 512)
	if err != nil {
		t.Fatalf("ApplyLogRetention: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[Journal]", "SystemMaxUse=512M", "MaxRetentionSec=30d"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("drop-in 缺少 %q:\n%s", want, got)
		}
	}
	if !strings.Contains(strings.Join(fr.calls, " "), "systemctl kill -s HUP systemd-journald") {
		t.Fatalf("应 SIGHUP journald 使其重载: %v", fr.calls)
	}
	// 0/0 表示不限制 → 不生成 drop-in
	fr2 := &fakeRunner{}
	p2, err := ApplyLogRetention(t.Context(), fr2.run, 0, 0)
	if err != nil || p2 != "" {
		t.Fatalf("未设置保留策略应跳过: %q %v", p2, err)
	}
	if len(fr2.calls) != 0 {
		t.Fatalf("未设置策略不应执行命令: %v", fr2.calls)
	}

}
