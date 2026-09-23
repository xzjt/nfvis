package api

// 抓包「停止并导出」（决策 #129）：`DELETE /vpp/capture?export=true` —— 与 CLI
// `request vpp trace export` 同义。此前 REST 只能「停止不导出」，界面因此接不上导出能力。

import (
	"net/http"
	"strings"
	"testing"
)

// 导出成功：200 + 文件名/大小；且 fake 记录到 export=true 被传入。
func TestCaptureStopAndExport(t *testing.T) {
	f := &fakeCapture{active: &CaptureSessionRow{Interface: "ens192"}}
	ts := newTestServerOpts(t, Options{Capture: f})
	token := loginAdmin(t, ts)

	status, _, data := cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/vpp/capture?export=true", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("停止并导出: %d %s", status, data)
	}
	if !f.exported {
		t.Fatal("应把 export=true 传给实现（否则等于只停止）")
	}
	for _, want := range []string{`"exported":true`, `"name"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("响应应含 %s: %s", want, data)
		}
	}
}

// 缺省（不带 export）仍是「停止不导出」：204，且实现收到 export=false。
func TestCaptureStopWithoutExport(t *testing.T) {
	f := &fakeCapture{active: &CaptureSessionRow{Interface: "ens192"}}
	ts := newTestServerOpts(t, Options{Capture: f})
	token := loginAdmin(t, ts)

	status, _, _ := cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/vpp/capture", token, nil, nil)
	if status != http.StatusNoContent {
		t.Fatalf("缺省停止应 204，得到 %d", status)
	}
	if f.exported {
		t.Fatal("缺省不该导出")
	}
}
