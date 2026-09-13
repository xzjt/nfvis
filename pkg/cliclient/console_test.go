package cliclient

// M4-12：console WebSocket 地址解析（http→ws / https→wss，相对路径拼接）。

import "testing"

func TestWSURLSchemeMapping(t *testing.T) {
	cases := []struct {
		base, path, want string
	}{
		{"http://127.0.0.1:8443", "/api/v1/virtual-machine-functions/fw/console/ws?ticket=x",
			"ws://127.0.0.1:8443/api/v1/virtual-machine-functions/fw/console/ws?ticket=x"},
		{"https://nfvis.local", "/api/v1/virtual-machine-functions/fw/console/ws?ticket=y",
			"wss://nfvis.local/api/v1/virtual-machine-functions/fw/console/ws?ticket=y"},
	}
	for _, c := range cases {
		cl := New(c.base)
		got, err := cl.wsURL(c.path)
		if err != nil {
			t.Fatalf("wsURL(%q): %v", c.path, err)
		}
		if got != c.want {
			t.Errorf("wsURL(%q) = %q，期望 %q", c.path, got, c.want)
		}
	}
}

func TestWSURLRejectsEmpty(t *testing.T) {
	cl := New("http://127.0.0.1:8443")
	if _, err := cl.wsURL(""); err == nil {
		t.Fatal("空路径应报错")
	}
}
