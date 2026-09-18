package api

// 回归（发现 #10）：重签自签证书必须保留回环 IP SAN。
//
// 起因：`request system api tls regenerate` 走的是 CLI 执行器，它调
// `Regenerate(host, nil)`——重签出的证书**没有任何 IP SAN**；而 `nfvis-cli` 缺省连接
// `https://127.0.0.1:443` 且以服务端证书作信任锚做**完整校验**（决策 #78），
// 于是「重签」当场自毁管理路径，其后所有 CLI 调用报
// `x509: cannot validate certificate for 127.0.0.1 because it doesn't contain any IP SANs`。
//
// 断言口径是**契约语义**（「重签后 CLI 还连得上吗」），不是实现细节（SAN 列表长什么样）：
// 用重签出的证书起一个 TLS 监听，再以它自己的证书为信任锚去连 `127.0.0.1` ——
// 与 nfvis-cli 的缺省连接同一条路径。

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/system"
)

// assertCertUsableForLoopback 以 certPath 为信任锚访问 127.0.0.1（CLI 缺省连接同此路径）。
func assertCertUsableForLoopback(t *testing.T, certPath, keyPath, via string) {
	t.Helper()
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("[%s] 证书/私钥应可加载: %v", via, err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("[%s] 起 TLS 监听: %v", via, err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	pem, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatalf("[%s] 证书不含可用 PEM", via)
	}
	// 地址主机名是 127.0.0.1 → 按 IP SAN 校验，与 CLI 完全一致
	cli := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	}}}
	resp, err := cli.Get("https://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("[%s] 重签后的证书应仍可用于 CLI 缺省连接（127.0.0.1），实际校验失败: %v", via, err)
	}
	_ = resp.Body.Close()
}

// CLI 路径：request system api tls regenerate（FR-SYS-011）。
func TestCLIRegenerateKeepsLoopbackSAN(t *testing.T) {
	mgr := system.NewTLSManager(t.TempDir(), nil)
	x, _ := newCLIKit(t)
	x.setTLS(&testTLS{m: mgr})

	out := run(t, x, "admin", "super-user", "ssh", "request system api tls regenerate")
	if !strings.Contains(out, "自签证书已重签") {
		t.Fatalf("重签回显不符: %s", out)
	}
	assertCertUsableForLoopback(t, mgr.CertPath(), mgr.KeyPath(), "CLI")
}

// REST 路径：POST /system/tls:regenerate（FR-SYS-011）。
func TestRESTRegenerateKeepsLoopbackSAN(t *testing.T) {
	mgr := system.NewTLSManager(t.TempDir(), nil)
	ts := newTestServerOpts(t, Options{TLS: &testTLS{m: mgr}})
	token := loginAdmin(t, ts)

	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/tls:regenerate", token, nil,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("重签: %d %s", status, data)
	}
	assertCertUsableForLoopback(t, mgr.CertPath(), mgr.KeyPath(), "REST")
}
