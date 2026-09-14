package cliclient

// FR-SEC-004（决策 #72）：nfvisd 默认自签 HTTPS，客户端须能校验它（证书固定）。

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
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
