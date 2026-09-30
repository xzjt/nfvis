package cli

// 登录横幅（决策 #303）：nfvis-cli 交互模式启动时（未认证阶段）先取一次
// GET /login-banner，有横幅则打印后再提示登录。
//
//   - `-c` 脚本模式**不调用**本助手——脚本的判定口径是行首 %/%%，横幅会污染输出
//     与判定（既有口径，见 cmd/nfvis-cli 的脚本执行）；
//   - 网络/服务端任何失败都**静默跳过**：横幅是展示性功能，不能挡住登录流程。

import (
	"fmt"
	"io"
)

// BannerFetcher 登录横幅的取数能力（*cliclient.Client 结构性满足；收成接口便于不打真连接的单测）。
type BannerFetcher interface {
	LoginBanner() (string, error)
}

// PrintLoginBanner 取一次横幅并打印（横幅文本 + 空行，与随后的登录提示隔开）。
// 取不到（网络失败/未设置/取数器为空）时一个字都不输出并返回 false。
func PrintLoginBanner(w io.Writer, f BannerFetcher) bool {
	if f == nil {
		return false
	}
	banner, err := f.LoginBanner()
	if err != nil || banner == "" {
		return false
	}
	fmt.Fprintf(w, "%s\n\n", banner)
	return true
}
