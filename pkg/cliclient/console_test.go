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
		got, err := cl.wsURL(c.path, "串口")
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
	if _, err := cl.wsURL("", "串口"); err == nil {
		t.Fatal("空路径应报错")
	}
}

// 决策 #375（R142 B10）：拨号失败文案随调用方传入的会话显示名变化——同一函数被 VM 串口与
// 容器终端共用，写死「串口 console」会在容器终端上张冠李戴。空路径是最易构造的失败点。
func TestWSURLEmptyMessageUsesSessionName(t *testing.T) {
	cl := New("http://127.0.0.1:8443")
	for _, what := range []string{"串口", "容器终端"} {
		_, err := cl.wsURL("", what)
		if err == nil {
			t.Fatalf("%q 空路径应报错", what)
		}
		if got := err.Error(); got != what+" ws 路径为空" {
			t.Fatalf("空路径文案应含会话显示名 %q，得 %q", what, got)
		}
	}
}
