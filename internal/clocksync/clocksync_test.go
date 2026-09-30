package clocksync

// 三态标记（NFR-006）的单一事实源自校准：nil 探针=未知、真/假原样透出。
// 审计与告警都调本函数，本用例守住「口径本身」不被改偏。

import "testing"

func TestMarkThreeStates(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name  string
		probe func() bool
		want  *bool
	}{
		{"未注入探针保持未知", nil, nil},
		{"已同步", func() bool { return true }, &yes},
		{"未同步", func() bool { return false }, &no},
	} {
		got := Mark(tc.probe)
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("%s：应未知(nil)，实得 %v", tc.name, *got)
		case tc.want != nil && got == nil:
			t.Errorf("%s：应 %v，实得未知(nil)", tc.name, *tc.want)
		case tc.want != nil && *got != *tc.want:
			t.Errorf("%s：应 %v，实得 %v", tc.name, *tc.want, *got)
		}
	}
}
