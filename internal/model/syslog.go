package model

// FR-SYS-004（决策 #69）：远程 syslog 的 facility/severity 词汇表。
//
// 放在 model 的理由：这是**配置词汇**（CLI 枚举、commit 校验、OpenAPI 枚举同源），
// 且 model 是无内部依赖的叶子包——internal/system 已 import model，反向 import 会成环。
// RFC 5424 编号在此与名字同表，避免"名字集合"与"编号映射"两处漂移。

import "strings"

// SyslogFacilityCode facility 名 → RFC 5424 编号（名字须与 OpenAPI 枚举一致）。
var SyslogFacilityCode = map[string]int{
	"kern": 0, "user": 1, "mail": 2, "daemon": 3, "auth": 4, "syslog": 5,
	"lpr": 6, "news": 7, "uucp": 8, "cron": 9, "authpriv": 10, "ftp": 11,
	"local0": 16, "local1": 17, "local2": 18, "local3": 19,
	"local4": 20, "local5": 21, "local6": 22, "local7": 23,
}

// SyslogSeverityCode severity 名 → RFC 5424 编号（数值越小越严重）。
var SyslogSeverityCode = map[string]int{
	"emerg": 0, "alert": 1, "crit": 2, "error": 3, "err": 3,
	"warn": 4, "warning": 4, "notice": 5, "info": 6, "debug": 7,
}

// 远程 syslog 缺省值。
const (
	DefaultSyslogPort     = 514
	DefaultSyslogFacility = "user"
	DefaultSyslogSeverity = "info"
	DefaultSyslogNetwork  = "udp"
)

// FacilityCode 解析 facility 名（大小写不敏感；未知返回 false）。
func FacilityCode(name string) (int, bool) {
	c, ok := SyslogFacilityCode[strings.ToLower(strings.TrimSpace(name))]
	return c, ok
}

// SeverityCode 解析 severity 名（大小写不敏感；未知返回 false）。
func SeverityCode(name string) (int, bool) {
	c, ok := SyslogSeverityCode[strings.ToLower(strings.TrimSpace(name))]
	return c, ok
}
