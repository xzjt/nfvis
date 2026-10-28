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
//
// ⚠️ `Usable` / `PairUsable` 是**硬成员**（修 R51-M11；本轮收口）：可用性判据必须由真实实现
// 提供——缺了它是**编译失败**，而不是「断言不上就静默走旧口径」。可选能力断言
// （`mgr.(interface{ … })`）在本仓是有前科的：生产适配器漏实现时不报错、也不被任何单测覆盖，
// 严格路径于是从未在真机跑过（round49/#521 同族）。末行有编译期断言把它钉住。
type tlsCertSource interface {
	// Info 证书详情（subject/issuer/有效期/指纹）。ResolveTLS 判**可用性**走 Usable（私钥丢失/
	// 不匹配时 Info 仍答 ok，这正是本条缺陷）；保留它是因为它是测试替身侧唯一可按调用计数证明
	// 「该分支完全不碰 TLSManager」的观测点（round50 #512 矩阵的既有断言按 infoCalls 计数）。
	Info() (system.TlsInfo, bool)
	// Usable 证书可解析**且**与私钥成对可用（缺一即「没有可用证书」，调用方据此重建自签）。
	Usable() (system.TlsInfo, bool)
	// PairUsable 给定的一对路径是否成对可用（显式 -tls-cert/-tls-key 分支走它）。
	PairUsable(certPath, keyPath string) bool
	EnsureSelfSigned(hostname string) (system.TlsInfo, bool, error)
	CertPath() string
	KeyPath() string
}

// 编译期断言：生产实现必须完整满足该能力面（漏一个方法即在此处编译失败，而非运行期静默降级）。
var _ tlsCertSource = (*system.TLSManager)(nil)

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
//	① 显式给了 -tls-cert/-tls-key → 用给定路径，不碰 TLSManager（半对配置、路径不可读、
//	   R51-M11 起还包括「两者不成对」——一律拒绝）；
//	② plaintext=true（显式 -allow-plaintext）→ 不设证书路径、返回明文（不碰 TLSManager）；
//	③ 否则：已装**可用**的证书（cert+key 成对可加载）→ 用它；没有或半残 → 自动生成自签并用
//	   返回路径；自签失败、或最终仍取不到可读的 cert+key → 返回错误（**绝不**静默降级为明文）。
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
		// 修 R51-M11：两个文件「在」不等于「能用」——路径可读只是第一关，成对加载才是真判据
		// （只 stat 的话，一个不懂的证书/私钥组合会被当成可用，随后 API 侧加载失败、
		// 每一次握手都错）。文案与既有两条分开：那两条说的是「读不到」，这条说的是「读到了但不配对」。
		if !mgr.PairUsable(certFlag, keyFlag) {
			return tlsMaterial{}, fmt.Errorf(
				"指定的管理证书与私钥不成对（cert=%q key=%q）：两者无法一起加载，HTTPS 握手会全部失败。"+
					"请确认它们来自同一次签发（或以 -allow-plaintext 显式启动，仅限开发/测试）", certFlag, keyFlag)
		}
		return tlsMaterial{CertPath: certFlag, KeyPath: keyFlag, Source: tlsSourceExplicit}, nil
	}
	// ② 显式明文：这是唯一允许进入 HTTP 分支的路径（warn 文案由装配层给出，保持逐字不变）。
	if plaintext {
		return tlsMaterial{Source: tlsSourcePlaintext, Plaintext: true}, nil
	}
	// ③ 已装**可用**证书（cert 与 key 成对可加载）→ 直接用。
	//
	// 修 R51-M11：判据是可用性而不是存在性。半残（私钥丢失/与证书不匹配）在此**有意**不返回错误，
	// 而是落到下面的自签重建——既有口径即如此（见本条注释末），且它会自愈：管理面不会被一张
	// 永远握不上手的证书锁死。
	if _, usable := mgr.Usable(); usable {
		return tlsMaterial{CertPath: mgr.CertPath(), KeyPath: mgr.KeyPath(), Source: tlsSourceInstalled}, nil
	}
	// ③ 自签：缺失或半残则生成（EnsureSelfSigned 遇**可用**的证书原样返回，不会覆盖）。
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
