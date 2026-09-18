package system

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestTLSRegenerateAndInfo(t *testing.T) {
	dir := t.TempDir()
	m := NewTLSManager(dir, (&fakeRunner{}).run)

	info, err := m.regenerate("nfvis-vm", []string{"192.168.155.129"})
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
	info2, err := m.regenerate("nfvis-vm", []string{"192.168.155.129"})
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
	if _, err := m.regenerate("h", nil); err != nil {
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
	if _, err := m.regenerate("h2", nil); err != nil {
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
	if _, err := m.regenerate("h", nil); err != nil {
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

// FR-SEC-004（决策 #72）：EnsureSelfSigned 缺失时生成、已存在时复用。
func TestEnsureSelfSigned(t *testing.T) {
	m := NewTLSManager(t.TempDir(), nil)

	info, generated, err := m.EnsureSelfSigned("nfvis-test")
	if err != nil {
		t.Fatalf("EnsureSelfSigned: %v", err)
	}
	if !generated {
		t.Fatal("首次调用应生成证书")
	}
	if !info.SelfSigned || !strings.Contains(info.Subject, "nfvis-test") {
		t.Fatalf("证书信息不符: %+v", info)
	}

	// 第二次应复用（不重新生成）——以指纹是否变化判定
	info2, generated2, err := m.EnsureSelfSigned("nfvis-test")
	if err != nil {
		t.Fatalf("EnsureSelfSigned(2): %v", err)
	}
	if generated2 {
		t.Fatal("已存在证书不应重复生成")
	}
	if info2.Fingerprint != info.Fingerprint {
		t.Fatalf("复用的证书指纹应一致: %s vs %s", info.Fingerprint, info2.Fingerprint)
	}
}

// 决策 #99：自签证书的 SAN 由管理端按本机监听地址统一推导——回环必须在里面，
// 且监听地址与本机接口地址都要覆盖；重复项去重。
func TestServerCertSANs(t *testing.T) {
	sans := ServerCertSANs("192.168.155.7:443")
	got := map[string]bool{}
	for _, ip := range sans {
		if got[ip] {
			t.Fatalf("SAN 应去重: %v", sans)
		}
		got[ip] = true
	}
	for _, want := range []string{"127.0.0.1", "::1", "192.168.155.7"} {
		if !got[want] {
			t.Fatalf("SAN 应含 %s: %v", want, sans)
		}
	}
	// 通配监听（缺省 :443）：仍须含回环——CLI 缺省就靠它连
	for _, ip := range ServerCertSANs(":443") {
		if ip == "127.0.0.1" {
			return
		}
	}
	t.Fatal("通配监听时 SAN 仍须含 127.0.0.1")
}

// 决策 #99：重签自签证书的 SAN 取自 TLSManager 记录的监听地址（SetListen）。
func TestRegenerateSelfSignedUsesListen(t *testing.T) {
	m := NewTLSManager(t.TempDir(), nil)
	m.SetListen("10.9.8.7:443")
	if _, err := m.RegenerateSelfSigned("nfvis-vm"); err != nil {
		t.Fatalf("RegenerateSelfSigned: %v", err)
	}
	raw, err := os.ReadFile(m.CertPath())
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	var haveLoopback, haveListen bool
	for _, ip := range cert.IPAddresses {
		switch ip.String() {
		case "127.0.0.1":
			haveLoopback = true
		case "10.9.8.7":
			haveListen = true
		}
	}
	if !haveLoopback || !haveListen {
		t.Fatalf("证书 SAN IP 应含回环与监听地址，实际 %v", cert.IPAddresses)
	}
	// DNS SAN：主机名 + localhost
	joined := strings.Join(cert.DNSNames, ",")
	if !strings.Contains(joined, "nfvis-vm") || !strings.Contains(joined, "localhost") {
		t.Fatalf("证书 DNS SAN 不符: %v", cert.DNSNames)
	}
}
