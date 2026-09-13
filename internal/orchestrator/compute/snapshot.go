package compute

// 快照模型与 XML 组装（FR-CMP-015）。qcow2 内部快照，含全部磁盘（根盘 + 数据盘）。

import (
	"fmt"
	"strings"
	"time"

	"libvirt.org/go/libvirtxml"
)

// SnapshotInfo 快照元数据（契约 components/schemas/Snapshot）。
// SizeBytes 在 libvirt 不提供时记 0（契约可选字段）。
type SnapshotInfo struct {
	Name        string    `json:"name"`
	CreatedAt   time.Time `json:"created_at,omitempty"`
	SizeBytes   int64     `json:"size_bytes,omitempty"`
	Description string    `json:"description,omitempty"`
}

// BuildSnapshotXML 生成 domain snapshot XML：
// 内部快照（qcow2），显式列出全部磁盘，保证数据盘一并纳入（FR-CMP-018）。
func BuildSnapshotXML(name, description string, diskTargets []string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("快照名不能为空")
	}
	snap := libvirtxml.DomainSnapshot{Name: name, Description: description}
	if len(diskTargets) > 0 {
		disks := &libvirtxml.DomainSnapshotDisks{}
		for _, d := range diskTargets {
			disks.Disks = append(disks.Disks, libvirtxml.DomainSnapshotDisk{Name: d, Snapshot: "internal"})
		}
		snap.Disks = disks
	}
	return snap.Marshal()
}

// SnapshotInfoFromXML 解析 libvirt 快照 XML（纯函数，单测覆盖）。
// creationTime 为 Unix 秒字符串；无法解析时留零值。
func SnapshotInfoFromXML(doc string) (SnapshotInfo, error) {
	var snap libvirtxml.DomainSnapshot
	if err := snap.Unmarshal(doc); err != nil {
		return SnapshotInfo{}, fmt.Errorf("解析快照 XML: %w", err)
	}
	info := SnapshotInfo{Name: snap.Name, Description: snap.Description}
	if snap.CreationTime != "" {
		var sec int64
		if _, err := fmt.Sscanf(snap.CreationTime, "%d", &sec); err == nil && sec > 0 {
			info.CreatedAt = time.Unix(sec, 0).UTC()
		}
	}
	return info, nil
}

// DiskTargetsOf 从 domain XML 提取磁盘 target dev（vda/vdb… 仅 device='disk'），
// 用于快照包含哪些盘。纯函数，便于单测。
func DiskTargetsOf(domainXML string) []string {
	var out []string
	rest := domainXML
	for {
		i := strings.Index(rest, "<disk ")
		if i < 0 {
			break
		}
		rest = rest[i:]
		end := strings.Index(rest, "</disk>")
		if end < 0 {
			break
		}
		block := rest[:end]
		rest = rest[end:]
		if !strings.Contains(block, `device='disk'`) {
			continue
		}
		if j := strings.Index(block, `<target dev='`); j >= 0 {
			v := block[j+len(`<target dev='`):]
			if k := strings.Index(v, "'"); k > 0 {
				out = append(out, v[:k])
			}
		}
	}
	return out
}
