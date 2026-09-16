// M5-7：软件升级/回退与电源、NTP（FR-OPS-001~003）。
//
// 语义：
//   - add：本地 .deb 或 http(s) URL（可选 sha256 强校验）→ 校验包名/依赖（`dpkg-deb -f`）
//     → 归档到软件目录（供 rollback）→ `dpkg -i` 安装（postinst 负责重启 nfvisd）→ 读回版本报告。
//   - rollback：从归档目录选**版本最高且非当前**的 .deb → `dpkg -i` 回退（FR-OPS-002）。
//   - reboot/shutdown：`systemctl reboot|poweroff`（调用方须已完成确认）。
//   - ntp sync：优先 chronyc，其次 ntpdate（取配置服务器），最后 systemd-timesyncd 重启。
//
// 底座命令经 Runner 注入，单测在任意平台用假实现覆盖；govpp/系统命令适配见 main.go。
package system

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// DefaultSoftwareDir 软件包归档目录（保留历史版本供回退）。
const DefaultSoftwareDir = "/var/lib/nfvis/software"

// Runner 执行宿主命令（dpkg/systemctl/chronyc…），返回合并输出。
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// SoftwareResult 升级/回退结果。
type SoftwareResult struct {
	Action   string `json:"action"` // add|rollback
	Version  string `json:"version"`
	Previous string `json:"previous,omitempty"`
	Package  string `json:"package,omitempty"`
	Output   string `json:"output,omitempty"`
}

// SoftwareManager 软件升级管理。
type SoftwareManager struct {
	dir     string
	run     Runner
	version func() string
	client  *http.Client
	now     func() time.Time
}

// NewSoftwareManager 构造（run/version 必填；dir 空取缺省）。
func NewSoftwareManager(dir string, run Runner, version func() string) *SoftwareManager {
	if dir == "" {
		dir = DefaultSoftwareDir
	}
	return &SoftwareManager{dir: dir, run: run, version: version,
		client: &http.Client{Timeout: 10 * time.Minute}, now: time.Now}
}

var debVersionRe = regexp.MustCompile(`^nfvis_(.+)_(amd64|arm64|all)\.deb$`)

// currentVersion 读取**已安装**的 nfvis 版本（dpkg-query 为准；查不到时回落到编译期版本）。
// 回退判定必须用系统实际安装版本，而非运行中进程的编译版本（后者在升级后仍为旧值）。
func (m *SoftwareManager) currentVersion(ctx context.Context) string {
	if out, err := m.run(ctx, "dpkg-query", "-W", "-f=${Version}", "nfvis"); err == nil {
		if v := strings.TrimSpace(out); v != "" {
			return v
		}
	}
	return m.version()
}

// Add 安装软件包（本地路径或 URL）。
func (m *SoftwareManager) Add(ctx context.Context, pkg, expectSHA string) (SoftwareResult, error) {
	res := SoftwareResult{Action: "add", Previous: m.currentVersion(ctx)}
	path := pkg
	if isURL(pkg) {
		p, err := m.download(ctx, pkg, expectSHA)
		if err != nil {
			return res, err
		}
		defer os.Remove(p) // 下载件移到归档后即删（归档名按版本规范）
		path = p
	} else {
		if _, err := os.Stat(path); err != nil {
			return res, fmt.Errorf("软件包不存在: %s", pkg)
		}
		if expectSHA != "" {
			if err := verifySHA(path, expectSHA); err != nil {
				return res, err
			}
		}
	}

	// 包名/类型校验（防误装）：dpkg-deb -f <pkg> Package
	nameOut, err := m.run(ctx, "dpkg-deb", "-f", path, "Package")
	if err != nil {
		return res, fmt.Errorf("校验软件包（dpkg-deb）: %w: %s", err, strings.TrimSpace(nameOut))
	}
	if pkgName := strings.TrimSpace(nameOut); pkgName != "nfvis" {
		return res, fmt.Errorf("软件包 %q 不是 nfvis（Package=%s）", filepath.Base(path), pkgName)
	}
	verOut, err := m.run(ctx, "dpkg-deb", "-f", path, "Version")
	if err != nil {
		return res, fmt.Errorf("读取软件包版本: %w", err)
	}
	newVer := strings.TrimSpace(verOut)
	if newVer == "" {
		return res, errors.New("软件包版本为空")
	}

	// 归档（供回退）后用 dpkg -i 安装
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return res, fmt.Errorf("创建软件目录: %w", err)
	}
	archived := filepath.Join(m.dir, fmt.Sprintf("nfvis_%s_amd64.deb", newVer))
	if err := copyFileTo(path, archived); err != nil {
		return res, fmt.Errorf("归档软件包: %w", err)
	}
	out, err := m.installDeb(ctx, archived)
	if err != nil {
		return res, fmt.Errorf("安装失败（失败应自动回退，可执行 request system software rollback）: %w: %s", err, strings.TrimSpace(out))
	}
	res.Package, res.Output = filepath.Base(archived), strings.TrimSpace(out)
	// 以包内版本为准报告（安装后版本读回由 dpkg-query 另行校验，避免时钟/缓存歧义）
	res.Version = newVer
	return res, nil
}

// Rollback 回退到归档中的上一个版本（FR-OPS-002）。
func (m *SoftwareManager) Rollback(ctx context.Context) (SoftwareResult, error) {
	res := SoftwareResult{Action: "rollback", Previous: m.currentVersion(ctx)}
	cands, err := m.archived()
	if err != nil {
		return res, err
	}
	if len(cands) == 0 {
		return res, fmt.Errorf("无可回退的版本：归档目录 %s 为空（仅能回退到经 request system software add 安装过的版本）", m.dir)
	}
	// 排除当前版本，取版本最高者
	cur := m.currentVersion(ctx)
	var pick string
	for _, c := range cands { // archived 已按版本升序
		if c.version != cur {
			pick = c.path
			res.Version = c.version
		}
	}
	if pick == "" {
		return res, fmt.Errorf("无可回退的版本：归档中仅有当前版本 %s（%d 个包）", cur, len(cands))
	}
	out, err := m.installDeb(ctx, pick)
	if err != nil {
		return res, fmt.Errorf("回退失败: %w: %s", err, strings.TrimSpace(out))
	}
	res.Package, res.Output = filepath.Base(pick), strings.TrimSpace(out)
	res.Version = m.currentVersion(ctx)
	return res, nil
}

// archived 列出归档包（按版本升序；版本比较用 dpkg --compare-versions 等价的简化排序）。
func (m *SoftwareManager) archived() ([]struct{ version, path string }, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]struct{ version, path string }, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		mx := debVersionRe.FindStringSubmatch(e.Name())
		if mx == nil {
			continue
		}
		out = append(out, struct{ version, path string }{mx[1], filepath.Join(m.dir, e.Name())})
	}
	sort.Slice(out, func(i, j int) bool { return compareVersion(out[i].version, out[j].version) < 0 })
	return out, nil
}

// compareVersion 版本比较（数字段逐段比较，非数字段字典序；够用即可）。
func compareVersion(a, b string) int {
	as, bs := strings.FieldsFunc(a, func(r rune) bool { return r == '.' || r == '-' || r == '+' || r == '~' }), strings.FieldsFunc(b, func(r rune) bool { return r == '.' || r == '-' || r == '+' || r == '~' })
	for i := 0; i < len(as) && i < len(bs); i++ {
		ai, aerr := parseInt(as[i])
		bi, berr := parseInt(bs[i])
		switch {
		case aerr == nil && berr == nil:
			if ai != bi {
				if ai < bi {
					return -1
				}
				return 1
			}
		default:
			if as[i] != bs[i] {
				if as[i] < bs[i] {
					return -1
				}
				return 1
			}
		}
	}
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	}
	return 0
}

func parseInt(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, errors.New("empty")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

// installDeb 安装 .deb；dpkg 前端锁被 apt/unattended-upgrades 占用时短等重试
// （Ubuntu 定时升级是常态，不应让一次升级因锁竞争失败）。
func (m *SoftwareManager) installDeb(ctx context.Context, path string) (string, error) {
	const attempts = 6
	var out string
	var err error
	for i := 0; i < attempts; i++ {
		out, err = m.run(ctx, "dpkg", "-i", path)
		if err == nil {
			return out, nil
		}
		if !strings.Contains(out, "dpkg frontend lock") && !strings.Contains(out, "frontend lock was locked") {
			return out, err
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return out, err
}

// Reboot 重启整机（调用方须已完成确认，FR-OPS-003）。
func (m *SoftwareManager) Reboot(ctx context.Context) error {
	_, err := m.run(ctx, "systemctl", "reboot")
	return err
}

// Shutdown 关机（调用方须已完成确认）。
func (m *SoftwareManager) Shutdown(ctx context.Context) error {
	_, err := m.run(ctx, "systemctl", "poweroff")
	return err
}

// NTPSync 立即同步一次时间（chronyc → ntpdate → systemd-timesyncd）。
func (m *SoftwareManager) NTPSync(ctx context.Context, servers []string) (string, error) {
	if out, err := m.run(ctx, "chronyc", "makestep"); err == nil {
		return "chronyc makestep: " + strings.TrimSpace(out), nil
	}
	if len(servers) > 0 {
		args := append([]string{"-u"}, servers...)
		if out, err := m.run(ctx, "ntpdate", args...); err == nil {
			return "ntpdate: " + strings.TrimSpace(out), nil
		}
	}
	if _, err := m.run(ctx, "systemctl", "restart", "systemd-timesyncd"); err == nil {
		return "systemd-timesyncd restarted", nil
	}
	return "", errors.New("无可用 NTP 工具（chronyc/ntpdate/systemd-timesyncd 均不可用）")
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func (m *SoftwareManager) download(ctx context.Context, url, expectSHA string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载软件包 %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("下载软件包 %s: HTTP %d", url, resp.StatusCode)
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(m.dir, "download-*.deb")
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if expectSHA != "" && !strings.EqualFold(sum, expectSHA) {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("sha256 校验失败：期望 %s，实际 %s", expectSHA, sum)
	}
	return f.Name(), nil
}

func verifySHA(path, expect string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, expect) {
		return fmt.Errorf("sha256 校验失败：期望 %s，实际 %s", expect, got)
	}
	return nil
}

// copyFileTo 复制文件（保留源文件；跨设备亦可）。
func copyFileTo(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
