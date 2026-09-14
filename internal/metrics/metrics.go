// Package metrics 采集与渲染 Prometheus 文本格式（M5-2，FR-SYS-005）。
//
// 设计：采集（各来源）与渲染分离——Render 为纯函数（单测覆盖），
// 主机指标（/proc、statfs）按平台分文件（metrics_linux.go / metrics_other.go），
// 数据面/VNF/告警指标由 API 层从已注入运行态接口取值后以 Sample 传入。
package metrics

import (
	"fmt"
	"sort"
	"strings"
)

// Sample 一条指标样本（Name 为不含标签的指标名）。
type Sample struct {
	Name   string
	Help   string
	Type   string // gauge|counter
	Labels map[string]string
	Value  float64
}

// Render 渲染为 Prometheus text exposition format 0.0.4。
// 同一指标名的 HELP/TYPE 只输出一次；样本按 name+labels 排序保证可复现。
func Render(samples []Sample) string {
	var b strings.Builder
	seen := map[string]bool{}
	// 按 name 稳定排序，同时保持同名的 HELP/TYPE 只出现一次
	ordered := append([]Sample(nil), samples...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Name != ordered[j].Name {
			return ordered[i].Name < ordered[j].Name
		}
		return labelsKey(ordered[i].Labels) < labelsKey(ordered[j].Labels)
	})
	for _, s := range ordered {
		if !seen[s.Name] {
			seen[s.Name] = true
			if s.Help != "" {
				fmt.Fprintf(&b, "# HELP %s %s\n", s.Name, s.Help)
			}
			typ := s.Type
			if typ == "" {
				typ = "gauge"
			}
			fmt.Fprintf(&b, "# TYPE %s %s\n", s.Name, typ)
		}
		b.WriteString(s.Name)
		b.WriteString(labelsText(s.Labels))
		fmt.Fprintf(&b, " %s\n", formatValue(s.Value))
	}
	return b.String()
}

func labelsKey(l map[string]string) string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(l[k])
		sb.WriteByte(',')
	}
	return sb.String()
}

func labelsText(l map[string]string) string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		// 注意：不能用 %q（会二次转义反斜杠/引号）
		parts = append(parts, fmt.Sprintf("%s=\"%s\"", k, escapeLabel(l[k])))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escapeLabel(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return strings.ReplaceAll(v, `"`, `\"`)
}

// formatValue 按 Prometheus 惯例输出：整数不带小数点。
func formatValue(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}
