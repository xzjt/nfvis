package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

type fakeSnapshots struct {
	rows    []SnapshotRow
	created []string
	revert  []string
	deleted []string
	err     error
}

func (f *fakeSnapshots) SnapshotCreate(_ context.Context, domain, name, desc string) error {
	if f.err != nil {
		return f.err
	}
	f.created = append(f.created, domain+"/"+name)
	f.rows = append(f.rows, SnapshotRow{Name: name, Description: desc})
	return nil
}
func (f *fakeSnapshots) Snapshots(context.Context, string) ([]SnapshotRow, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}
func (f *fakeSnapshots) SnapshotRevert(_ context.Context, domain, name string) error {
	if f.err != nil {
		return f.err
	}
	f.revert = append(f.revert, domain+"/"+name)
	return nil
}
func (f *fakeSnapshots) SnapshotDelete(_ context.Context, domain, name string) error {
	if f.err != nil {
		return f.err
	}
	f.deleted = append(f.deleted, domain+"/"+name)
	return nil
}

func TestSnapshotEndpoints(t *testing.T) {
	fs := &fakeSnapshots{}
	ts := newTestServerOpts(t, Options{VM: newFakeVM(), VMSnapshots: fs})
	token := loginAdmin(t, ts)
	seedVMPool(t, ts, token)
	cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		vmBody("fw-vm"), map[string]string{"X-NFVIS-Auto-Commit": "true"})

	// 创建
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm/snapshots",
		token, map[string]any{"name": "snap1", "description": "演示"}, nil)
	if status != http.StatusAccepted {
		t.Fatalf("创建快照应 202: %d %s", status, data)
	}
	if len(fs.created) != 1 || fs.created[0] != "fw-vm/snap1" {
		t.Fatalf("应创建 snap1: %v", fs.created)
	}
	// 非法名 → 400
	status, _, _ = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm/snapshots",
		token, map[string]any{"name": "bad name!"}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("非法快照名应 400: %d", status)
	}

	// 列表
	fs.rows = []SnapshotRow{{Name: "snap1", CreatedAt: ptrTime(time.Now().UTC())}}
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm/snapshots", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), `"snap1"`) {
		t.Fatalf("列表: %d %s", status, data)
	}

	// 回滚
	status, _, data = cfgRequest(t, http.MethodPost,
		ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm/snapshots/snap1:rollback", token, nil, nil)
	if status != http.StatusAccepted {
		t.Fatalf("回滚应 202: %d %s", status, data)
	}
	if len(fs.revert) != 1 {
		t.Fatalf("应回滚: %v", fs.revert)
	}

	// 删除
	status, _, data = cfgRequest(t, http.MethodDelete,
		ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm/snapshots/snap1", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("删除应 200: %d %s", status, data)
	}
	if len(fs.deleted) != 1 {
		t.Fatalf("应删除: %v", fs.deleted)
	}

	// 审计
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/audit-logs?limit=50", token, nil, nil)
	if status != http.StatusOK {
		t.Fatal(status)
	}
	for _, want := range []string{"vm.snapshot.create", "vm.snapshot.rollback", "vm.snapshot.delete"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("审计应含 %s: %s", want, data)
		}
	}
}

func TestSnapshotErrors(t *testing.T) {
	// 未装配 → 503
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	status, _, _ := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm/snapshots", token, nil, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("未装配应 503: %d", status)
	}

	// VM 不存在 → 404
	fs := &fakeSnapshots{}
	ts2 := newTestServerOpts(t, Options{VM: newFakeVM(), VMSnapshots: fs})
	token2 := loginAdmin(t, ts2)
	status, _, _ = cfgRequest(t, http.MethodGet, ts2.URL+APIPrefix+"/virtual-machine-functions/ghost/snapshots", token2, nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("VM 不存在应 404: %d", status)
	}

	// 未知动作 → 404
	seedVMPool(t, ts2, token2)
	cfgRequest(t, http.MethodPost, ts2.URL+APIPrefix+"/virtual-machine-functions", token2,
		vmBody("fw-vm"), map[string]string{"X-NFVIS-Auto-Commit": "true"})
	status, _, _ = cfgRequest(t, http.MethodPost,
		ts2.URL+APIPrefix+"/virtual-machine-functions/fw-vm/snapshots/snap1:fly", token2, nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("未知快照动作应 404: %d", status)
	}

	// 运行时错误 → 500，且记 failure 审计
	fs.err = fmt.Errorf("libvirt 失败")
	status, _, _ = cfgRequest(t, http.MethodPost, ts2.URL+APIPrefix+"/virtual-machine-functions/fw-vm/snapshots",
		token2, map[string]any{"name": "s2"}, nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("运行时错误应 500: %d", status)
	}
	_, _, data := cfgRequest(t, http.MethodGet, ts2.URL+APIPrefix+"/audit-logs?limit=50", token2, nil, nil)
	var rows []map[string]any
	_ = json.Unmarshal(data, &rows)
	foundFail := false
	for _, r := range rows {
		if r["action"] == "vm.snapshot.create" && r["result"] == "failure" {
			foundFail = true
		}
	}
	if !foundFail {
		t.Errorf("失败快照应记 failure 审计: %s", data)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
