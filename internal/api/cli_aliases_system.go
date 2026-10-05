package api

// 系统类配置语句别名（M5-5/M5-8）。
//
// 背景：与网络/计算类同因（见 cli_aliases_net.go 头部）——CLI 树把阈值/证书组织为嵌套关键字，
// 而模型是扁平字段，通用树遍历写不到模型（encoding/json 静默丢弃）。
//   - `system health thresholds cpu-temp-celsius <n>` → SystemConfig.Health.{CPUTempCelsius,...}
//   - `system api tls cert-file|key-file|self-signed …` → SystemConfig.API（M5-8 使用）

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

var statementAliasesSystem = []aliasRule{
	// system management {interface|ip address|gateway} …（FR-SYS-001；FR-NET-002/FR-SEC-001 管理口身份）
	// 注：树里是 `management ip address`，模型是 `management.address`——多出的 `ip` 段
	// 此前无别名映射，导致 `set system management ip address …` 报「语句未产生配置变更」
	// （管理口 IP 在 CLI 上根本设不了，决策 #71）。
	{pattern: []string{"system", "management", "interface", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			mgmt := ensureObj(ensureObj(tree, "system"), "management")
			if !isSet {
				delete(mgmt, "interface")
				return nil
			}
			mgmt["interface"] = t[3]
			return nil
		}},
	// `delete system management ip address`（**不带值**，与 interface/gateway 的 delete 形态一致）：
	// 树里 `ip address` 比模型多一层 `ip`（模型是 `management.address`），无值形态若落回通用树遍历
	// 会去找不存在的 `system.management.ip` ⇒ 报「无匹配配置: ip」（决策 #379/R152-1）。
	// patternMatches 按长度精确匹配，故与下面的 5-token 带值形态互不干扰。
	{pattern: []string{"system", "management", "ip", "address"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			if isSet {
				return fmt.Errorf("缺少取值：set system management ip address <prefix>")
			}
			mgmt := ensureObj(ensureObj(tree, "system"), "management")
			delete(mgmt, "address")
			return nil
		}},
	{pattern: []string{"system", "management", "ip", "address", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			mgmt := ensureObj(ensureObj(tree, "system"), "management")
			if !isSet {
				delete(mgmt, "address")
				return nil
			}
			mgmt["address"] = t[4]
			return nil
		}},
	{pattern: []string{"system", "management", "gateway", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			mgmt := ensureObj(ensureObj(tree, "system"), "management")
			if !isSet {
				delete(mgmt, "gateway")
				return nil
			}
			mgmt["gateway"] = t[3]
			return nil
		}},
	// system health thresholds <cpu-temp-celsius|disk-temp-celsius|disk-used-percent> <n>
	{pattern: []string{"system", "health", "thresholds", "*", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			// CLI 路径为 system health thresholds …，须落在 system 对象下（模型 SystemConfig.Health）
			health := ensureObj(ensureObj(tree, "system"), "health")
			key := ""
			switch t[3] {
			case "cpu-temp-celsius":
				key = "cpu_temp_celsius"
			case "disk-temp-celsius":
				key = "disk_temp_celsius"
			case "disk-used-percent":
				key = "disk_used_percent"
			default:
				return errString("未知阈值: " + t[3])
			}
			if !isSet {
				delete(health, key)
				return nil
			}
			n, err := strconv.Atoi(t[4])
			if err != nil {
				return errString("阈值须为整数: " + t[4])
			}
			health[key] = n
			return nil
		}},
	// system syslog local <level|retention-days|max-size-mb> <v> → SyslogConfig 扁平字段（FR-SYS-004/013）
	{pattern: []string{"system", "syslog", "local", "*", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			syslog := ensureObj(ensureObj(tree, "system"), "syslog")
			key := ""
			switch t[3] {
			case "level":
				key = "level"
			case "retention-days":
				key = "retention_days"
			case "max-size-mb":
				key = "max_size_mb"
			default:
				return errString("未知 syslog local 参数: " + t[3])
			}
			if !isSet {
				delete(syslog, key)
				return nil
			}
			switch key {
			case "level":
				syslog[key] = t[4]
			default:
				n, err := strconv.Atoi(t[4])
				if err != nil {
					return errString("须为整数: " + t[4])
				}
				syslog[key] = n
			}
			return nil
		}},
	// system metrics history <interval|retention-days> <v> → MetricsHistoryConfig 扁平字段（决策 #356）
	// 字段名与叶子名不同（interval ⇄ interval_seconds、retention-days ⇄ retention_days），
	// 通用树遍历写不到模型；delete 即删键，回落默认值（默认值不在配置里写常数）。
	{pattern: []string{"system", "metrics", "history", "*", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			hist := ensureObj(ensureObj(ensureObj(tree, "system"), "metrics"), "history")
			key, err := metricsHistoryKey(t[3])
			if err != nil {
				return err
			}
			if !isSet {
				delete(hist, key)
				return nil
			}
			n, err := strconv.Atoi(t[4])
			if err != nil {
				return errString("须为整数: " + t[4])
			}
			hist[key] = n
			return nil
		}},
	// delete system metrics history <interval|retention-days>（**无取值**，回落默认；
	// 契约 §2.2 /《命令全表》的删除形态不带取值，故须单独一条 4-token 规则）→ 删键。
	{pattern: []string{"system", "metrics", "history", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			if isSet {
				return errString("metrics history 参数缺少取值: " + t[3])
			}
			hist := ensureObj(ensureObj(ensureObj(tree, "system"), "metrics"), "history")
			key, err := metricsHistoryKey(t[3])
			if err != nil {
				return err
			}
			delete(hist, key)
			return nil
		}},
	// system syslog host <ip> → remote_host（删除时一并清除该目标的其他参数）
	{pattern: []string{"system", "syslog", "host", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			syslog := ensureObj(ensureObj(tree, "system"), "syslog")
			if !isSet {
				delete(syslog, "remote_host")
				delete(syslog, "remote_port")
				delete(syslog, "facility")
				delete(syslog, "severity")
				return nil
			}
			syslog["remote_host"] = t[3]
			return nil
		}},
	// system syslog host <ip> [port <n>] [facility <f>] [severity <s>]（任意顺序、可组合）
	// → remote_host/remote_port/facility/severity（FR-SYS-004）
	// 注：此前仅 `port` 有映射，facility/severity 虽已在命令树与 CLI 契约中声明却未被
	// 执行器接受（契约与实现漂移），本次补齐为统一的不定长键值对解析（决策 #69）。
	{pattern: []string{"system", "syslog", "host", "*", "**"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			syslog := ensureObj(ensureObj(tree, "system"), "syslog")
			syslog["remote_host"] = t[3]
			tail := t[4:]
			if len(tail)%2 != 0 {
				return errString("system syslog host 参数须成对出现: " + strings.Join(tail, " "))
			}
			for i := 0; i < len(tail); i += 2 {
				key, val := tail[i], tail[i+1]
				switch key {
				case "port":
					if !isSet {
						delete(syslog, "remote_port")
						continue
					}
					n, err := strconv.Atoi(val)
					if err != nil {
						return errString("端口须为整数: " + val)
					}
					syslog["remote_port"] = n
				case "facility":
					if !isSet {
						delete(syslog, "facility")
						continue
					}
					if _, ok := model.FacilityCode(val); !ok {
						return errString("非法 facility: " + val)
					}
					syslog["facility"] = val
				case "severity":
					if !isSet {
						delete(syslog, "severity")
						continue
					}
					switch val {
					case "debug", "info", "warn", "error":
					default:
						return errString("severity 须为 debug|info|warn|error: " + val)
					}
					syslog["severity"] = val
				default:
					return errString("未知 system syslog host 参数: " + key)
				}
			}
			return nil
		}},
	// system api tls cert-file|key-file <path>；system api tls self-signed regenerate
	// 注意：更具体的 5-token 形态须排在带通配的 4-token 形态之前（matchAlias 取首个匹配）
	{pattern: []string{"system", "api", "tls", "self-signed", "regenerate"},
		apply: func(tree map[string]any, _ []string, isSet bool) error {
			api := ensureObj(ensureObj(tree, "system"), "api")
			if isSet {
				api["tls_self_signed"] = true
				return nil
			}
			delete(api, "tls_self_signed")
			return nil
		}},

	{pattern: []string{"system", "api", "tls", "*", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			api := ensureObj(ensureObj(tree, "system"), "api")
			if !isSet {
				switch t[3] {
				case "cert-file":
					delete(api, "cert_file")
				case "key-file":
					delete(api, "key_file")
				}
				return nil
			}
			switch t[3] {
			case "cert-file":
				api["cert_file"] = t[4]
			case "key-file":
				api["key_file"] = t[4]
			default:
				return errString("未知 TLS 参数: " + t[3])
			}
			return nil
		}},
	// delete system health thresholds（整段）
	{pattern: []string{"system", "health", "thresholds"},
		apply: func(tree map[string]any, _ []string, _ bool) error {
			if sys, ok := tree["system"].(map[string]any); ok {
				delete(sys, "health")
			}
			return nil
		}},
}

// metricsHistoryKey 历史时序存储叶子名 → MetricsHistoryConfig 字段名（决策 #356）；
// 未知叶子返回与 syslog 同风格的错误串。
func metricsHistoryKey(leaf string) (string, error) {
	switch leaf {
	case "interval":
		return "interval_seconds", nil
	case "retention-days":
		return "retention_days", nil
	}
	return "", errString("未知 metrics history 参数: " + leaf)
}
