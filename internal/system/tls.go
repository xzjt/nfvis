// M5-8：TLS 证书管理与日志保留（FR-SYS-011/013）。
//
// 证书：
//   - Info(certPath)：解析 PEM 叶子证书（subject/issuer/有效期/是否自签），解析失败视为未配置。
//   - Install(certPEM, keyPEM)：校验证书与私钥匹配（公钥一致）后落盘（证书 0644、私钥 0600）。
//   - RegenerateSelfSigned(hostname)：生成自签证书（RSA 2048、SAN 含主机名与 IP、有效期 1 年）并落盘。
//     证书写哪些 IP **由本管理端按本机地址统一推导**（ServerCertSANs），调用方无从指定——
//     决策 #99 之前三处调用方各自传 ips（CLI 路径传 nil），重签出的证书缺回环 IP SAN，
//     而 nfvis-cli 缺省连 https://127.0.0.1（决策 #78），于是「重签」当场自毁管理路径（发现 #10）。
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
	"crypto/tls"
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
	"sync"
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

	mu     sync.Mutex
	listen string // 实际监听地址（host:port），用于推导自签证书 SAN（决策 #99）
}

// NewTLSManager 构造（dir 空取缺省）。
func NewTLSManager(dir string, run Runner) *TLSManager {
	if dir == "" {
		dir = DefaultTLSDir
	}
	return &TLSManager{dir: dir, run: run, now: time.Now}
}

// SetListen 记录实际监听地址（启动期设置，监听地址收敛后更新）。
// 仅影响**之后**生成的自签证书的 SAN（见 ServerCertSANs）。
func (m *TLSManager) SetListen(addr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listen = addr
}

// SANs 当前应写入自签证书的 SAN IP 列表（按本机监听地址与本机接口地址推导）。
func (m *TLSManager) SANs() []string {
	m.mu.Lock()
	listen := m.listen
	m.mu.Unlock()
	return ServerCertSANs(listen)
}

// CertPath / KeyPath 返回证书/私钥路径。
func (m *TLSManager) CertPath() string { return filepath.Join(m.dir, "server.crt") }
func (m *TLSManager) KeyPath() string  { return filepath.Join(m.dir, "server.key") }

// Info 读取证书信息；文件缺失或解析失败返回 ok=false。
//
// ⚠️ 它只回答「证书文件能不能解析」，**不回答「这对证书+私钥能不能用」**——私钥丢失/不匹配时
// 它照样返回 ok=true。判断可用性请用 PairUsable / Usable（修 R51-M11）。
func (m *TLSManager) Info() (TlsInfo, bool) {
	return InfoFile(m.CertPath())
}

// PairUsable 判定一对证书/私钥文件是否**成对可用**（能否一起被 crypto/tls 加载）——与 API 侧
// 建 HTTPS 服务用的是同一个判据（tls.LoadX509KeyPair）。
//
// 修 R51-M11：此前只判「证书能解析 **且** 两个路径都存在」（Info + os.Stat），于是私钥从未写出
// （Install/regenerate 里那两次独立的 os.WriteFile 之间的中间态）、私钥被换/被截断、或证书与
// 私钥本就不匹配时，装配层都判「有已装证书、用它」——而 API 侧加载失败，**每一次 TLS 握手都错**，
// 管理面彻底不可达，且没有明文回退、没有自愈（只能旁路救场）。
//
// 做成**方法**是必需的：装配层把可用性读口当作 `tlsCertSource` 的**硬成员**用（见
// cmd/nfvisd/tls_resolve.go，那里有 `var _ tlsCertSource = (*system.TLSManager)(nil)` 的编译期
// 断言）——漏实现即编译失败，不会出现「可选断言不上、严格判据从未生效而单测全绿」。
func (m *TLSManager) PairUsable(certPath, keyPath string) bool {
	_, err := tls.LoadX509KeyPair(certPath, keyPath)
	return err == nil
}

// Usable 判定磁盘上的服务证书是否可用：证书可解析（Info）**且**与私钥成对（PairUsable）。
// 半残（私钥丢失/不匹配）一律视为「没有可用证书」⇒ 调用方重新生成自签证书自愈。
func (m *TLSManager) Usable() (TlsInfo, bool) {
	info, ok := m.Info()
	if !ok || !m.PairUsable(m.CertPath(), m.KeyPath()) {
		return TlsInfo{}, false
	}
	return info, true
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
	if err := writePairAtomic(m.CertPath(), m.KeyPath(),
		normalizePEM("CERTIFICATE", cert.Raw), ensurePEMKey(keyBlock)); err != nil {
		return TlsInfo{}, fmt.Errorf("写入证书与私钥: %w", err)
	}
	info, ok := InfoFile(m.CertPath())
	if !ok {
		return TlsInfo{}, errors.New("安装后无法解析证书")
	}
	return info, nil
}

// regenerate 生成新的自签证书（hostname + 指定 IP SAN）并落盘。包内原语：
// **生产路径一律走 RegenerateSelfSigned**，避免调用方各自决定 SAN（决策 #99）。
func (m *TLSManager) regenerate(hostname string, ips []string) (TlsInfo, error) {
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
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return TlsInfo{}, err
	}
	if err := writePairAtomic(m.CertPath(), m.KeyPath(),
		normalizePEM("CERTIFICATE", der),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})); err != nil {
		return TlsInfo{}, fmt.Errorf("写入证书与私钥: %w", err)
	}
	info, ok := InfoFile(m.CertPath())
	if !ok {
		return TlsInfo{}, errors.New("安装后无法解析证书")
	}
	return info, nil
}

// RegenerateSelfSigned 重签自签证书：SAN 由本机监听地址统一推导（ServerCertSANs），
// 调用方无从指定（决策 #99）。**这是重签自签证书的唯一入口**——REST 与 CLI 两条路径
// 都经它，从而与启动期生成走同一份实现。
func (m *TLSManager) RegenerateSelfSigned(hostname string) (TlsInfo, error) {
	return m.regenerate(hostname, m.SANs())
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

// writePairAtomic 原子替换一对 PEM 文件（证书 0644、私钥 0600），修 R51-M11。
//
// 为什么必须原子：Install / regenerate 此前是两次独立的 os.WriteFile——换证窗口内一个正在建立
// 连接的读者会读到「新证书 + 旧私钥」这一对不匹配的文件（控制台的 GetCertificate 正是那样读的，
// round47 观测到一次 `x509: certificate signed by unknown authority`）。
//
// 步骤与崩溃安全：① 两个文件**都**先写成同目录下的临时文件——任一步失败即返回，旧的一对原封
// 不动（这是崩溃安全的关键：任何中途失败都不会留下半个新文件冒充完整证书）；② 逐个
// `os.Rename` 进位（同一文件系统内 rename 原子，读者绝不会看到半写文件）。
//
// 顺序：**先私钥、后证书**。证书是「已装证书」的身份基准（Info() 的指纹、日志与运维比对都用
// 它），把它放最后一步意味着换证窗口内读到的仍可能是**上一份完整安装**的那张证书；一旦读者恰好
// 看到「新私钥 + 旧证书」，这对不成对的文件会被 PairUsable/Usable 判为不可用 ⇒
// ResolveTLS/EnsureSelfSigned 重新生成自签证书把它修好，而不会把「新证书 + 旧私钥」当作已装证书
// 继续用（那正是本条缺陷的现场形态）。
func writePairAtomic(certPath, keyPath string, certPEM, keyPEM []byte) error {
	certTmp, err := writePEMTemp(filepath.Dir(certPath), filepath.Base(certPath), certPEM, 0o644)
	if err != nil {
		return err
	}
	keyTmp, err := writePEMTemp(filepath.Dir(keyPath), filepath.Base(keyPath), keyPEM, 0o600)
	if err != nil {
		_ = os.Remove(certTmp) // 还没动过正式文件：清掉临时件即回到旧的一对
		return err
	}
	if err := os.Rename(keyTmp, keyPath); err != nil {
		_ = os.Remove(certTmp)
		_ = os.Remove(keyTmp)
		return err
	}
	if err := os.Rename(certTmp, certPath); err != nil {
		_ = os.Remove(certTmp)
		return err
	}
	return nil
}

// writePEMTemp 在 dir 下写一个临时 PEM 文件（同目录 ⇒ rename 在同一文件系统内原子），
// 返回其路径。CreateTemp 建件即 0600，再按目标权限收口（证书 0644 / 私钥 0600）。
func writePEMTemp(dir, base string, data []byte, mode os.FileMode) (string, error) {
	f, err := os.CreateTemp(dir, "."+base+".*.tmp")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	if err := os.Chmod(name, mode); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
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

// EnsureSelfSigned 确保存在**可用**的服务证书：可用则原样返回，缺失或半残则生成自签证书。
//
// FR-SEC-004 / FR-API-001（决策 #72）：规格要求「REST over HTTPS（自签证书，可换）」，
// 而原实现未配置证书时直接以明文 HTTP 提供服务。改为默认自签，明文需显式开启。
// SAN 由 RegenerateSelfSigned 按本机监听地址推导（调用方不得指定，决策 #99）。
// 返回 generated 表示本次新建了证书。
//
// 修 R51-M11：判据从「证书存在且可解析（Info）」改为「证书与私钥**成对可用**（Usable）」——
// 私钥从未写出（两次独立 os.WriteFile 的中间态）或与证书不匹配时，旧判据原样返回那张永远握不上
// 手的证书，管理面每一次握手都错且不自愈；现在这类半残会被重签自签证书修好。
func (m *TLSManager) EnsureSelfSigned(hostname string) (info TlsInfo, generated bool, err error) {
	if existing, ok := m.Usable(); ok {
		return existing, false, nil
	}
	info, err = m.RegenerateSelfSigned(hostname)
	if err != nil {
		return TlsInfo{}, false, err
	}
	return info, true, nil
}
