package main

// 决策 #512：管理面证书解析（fail-closed）。
//
// 由来（round50 只读评审）：主装配在 `EnsureSelfSigned` 失败时只记一行 Error 日志——既不设
// 证书路径也不退出；随后 API 装配见证书路径为空即以**明文 HTTP** 服务，而日志却写着
// `-allow-plaintext`。管理员从未授权明文（TLS 目录不可建、磁盘满、权限被改均可触发），
// 口令与 Bearer token 会在管理网明文传输。
//
// 口径：证书准备（已装证书或自签）失败 ⇒ 启动失败并给出根因与出路；只有**显式**
// `-allow-plaintext` 才允许进入 HTTP 分支（该开关语义不变：给出即忽略已装/自签证书）。
// 这里把决策逻辑抽成一个不读全局 flag、只依赖注入能力的函数，便于逐条守护语义矩阵。

import (
	"fmt"
	"os"

	"github.com/xzjt/nfvis/internal/system"
)

// tlsCertSource 是 ResolveTLS 用到的证书管理器能力子集（*system.TLSManager 实现）。
// 抽成接口只为让解析逻辑可单测：真实实现要建目录、写文件，而这些正是「证书准备失败」
// 需要被模拟出来的路径。
type tlsCertSource interface {
	Info() (system.TlsInfo, bool)
	EnsureSelfSigned(hostname string) (system.TlsInfo, bool, error)
	CertPath() string
	KeyPath() string
}

// tlsSource 说明证书来自哪条路径（仅用于日志与测试断言）。
type tlsSource string

const (
	tlsSourceExplicit   tlsSource = "explicit"    // 命令行显式 -tls-cert/-tls-key
	tlsSourceInstalled  tlsSource = "installed"   // 磁盘上已安装的证书
	tlsSourceSelfSigned tlsSource = "self-signed" // 本次自动生成（或已存在）的自签证书
	tlsSourcePlaintext  tlsSource = "plaintext"   // 显式 -allow-plaintext（不启用 TLS）
)

// tlsMaterial 是解析结果：CertPath/KeyPath 为空表示不启用 TLS（仅明文分支）。
type tlsMaterial struct {
	CertPath    string
	KeyPath     string
	Source      tlsSource
	Fingerprint string // 自签证书指纹（仅 self-signed 路径有值，便于运维比对）
	Plaintext   bool
}

// ResolveTLS 决定管理面 API 使用哪份证书（决策 #512）。
//
// 语义矩阵（顺序即优先级）：
//
//	① 显式给了 -tls-cert/-tls-key → 用给定路径，不碰 TLSManager（半对配置一律拒绝）；
//	② plaintext=true（显式 -allow-plaintext）→ 不设证书路径、返回明文（不碰 TLSManager）；
//	③ 否则：已装可用的证书 → 用它；没有（或半残）→ 自动生成自签并用返回路径；
//	   自签失败、或最终仍取不到可读的 cert+key → 返回错误（**绝不**静默降级为明文）。
//
// fileExists 用于复核最终 cert/key 确实可读——这是 fail-closed 的最后一道；测试可注入假件，
// 生产注入 os.Stat 版实现（见 fileExists）。
func ResolveTLS(certFlag, keyFlag string, plaintext bool, mgr tlsCertSource, hostname string, fileExists func(string) bool) (tlsMaterial, error) {
	// ① 显式证书优先：把路径交给 API 即走 HTTPS。显式与 -allow-plaintext 同时给出时，显式
	//    证书胜出（与既有实现一致——明文只忽略「已装/自签」，不忽略显式证书）。
	if certFlag != "" || keyFlag != "" {
		if certFlag == "" || keyFlag == "" {
			return tlsMaterial{}, fmt.Errorf(
				"显式指定管理证书时 -tls-cert 与 -tls-key 必须成对给出（cert=%q key=%q）", certFlag, keyFlag)
		}
		if !fileExists(certFlag) {
			return tlsMaterial{}, fmt.Errorf("指定的管理证书不可读: %s（请确认路径与权限）", certFlag)
		}
		if !fileExists(keyFlag) {
			return tlsMaterial{}, fmt.Errorf("指定的管理私钥不可读: %s（请确认路径与权限）", keyFlag)
		}
		return tlsMaterial{CertPath: certFlag, KeyPath: keyFlag, Source: tlsSourceExplicit}, nil
	}
	// ② 显式明文：这是唯一允许进入 HTTP 分支的路径（warn 文案由装配层给出，保持逐字不变）。
	if plaintext {
		return tlsMaterial{Source: tlsSourcePlaintext, Plaintext: true}, nil
	}
	// ③ 已装证书（cert 与 key 都在）→ 直接用。
	if _, ok := mgr.Info(); ok {
		cert, key := mgr.CertPath(), mgr.KeyPath()
		if fileExists(cert) && fileExists(key) {
			return tlsMaterial{CertPath: cert, KeyPath: key, Source: tlsSourceInstalled}, nil
		}
		// 半残（如私钥丢失）：落到自签重建，不在这里就地返回错误。
	}
	// ③ 自签：缺失则生成（EnsureSelfSigned 遇已存在证书原样返回，不会覆盖）。
	info, _, err := mgr.EnsureSelfSigned(hostname)
	if err != nil {
		return tlsMaterial{}, fmt.Errorf(
			"管理证书不可用，且自动生成自签证书失败: %w。请用 set system api tls cert-file <证书> key-file <私钥> "+
				"安装管理证书；或以 -allow-plaintext 显式启动（仅限开发/测试）", err)
	}
	cert, key := mgr.CertPath(), mgr.KeyPath()
	if !fileExists(cert) || !fileExists(key) {
		return tlsMaterial{}, fmt.Errorf(
			"未能取得可用的管理证书（cert=%q key=%q 不可读）。请安装管理证书，或以 -allow-plaintext 显式启动（仅限开发/测试）",
			cert, key)
	}
	return tlsMaterial{CertPath: cert, KeyPath: key, Source: tlsSourceSelfSigned, Fingerprint: info.Fingerprint}, nil
}

// fileExists 复核路径存在且不是目录（生产注入给 ResolveTLS）。
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
