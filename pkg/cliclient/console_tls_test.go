package cliclient

// 决策 #156：console 的 wss 拨号必须复用 REST 客户端的 TLS 口径
// （缺省证书固定 / -ca-file / -insecure）——自签 HTTPS 下 console 曾必挂
// （x509: certificate signed by unknown authority，真机实测）。

import (
	"encoding/pem"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func TestDialConsoleUsesPinnedCert(t *testing.T) {
	echo := websocket.Handler(func(ws *websocket.Conn) { _, _ = io.Copy(ws, ws) })
	srv := httptest.NewTLSServer(echo)
	defer srv.Close()

	// 把 httptest 自签证书落盘，走与真机相同的「证书固定」路径
	pemPath := filepath.Join(t.TempDir(), "server.crt")
	if err := os.WriteFile(pemPath,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewWithTLS(srv.URL, TLSOptions{CAFile: pemPath})
	if err != nil {
		t.Fatalf("NewWithTLS: %v", err)
	}
	stream, err := c.DialConsole("/api/v1/virtual-machine-functions/vm/console/ws?ticket=x")
	if err != nil {
		t.Fatalf("证书固定后 wss 拨号应成功（修复前报 x509 unknown authority）: %v", err)
	}
	defer stream.Close()
	if _, err := stream.Write([]byte("ping")); err != nil {
		t.Fatalf("写入: %v", err)
	}
	_ = stream.(interface{ SetReadDeadline(time.Time) error }).SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(stream, buf); err != nil {
		t.Fatalf("回读: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo 不符: %q", buf)
	}
}

func TestDialConsoleInsecureAlsoWorks(t *testing.T) {
	echo := websocket.Handler(func(ws *websocket.Conn) { _, _ = io.Copy(ws, ws) })
	srv := httptest.NewTLSServer(echo)
	defer srv.Close()
	c, err := NewWithTLS(srv.URL, TLSOptions{Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := c.DialConsole("/console/ws?ticket=x")
	if err != nil {
		t.Fatalf("-insecure 下 wss 拨号应成功: %v", err)
	}
	defer stream.Close()
}
