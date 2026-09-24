// 守护：自锁兜底的**豁免开关**（config.CommitOpts.AllowNoSuperUser）在**产品代码**里
// 只允许一个赋值点——恢复出厂（request system zeroize）。
//
// 由来（决策 #152）：整文档提交必须至少留一个 super-user，否则一次提交就能把本机提交成
// 「无人可登录」（只能带外恢复）。这条兜底唯一的例外是恢复出厂——它的目的就是把账号随空
// 配置复位。例外一旦被复制到第二条路径（`restore`、某个 CLI 命令、某个 REST handler），
// 兜底就在那条路上静默失效，而且**行为测试照旧全绿**（那条路本来就没写用例）。
//
// 所以判据落在源码结构上，与 TestUIConsoleRoleGating 同款「读源码断言」：
//   - 产品代码（非 _test.go）里 `AllowNoSuperUser: true` 恰好 1 处，且位于 internal/system/backup.go；
//   - 引擎里确实读了这个开关（`!opts.AllowNoSuperUser` 恰好 1 处）——只加字段不接线同样危险。
//
// 测试文件不受此限：用例要正反两面都验（豁免能过、不豁免被拒），置位是必要的。
package archtest

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// allowNoSuperUserAssign 赋值形态（含换行/空格的宽松写法）。
var allowNoSuperUserAssign = regexp.MustCompile(`AllowNoSuperUser\s*:\s*true`)

// allowNoSuperUserRead 引擎里的读取点（兜底实际生效的地方）。
var allowNoSuperUserRead = regexp.MustCompile(`!opts\.AllowNoSuperUser`)

// prodGoFiles 产品代码（非测试）的 Go 文件相对路径（斜杠分隔）。
func prodGoFiles(t *testing.T) map[string]string {
	t.Helper()
	root := filepath.Join("..", "..")
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "build", "prototype", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".go") || strings.HasSuffix(info.Name(), "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		out[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("遍历仓库: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("没扫到任何产品 Go 文件——扫描根或排除项写错了")
	}
	return out
}

func TestAllowNoSuperUserAssignedOnlyByZeroize(t *testing.T) {
	files := prodGoFiles(t)

	// ① 赋值点唯一，且必须在恢复出厂那条路上。
	var sites []string
	for rel, src := range files {
		if n := len(allowNoSuperUserAssign.FindAllString(src, -1)); n > 0 {
			sites = append(sites, rel)
			if rel != "internal/system/backup.go" {
				t.Errorf("%s 里置了 AllowNoSuperUser——该开关只允许恢复出厂（request system zeroize）使用，"+
					"置到别处等于关掉「提交后至少留一个 super-user」的自锁兜底（决策 #152）", rel)
			}
		}
	}
	if len(sites) != 1 {
		t.Fatalf("AllowNoSuperUser 的赋值点应恰好 1 处（internal/system/backup.go），实得 %d: %v", len(sites), sites)
	}
	if n := len(allowNoSuperUserAssign.FindAllString(files["internal/system/backup.go"], -1)); n != 1 {
		t.Errorf("internal/system/backup.go 里的赋值应恰好 1 处，实得 %d", n)
	}

	// ② 开关必须真的被引擎读（只加字段不接线 = 兜底静默失效）。
	engine := files["internal/config/engine.go"]
	if engine == "" {
		t.Fatal("读不到 internal/config/engine.go")
	}
	if n := len(allowNoSuperUserRead.FindAllString(engine, -1)); n != 1 {
		t.Errorf("engine.go 里 `!opts.AllowNoSuperUser` 应恰好 1 处（兜底的读取点），实得 %d", n)
	}
}
