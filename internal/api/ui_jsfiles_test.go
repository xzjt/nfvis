package api

// Web 控制台前端 JS 源的**共享搜索面**（Web 控制台重构 2026-10：app.js 按 view 域拆分后，
// 前端逻辑散在 ui/*.js 与 ui/js/** 多个文件里——凡「搜 app.js」的守护一律改搜这份集合，
// 断言内容不变、搜索面从单文件变为全量，防止逻辑搬走后守护静默空转）。
//
// 范围口径：ui/ 根下的自有模块（app.js / router.js / vncview.js）+ ui/js/**（core 与 views）；
// ui/novnc/** 是第三方库（注释里本就含 URL 与端点名），不属于守护面，跳过。

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// uiJSFiles 返回前端**自有** JS 源（ui/*.js 与 ui/js/**，不含 novnc/ 第三方库），文件路径 → 源码。
func uiJSFiles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir("ui", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "novnc" {
				return filepath.SkipDir // 第三方库：注释里本就含 URL，不属守护面
			}
			return nil
		}
		if filepath.Ext(path) != ".js" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(path)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 ui/ 下的 js: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("ui/ 下没有任何 js 文件——守护的搜索面空了")
	}
	return out
}

// uiJSFile 取集合里的单文件（守护确实只关心某一份时用；锚点变更要同步调用处）。
func uiJSFile(t *testing.T, name string) string {
	t.Helper()
	src, ok := uiJSFiles(t)[name]
	if !ok {
		t.Fatalf("ui/ 下找不到 %s", name)
	}
	return src
}

// uiJSSortedNames 稳定序文件名清单（报错信息可读）。
func uiJSSortedNames(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
