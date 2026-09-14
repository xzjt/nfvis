//go:build linux

package metrics

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// HostMetrics 采集主机指标（FR-SYS-005：CPU/内存/磁盘/大页）。
// 读取失败的分项静默省略（不阻塞 /metrics 整体）。
func HostMetrics() []Sample {
	var out []Sample
	out = append(out, memInfoMetrics()...)
	out = append(out, cpuMetrics()...)
	out = append(out, diskMetrics("/")...)
	if up, ok := uptimeSeconds(); ok {
		out = append(out, Sample{Name: "nfvis_system_uptime_seconds", Help: "主机运行时长（秒）", Type: "gauge", Value: up})
	}
	return out
}

// uptimeSeconds 读取 /proc/uptime 的第一列（秒）。
func uptimeSeconds() (float64, bool) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func memInfoMetrics() []Sample {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil
	}
	defer f.Close()
	val := map[string]float64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		n, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		val[key] = n * 1024 // /proc/meminfo 以 kB 计
	}
	var out []Sample
	mapSample := func(name, help, key string) {
		if v, ok := val[key]; ok {
			out = append(out, Sample{Name: name, Help: help, Type: "gauge", Value: v})
		}
	}
	mapSample("nfvis_system_memory_total_bytes", "主机物理内存总量", "MemTotal")
	mapSample("nfvis_system_memory_available_bytes", "主机可用内存", "MemAvailable")
	mapSample("nfvis_system_hugepages_total", "1G/2M 大页总量（全部页规格合计，单位：页）", "HugePages_Total")
	mapSample("nfvis_system_hugepages_free", "空闲大页（单位：页）", "HugePages_Free")
	return out
}

func cpuMetrics() []Sample {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var total, idle float64
		for i, s := range fields[1:] {
			n, err := strconv.ParseFloat(s, 64)
			if err != nil {
				continue
			}
			total += n
			if i == 3 || i == 4 { // idle + iowait
				idle += n
			}
		}
		if total > 0 {
			return []Sample{{
				Name: "nfvis_system_cpu_utilization_ratio", Help: "主机 CPU 总体使用率（0-1）",
				Type: "gauge", Value: (total - idle) / total,
			}}
		}
	}
	return nil
}

func diskMetrics(path string) []Sample {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return nil
	}
	total := float64(st.Blocks) * float64(st.Bsize)
	free := float64(st.Bavail) * float64(st.Bsize)
	if total <= 0 {
		return nil
	}
	return []Sample{
		{Name: "nfvis_system_disk_total_bytes", Help: "根文件系统总容量", Type: "gauge", Value: total},
		{Name: "nfvis_system_disk_free_bytes", Help: "根文件系统可用容量", Type: "gauge", Value: free},
		{Name: "nfvis_system_disk_used_ratio", Help: "根文件系统使用率（0-1）", Type: "gauge", Value: (total - free) / total},
	}
}
