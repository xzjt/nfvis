package cli

// 登录横幅打印助手（决策 #303）的单测：不打真连接，取数器用桩。
//
// 覆盖：有横幅打印并返回 true；未设置（空串）与网络失败都静默（零输出、返回 false）；
// nil 取数器安全。

import (
	"bytes"
	"errors"
	"testing"
)

type stubFetcher struct {
	banner string
	err    error
	calls  int
}

func (s *stubFetcher) LoginBanner() (string, error) {
	s.calls++
	return s.banner, s.err
}

func TestPrintLoginBannerPrintsWhenSet(t *testing.T) {
	var out bytes.Buffer
	f := &stubFetcher{banner: "仅限授权人员访问"}
	if !PrintLoginBanner(&out, f) {
		t.Fatal("有横幅应返回 true")
	}
	if f.calls != 1 {
		t.Fatalf("应只取一次横幅: %d", f.calls)
	}
	if got := out.String(); got != "仅限授权人员访问\n\n" {
		t.Fatalf("输出应为横幅 + 空行: %q", got)
	}
}

func TestPrintLoginBannerSilentWhenUnsetOrFails(t *testing.T) {
	cases := map[string]stubFetcher{
		"未设置": {},
		"失败":  {err: errors.New("连接 nfvisd 失败")},
	}
	for name, f := range cases {
		var out bytes.Buffer
		if PrintLoginBanner(&out, &f) {
			t.Fatalf("%s：应静默跳过（返回 false）", name)
		}
		if out.Len() != 0 {
			t.Fatalf("%s：不应有输出，得到 %q", name, out.String())
		}
	}
}

func TestPrintLoginBannerNilFetcher(t *testing.T) {
	var out bytes.Buffer
	if PrintLoginBanner(&out, nil) {
		t.Fatal("nil 取数器应返回 false")
	}
	if out.Len() != 0 {
		t.Fatalf("不应有输出: %q", out.String())
	}
}
