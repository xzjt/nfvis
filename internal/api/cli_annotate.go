package api

// W4：annotate / load / save（FR-CFG-007/008，命令树 §2.1）。
// 决策 #27：注释以语句路径为键存于配置文档顶层 annotations 字段，
// 随事务引擎 compare/rollback，随 load/save 导入导出。

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/schema"
)

// cfgAnnotate annotate <path> "text"（FR-CFG-007）。delete annotate <path> 清除；
// 无引号文本视为删除。
func (x *cliExecutor) cfgAnnotate(user, source string, raw string) string {
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "annotate"))
	if rest == "" {
		return "%% 语法: annotate <path> \"text\"（delete annotate <path> 清除）\n"
	}
	isDelete := strings.HasPrefix(rest, "delete ")
	if isDelete {
		rest = strings.TrimSpace(strings.TrimPrefix(rest, "delete"))
	}
	// 拆出路径 token 与（可选的）引号文本
	text := ""
	if idx := strings.Index(rest, "\""); idx >= 0 {
		text = strings.TrimSpace(rest[idx:])
		text = strings.TrimSuffix(strings.TrimPrefix(text, "\""), "\"")
		rest = strings.TrimSpace(rest[:idx])
	}
	path := strings.Fields(rest)
	if len(path) == 0 {
		return "%% 语法: annotate <path> \"text\"\n"
	}
	if _, _, err := schema.Match(cfgPathRoot(), path); err != nil {
		return "%% " + err.Error() + "\n"
	}
	key := strings.Join(path, " ")

	sess := config.Session{User: user, Source: source}
	if err := x.engine.Edit(sess); err != nil {
		return "%% " + err.Error() + "\n"
	}
	cfg, _, err := x.engine.Candidate()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	if cfg.Annotations == nil {
		cfg.Annotations = map[string]string{}
	}
	if isDelete || text == "" {
		delete(cfg.Annotations, key)
	} else {
		cfg.Annotations[key] = text
	}
	if err := x.engine.UpdateCandidate(sess, cfg); err != nil {
		return "%% " + err.Error() + "\n"
	}
	if isDelete || text == "" {
		return "已删除注释: " + key + "\n"
	}
	return "[ok] " + key + " 注释已更新\n"
}

// cfgLoad load override|merge <file>（FR-CFG-008，JSON 配置导入）。
func (x *cliExecutor) cfgLoad(user, source string, args []string) string {
	if len(args) != 2 || (args[0] != "override" && args[0] != "merge") {
		return "%% 语法: load override|merge <file>\n"
	}
	data, err := os.ReadFile(args[1])
	if err != nil {
		return "%% 读取文件失败: " + err.Error() + "\n"
	}
	var doc model.Config
	if err := json.Unmarshal(data, &doc); err != nil {
		return "%% 配置 JSON 不合法: " + err.Error() + "\n"
	}
	sess := config.Session{User: user, Source: source}
	if err := x.engine.Edit(sess); err != nil {
		return "%% " + err.Error() + "\n"
	}
	if args[0] == "override" {
		err = x.engine.UpdateCandidate(sess, doc)
	} else {
		err = x.engine.MergeCandidate(sess, doc)
	}
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	return "[ok] load " + args[0] + " " + args[1] + "（未提交，需 commit）\n"
}

// cfgSave save <file>（FR-CFG-008，candidate 导出 JSON）。
func (x *cliExecutor) cfgSave(user, source string, args []string) string {
	if len(args) != 1 {
		return "%% 语法: save <file>\n"
	}
	cfg, _, err := x.engine.Candidate()
	if err != nil {
		return "%% 无活跃 candidate 会话\n"
	}
	b, err := json.MarshalIndent(&cfg, "", "  ")
	if err != nil {
		return "%% 序列化失败: " + err.Error() + "\n"
	}
	if err := os.WriteFile(args[0], append(b, '\n'), 0o600); err != nil {
		return "%% 写文件失败: " + err.Error() + "\n"
	}
	return "[ok] 已保存至 " + args[0] + "\n"
}

// renderAnnotations 注释以注释行形式追加在配置渲染末尾（show configuration）。
func renderAnnotations(b *strings.Builder, m map[string]any) {
	ann, ok := m["annotations"]
	if !ok {
		return
	}
	am, ok := ann.(map[string]any)
	if !ok {
		return
	}
	keys := make([]string, 0, len(am))
	for k := range am {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(b, "/* %s: %s */\n", k, scalarStringOf(am[k]))
	}
}

// ensureModelAnnotations 编译期锚点：annotations 字段属于单一数据模型。
var _ = model.Config{}.Annotations
