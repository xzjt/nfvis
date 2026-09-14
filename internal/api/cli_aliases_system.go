package api

// 系统类配置语句别名（M5-5/M5-8）。
//
// 背景：与网络/计算类同因（见 cli_aliases_net.go 头部）——CLI 树把阈值/证书组织为嵌套关键字，
// 而模型是扁平字段，通用树遍历写不到模型（encoding/json 静默丢弃）。
//   - `system health thresholds cpu-temp-celsius <n>` → SystemConfig.Health.{CPUTempCelsius,...}
//   - `system api tls cert-file|key-file|self-signed …` → SystemConfig.API（M5-8 使用）

import "strconv"

var statementAliasesSystem = []aliasRule{
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
	// system syslog host <ip> [port <n>] → remote_host/remote_port
	{pattern: []string{"system", "syslog", "host", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			syslog := ensureObj(ensureObj(tree, "system"), "syslog")
			if !isSet {
				delete(syslog, "remote_host")
				delete(syslog, "remote_port")
				return nil
			}
			syslog["remote_host"] = t[3]
			return nil
		}},
	{pattern: []string{"system", "syslog", "host", "*", "port", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			syslog := ensureObj(ensureObj(tree, "system"), "syslog")
			if !isSet {
				delete(syslog, "remote_port")
				return nil
			}
			n, err := strconv.Atoi(t[5])
			if err != nil {
				return errString("端口须为整数: " + t[5])
			}
			syslog["remote_host"] = t[3]
			syslog["remote_port"] = n
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
