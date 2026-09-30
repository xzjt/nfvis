package api

// 决策 #307：Web 总览「告警」卡在**列出时间**的位置带上时钟可信标记（NFR-006）。
//
// 判据与 TestUIPermissionsViewRenderWiring 同风格——"源码里有没有这条线"：
// 该卡本就渲染 raised_at，故未同步时须追加 [时钟未同步]；未知/已同步不标。
// **真行为仍由浏览器验收**（决策 #141），本用例替代不了它。

import (
	"os"
	"strings"
	"testing"
)

func TestUIAlarmClockMarkWiring(t *testing.T) {
	jsB, err := os.ReadFile("ui/app.js")
	if err != nil {
		t.Fatalf("读取 ui/app.js: %v", err)
	}
	js := string(jsB)
	for _, want := range []string{
		"function alarmClockMark(",
		"a.time_synced === false",
		"'  [时钟未同步]'",
		"alarmClockMark(a)",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 缺少告警时钟标记的接线：%q", want)
		}
	}
}
