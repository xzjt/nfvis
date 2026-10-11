package network

// 抓包会话的**持久记录**（决策 #466）：会话登记只在 nfvisd 进程内，重启后冻结/在抓会话
// 的登记就没了——数据文件在盘上却没有任何路径能导出它（VPP 侧还会被下一次 start 静默删掉）。
// 本文件把「会话是什么」落成一个小 JSON（按数据面分键），供启动收敛（Reconcile）把重启前
// 的会话重建为「已停止待导出」。
//
// 口径（与 #100 的 dpdk-bindings.json / #464 的 dpdk-native-drivers.json 同族）：
//   - **运行态事实**，不入 committed 配置；
//   - 但 capture-sessions.json 是「抓包文件」目录的索引，故**随 format-data 受管清单一起清**
//     （目录被清空后记录若还在，指向的必是死文件——同生共死）；
//   - 写失败不阻断抓包（退化为进程内登记＝原行为，重启后不可恢复），不用错误码打扰调用方。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultCaptureSessionsFile 会话记录落点（两个数据面共用一个文件、按 plane 分键）。
const DefaultCaptureSessionsFile = "/var/lib/nfvis/capture-sessions.json"

// 记录文件里两个数据面的键。
const (
	CapturePlaneVPP    = "vpp"
	CapturePlaneKernel = "kernel"
)

// captureWorkNameRe 工作/暂存文件命名：`.<导出终名>.part`（导出终名形如
// `nfvis-cap-<if>-<UTC>.pcap`）。隐藏 + `.part` 后缀即「不参与已导出清单」。
var captureWorkNameRe = regexp.MustCompile(`^\.(nfvis-cap-[^/\\]+\.pcap)\.part$`)

// CaptureWorkBase 若 name 是工作/暂存文件命名，返回其导出终名。
func CaptureWorkBase(name string) (string, bool) {
	m := captureWorkNameRe.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// RecoveredPcapName 裸残件（无源名可解析）归档用的文件名（按数据文件 mtime 生成）。
func RecoveredPcapName(mtime time.Time) string {
	return "nfvis-cap-recovered-" + mtime.UTC().Format("20060102T150405Z") + ".pcap"
}

// UniquePcapName 返回 dir 下不冲突的 pcap 文件名（base 冲突时在 .pcap 前顺延 `-r<n>`）。
func UniquePcapName(dir, base string) string {
	if !fileExists(filepath.Join(dir, base)) {
		return base
	}
	stem := strings.TrimSuffix(base, ".pcap")
	for i := 2; i < 100; i++ {
		name := stem + "-r" + strconv.Itoa(i) + ".pcap"
		if !fileExists(filepath.Join(dir, name)) {
			return name
		}
	}
	return base
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// CaptureSessionRecord 一个数据面的会话记录（JSON 形状即落盘形状）。
type CaptureSessionRecord struct {
	Interface string    `json:"interface"`
	Dev       string    `json:"dev,omitempty"` // 内核数据面：tcpdump -i 的目标设备名
	FileName  string    `json:"file_name"`     // 导出文件名（终名）
	WorkPath  string    `json:"work_path"`     // 工作文件当前路径（/tmp 或导出目录隐藏暂存位）
	StartedAt time.Time `json:"started_at"`
	Count     int       `json:"count,omitempty"`
	Stopped   bool      `json:"stopped,omitempty"`
	PID       int       `json:"pid,omitempty"` // 内核数据面：tcpdump 进程（孤儿判定用）
}

// LoadCaptureRecords 读记录文件（缺文件 → 空 map；解析失败 → 错误与空 map，调用方如实报告）。
func LoadCaptureRecords(path string) (map[string]*CaptureSessionRecord, error) {
	recs := map[string]*CaptureSessionRecord{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return recs, nil
		}
		return recs, err
	}
	if err := json.Unmarshal(data, &recs); err != nil {
		return map[string]*CaptureSessionRecord{}, err
	}
	return recs, nil
}

// SaveCaptureRecord 写 plane 键：rec==nil 即清该键。读-改-写 + 临时文件改名（原子）。
func SaveCaptureRecord(path, plane string, rec *CaptureSessionRecord) error {
	recs, err := LoadCaptureRecords(path)
	if err != nil {
		// 记录损坏：以本条为准重写（损坏内容已无恢复价值，且继续报错会让每次抓包都失败）。
		recs = map[string]*CaptureSessionRecord{}
	}
	if rec == nil {
		delete(recs, plane)
	} else {
		recs[plane] = rec
	}
	if len(recs) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(recs)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
