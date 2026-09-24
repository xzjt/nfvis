package model

// FR-SEC-007 / 决策 #25、#149：脱敏实现（RedactSensitive）的直接单测。
// 该实现原先只在 internal/api（配置视图回显），2026-09-24 下移到本包供诊断归档共用，
// 故判据与边界都在此锁定。

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactSensitiveRemovesOnlySensitiveLeaves(t *testing.T) {
	in := map[string]any{
		"system": map[string]any{
			"hostname": "sec-node",
			"login": map[string]any{
				"users": []any{
					map[string]any{"name": "admin", "class": "super-user", "password_hash": "pbkdf2$sha256$1$S$H"},
					map[string]any{"name": "ops", "class": "operator", "password-hash": "pbkdf2$sha256$1$S$H"},
				},
				"tokens": map[string]any{"token": "t0k3n", "secret": "s3cr3t", "psk": "psk1"},
			},
			"api": map[string]any{"cert_file": "/etc/nfvis/tls.crt", "key_file": "/etc/nfvis/tls.key"},
		},
		"virtual-machines": []any{
			map[string]any{"name": "vnf-a", "cloud_init": map[string]any{
				"ssh_keys": []any{"ssh-ed25519 AAAAC3 public"},
			}},
		},
	}
	out, err := json.Marshal(RedactSensitive(in))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, leak := range []string{"pbkdf2$", "password_hash", "password-hash", "t0k3n", "s3cr3t", "psk1"} {
		if strings.Contains(s, leak) {
			t.Fatalf("敏感字段 %q 未移除: %s", leak, s)
		}
	}
	// 脱敏 ≠ 掏空：非敏感字段（含 SSH **公**钥与私钥**路径**）必须还在
	for _, want := range []string{"sec-node", "admin", "super-user", "ops", "cert_file", "/etc/nfvis/tls.key", "ssh-ed25519 AAAAC3", "vnf-a"} {
		if !strings.Contains(s, want) {
			t.Fatalf("非敏感字段 %q 不应被移除: %s", want, s)
		}
	}
	// 原地不改入参（返回的是副本）
	b, _ := json.Marshal(in)
	if !strings.Contains(string(b), "pbkdf2$") {
		t.Fatalf("入参不应被就地修改: %s", b)
	}
}

// 键名判定与 CLI 风格等价（下划线/连字符、大小写），且只有敏感键受影响。
func TestRedactSensitiveKeyEquivalence(t *testing.T) {
	for _, k := range []string{"password_hash", "password-hash", "PASSWORD_HASH", "Token", "secret", "private_key", "PSK"} {
		if !IsSensitiveKey(k) {
			t.Fatalf("%q 应判为敏感", k)
		}
		out, _ := json.Marshal(RedactSensitive(map[string]any{k: "v"}))
		if string(out) != "{}" {
			t.Fatalf("键 %q 应被整个移除，实际 %s", k, out)
		}
	}
	for _, k := range []string{"name", "class", "retention_days", "public_key", "token_ttl_minutes"} {
		if IsSensitiveKey(k) {
			t.Fatalf("%q 不应判为敏感", k)
		}
	}
	// 不可序列化/非对象入参按兜底原样返回，不 panic
	if got := RedactSensitive(func() {}); got == nil {
		t.Fatal("兜底不应返回 nil")
	}
}
