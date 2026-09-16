// M5-8：TLS 证书管理与日志保留（FR-SYS-011/013）。
//
// 证书：
//   - Info(certPath)：解析 PEM 叶子证书（subject/issuer/有效期/是否自签），解析失败视为未配置。
//   - Install(certPEM, keyPEM)：校验证书与私钥匹配（公钥一致）后落盘（证书 0644、私钥 0600）。
//   - Regenerate(hostname, ips)：生成自签证书（RSA 2048、SAN 含主机名与 IP、有效期 1 年）并落盘。
//   - RegenerateSSHHostKeys()：`ssh-keygen -A` 重生成缺失的 SSH host key（FR-SYS-011）。
//   - 临近过期（<30 天）由调用方（nfvisd 巡检）产生告警。
//
// 日志保留（FR-SYS-013）：`ApplyLogRetention` 写 systemd-journald drop-in 并触发重载
// （SystemMaxUse/SystemKeepFree/MaxRetentionSec 由 committed 的 syslog 本地策略换算）。
package system

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultTLSDir TLS 材料目录。
const DefaultTLSDir = "/var/lib/nfvis/tls"

// journaldDropInDir journald drop-in 目录（测试可替换）。
var journaldDropInDir = "/etc/systemd/journald.conf.d"

// CertExpiryWarnDays 证书临近过期告警阈值（天）。
const CertExpiryWarnDays = 30

// TlsInfo 证书信息（契约 TlsInfo）。
type TlsInfo struct {
	Subject     string    `json:"subject"`
	Issuer      string    `json:"issuer"`
	NotBefore   time.Time `json:"not_before"`
	NotAfter    time.Time `json:"not_after"`
	SelfSigned  bool      `json:"self_signed"`
	Fingerprint string    `json:"fingerprint,omitempty"` // SHA-256（运维比对用）
}

// TLSManager 证书管理。
type TLSManager struct {
	dir string // 材料目录
	run Runner
	now func() time.Time
}

// NewTLSManager 构造（dir 空取缺省）。
func NewTLSManager(dir string, run Runner) *TLSManager {
	if dir == "" {
		dir = DefaultTLSDir
	}
	return &TLSManager{dir: dir, run: run, now: time.Now}
}

// CertPath / KeyPath 返回证书/私钥路径。
func (m *TLSManager) CertPath() string { return filepath.Join(m.dir, "server.crt") }
func (m *TLSManager) KeyPath() string  { return filepath.Join(m.dir, "server.key") }

// Info 读取证书信息；文件缺失或解析失败返回 ok=false。
func (m *TLSManager) Info() (TlsInfo, bool) {
	return InfoFile(m.CertPath())
}

// InfoFile 解析指定证书文件的信息。
func InfoFile(path string) (TlsInfo, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return TlsInfo{}, false
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return TlsInfo{}, false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return TlsInfo{}, false
	}
	sum := sha256.Sum256(cert.Raw)
	return TlsInfo{
		Subject: cert.Subject.String(), Issuer: cert.Issuer.String(),
		NotBefore: cert.NotBefore.UTC(), NotAfter: cert.NotAfter.UTC(),
		SelfSigned:  cert.Issuer.String() == cert.Subject.String(),
		Fingerprint: strings.ToUpper(hex.EncodeToString(sum[:])),
	}, true
}

// Install 安装外部证书与私钥（PEM 文本），校验二者匹配后落盘（FR-SYS-011）。
func (m *TLSManager) Install(certPEM, keyPEM string) (TlsInfo, error) {
	certBlock, _ := pem.Decode([]byte(certPEM))
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return TlsInfo{}, errors.New("证书不是 PEM 格式的 CERTIFICATE")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return TlsInfo{}, fmt.Errorf("解析证书: %w", err)
	}
	keyBlock, _ := pem.Decode([]byte(keyPEM))
	if keyBlock == nil {
		return TlsInfo{}, errors.New("私钥不是 PEM 格式")
	}
	key, err := parsePrivateKey(keyBlock.Bytes)
	if err != nil {
		return TlsInfo{}, fmt.Errorf("解析私钥: %w", err)
	}
	if !publicKeysEqual(cert, key) {
		return TlsInfo{}, errors.New("证书与私钥不匹配")
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return TlsInfo{}, err
	}
	if err := os.WriteFile(m.CertPath(), normalizePEM("CERTIFICATE", cert.Raw), 0o644); err != nil {
		return TlsInfo{}, err
	}
	if err := os.WriteFile(m.KeyPath(), ensurePEMKey(keyBlock), 0o600); err != nil {
		return TlsInfo{}, err
	}
	info, ok := InfoFile(m.CertPath())
	if !ok {
		return TlsInfo{}, errors.New("安装后无法解析证书")
	}
	return info, nil
}

// Regenerate 生成新的自签证书（hostname/IP SAN），返回新证书信息。
func (m *TLSManager) Regenerate(hostname string, ips []string) (TlsInfo, error) {
	if hostname == "" {
		hostname = "nfvis"
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return TlsInfo{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return TlsInfo{}, err
	}
	now := m.now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hostname, Organization: []string{"NFViS"}},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{hostname, "localhost"},
	}
	for _, ip := range ips {
		if parsed := net.ParseIP(strings.TrimSpace(ip)); parsed != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, parsed)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return TlsInfo{}, err
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return TlsInfo{}, err
	}
	if err := os.WriteFile(m.CertPath(), normalizePEM("CERTIFICATE", der), 0o644); err != nil {
		return TlsInfo{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return TlsInfo{}, err
	}
	if err := os.WriteFile(m.KeyPath(), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return TlsInfo{}, err
	}
	info, ok := InfoFile(m.CertPath())
	if !ok {
		return TlsInfo{}, errors.New("安装后无法解析证书")
	}
	return info, nil
}

// RegenerateSSHHostKeys 重新生成 SSH host key（删除现有 ssh_host_* 后 ssh-keygen -A）。
func (m *TLSManager) RegenerateSSHHostKeys(ctx context.Context) error {
	if matches, _ := filepath.Glob("/etc/ssh/ssh_host_*"); len(matches) > 0 {
		for _, f := range matches {
			if strings.HasSuffix(f, ".pub") {
				continue // 公钥随私钥删除后由 ssh-keygen -A 重建
			}
			_ = os.Remove(f)
		}
	}
	if _, err := m.run(ctx, "ssh-keygen", "-A"); err != nil {
		return fmt.Errorf("ssh-keygen -A: %w", err)
	}
	return nil
}

// ExpiryAlarm 证书是否临近过期（返回剩余天数与是否需告警）。
func (m *TLSManager) ExpiryAlarm() (int, bool) {
	info, ok := m.Info()
	if !ok {
		return 0, false
	}
	days := int(time.Until(info.NotAfter).Hours() / 24)
	return days, days < CertExpiryWarnDays
}

// ApplyLogRetention 按 committed 本地日志策略写 journald drop-in 并重载（FR-SYS-013）。
// retentionDays/maxSizeMB 为 0 表示该维度不限制（不写入对应键）。
func ApplyLogRetention(ctx context.Context, run Runner, retentionDays, maxSizeMB int) (string, error) {
	if retentionDays == 0 && maxSizeMB == 0 {
		return "", nil
	}
	dir := journaldDropInDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建 journald drop-in 目录: %w", err)
	}
	var b strings.Builder
	b.WriteString("# 由 NFViS 生成（本地日志保留策略；请勿手工编辑）\n[Journal]\n")
	if maxSizeMB > 0 {
		fmt.Fprintf(&b, "SystemMaxUse=%dM\n", maxSizeMB)
	}
	if retentionDays > 0 {
		fmt.Fprintf(&b, "MaxRetentionSec=%dd\n", retentionDays)
	}
	path := filepath.Join(dir, "nfvis-retention.conf")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return path, err
	}
	// journald 收到 SIGHUP 重新读取配置（不中断日志）
	if _, err := run(ctx, "systemctl", "kill", "-s", "HUP", "systemd-journald"); err != nil {
		return path, fmt.Errorf("重载 journald: %w", err)
	}
	return path, nil
}

func parsePrivateKey(der []byte) (any, error) {
	if k, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return k, nil
	}
	return x509.ParseECPrivateKey(der)
}

// publicKeysEqual 判定证书公钥与私钥是否配对（注意 crypto.PublicKey 是命名类型，
// 不能用 `interface{ Public() any }` 断言）。
func publicKeysEqual(cert *x509.Certificate, key any) bool {
	signer, ok := key.(crypto.Signer)
	if !ok {
		return false
	}
	cp, ok := cert.PublicKey.(interface {
		Equal(x crypto.PublicKey) bool
	})
	if !ok {
		return false
	}
	return cp.Equal(signer.Public())
}

func normalizePEM(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

// ensurePEMKey 保留原私钥 PEM（含类型头），仅确保末尾换行。
func ensurePEMKey(block *pem.Block) []byte {
	return pem.EncodeToMemory(block)
}

// SortedIPs 去重排序（用于证书 SAN，保证可复现）。
func SortedIPs(ips []string) []string {
	set := map[string]bool{}
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		ip = strings.TrimSpace(ip)
		if ip == "" || set[ip] {
			continue
		}
		set[ip] = true
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}

// EnsureSelfSigned 确保存在可用的服务证书：已存在则原样返回，缺失则生成自签证书。
//
// FR-SEC-004 / FR-API-001（决策 #72）：规格要求「REST over HTTPS（自签证书，可换）」，
// 而原实现未配置证书时直接以明文 HTTP 提供服务。改为默认自签，明文需显式开启。
// 返回 generated 表示本次新建了证书。
func (m *TLSManager) EnsureSelfSigned(hostname string, ips []string) (info TlsInfo, generated bool, err error) {
	if existing, ok := m.Info(); ok {
		return existing, false, nil
	}
	info, err = m.Regenerate(hostname, ips)
	if err != nil {
		return TlsInfo{}, false, err
	}
	return info, true, nil
}
