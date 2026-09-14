package cliclient

// FR-SEC-004（决策 #72）：nfvisd 默认自签 HTTPS，客户端须能校验它（证书固定）。

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTestCert 生成一张自签证书作为"服务端证书"用于固定校验。
func writeTestCert(t *testing.T, path string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-local"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}
}

func TestNewWithTLS(t *testing.T) {
	// http → 不设置 TLS 传输
	c, err := NewWithTLS("http://127.0.0.1:18443", TLSOptions{})
	if err != nil || c == nil {
		t.Fatalf("http: %v", err)
	}
	if c.hc.Transport != nil {
		t.Fatal("http 不应设置 TLS 传输")
	}

	// https + 不指定 CA → 构造成功（语义为按系统信任库校验）
	if _, err := NewWithTLS("https://127.0.0.1:18443", TLSOptions{}); err != nil {
		t.Fatalf("https 构造: %v", err)
	}

	// https + insecure → InsecureSkipVerify
	c, err = NewWithTLS("https://127.0.0.1:18443", TLSOptions{Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := c.hc.Transport.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("insecure 应设置 InsecureSkipVerify")
	}

	// https + CA 文件 → RootCAs 生效（证书固定）
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	writeTestCert(t, caPath)
	c, err = NewWithTLS("https://h:443", TLSOptions{CAFile: caPath})
	if err != nil {
		t.Fatalf("CA 校验构造: %v", err)
	}
	tr, ok = c.hc.Transport.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs == nil {
		t.Fatal("给出 CA 文件应设置 RootCAs（证书固定）")
	}
	// 最低版本仍为 TLS 1.2（FR-SEC-004）
	if tr.TLSClientConfig.MinVersion < 0x0303 {
		t.Fatalf("MinVersion 应 >= TLS1.2: %x", tr.TLSClientConfig.MinVersion)
	}

	// 非法 PEM / 文件缺失 → 明确报错
	bad := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(bad, []byte("not a pem"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWithTLS("https://h:443", TLSOptions{CAFile: bad}); err == nil {
		t.Fatal("非法 PEM 应报错")
	}
	if _, err := NewWithTLS("https://h:443", TLSOptions{CAFile: filepath.Join(dir, "none.pem")}); err == nil {
		t.Fatal("文件缺失应报错")
	}
}

// TestDefaultServerMatchesDaemonDefault 守护「CLI 缺省地址 == 守护进程缺省监听」。
//
// 由来（决策 #78）：CLI 缺省曾是 `http://127.0.0.1:8443`（明文、另一端口），而
// `deploy/nfvis.service` 设 `NFVIS_LISTEN=:443` 且 nfvisd 默认自动自签启用 HTTPS
// → **默认参数连不上**，「装完即用」第一步必然失败。本测试从 systemd 单元读取真实缺省，
// 与 cliclient.DefaultServer 比对，防止两者再次漂移。
func TestDefaultServerMatchesDaemonDefault(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/nfvis.service")
	if err != nil {
		t.Fatalf("读取 deploy/nfvis.service: %v", err)
	}
	listen := ""
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "Environment=NFVIS_LISTEN="); ok {
			listen = strings.TrimSpace(v)
		}
	}
	if listen == "" {
		t.Fatal("deploy/nfvis.service 未声明 NFVIS_LISTEN（缺省监听），本守护测试失效")
	}

	u, err := url.Parse(DefaultServer)
	if err != nil {
		t.Fatalf("DefaultServer 不是合法 URL: %q: %v", DefaultServer, err)
	}
	if u.Scheme != "https" {
		t.Fatalf("CLI 缺省应为 https（nfvisd 默认启用 HTTPS，决策 #72）：%q", DefaultServer)
	}

	// NFVIS_LISTEN 形如 ":443" / "0.0.0.0:443" / "127.0.0.1:443"；取其端口与 CLI 缺省比对。
	_, daemonPort, err := net.SplitHostPort(listen)
	if err != nil {
		t.Fatalf("无法从 NFVIS_LISTEN=%q 解析端口: %v", listen, err)
	}
	_, cliPort, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("无法从 DefaultServer=%q 解析端口: %v", DefaultServer, err)
	}
	if daemonPort != cliPort {
		t.Fatalf("CLI 缺省端口 %s 与守护进程缺省 %s 不一致（决策 #78：默认参数会连不上）\n"+
			"改一处须同改另一处：NFVIS_LISTEN(%s) ↔ cliclient.DefaultServer(%s)",
			cliPort, daemonPort, listen, DefaultServer)
	}
}
