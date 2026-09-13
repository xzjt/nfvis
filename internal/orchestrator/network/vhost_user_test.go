package network

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/orchestrator"
)

// fakeVhost 假 VhostUserClient（M4-4 单测）。
type fakeVhost struct {
	byName map[string]uint32
	next   uint32
	names  map[uint32]string
	socks  map[uint32]string
	macs   map[uint32]string
	up     map[uint32]bool
	links  map[uint32]bool
	create []string
	del    []uint32
	err    error
}

func newFakeVhost() *fakeVhost {
	return &fakeVhost{byName: map[string]uint32{}, next: 10, names: map[uint32]string{},
		socks: map[uint32]string{}, macs: map[uint32]string{}, up: map[uint32]bool{}, links: map[uint32]bool{}}
}

func (f *fakeVhost) CreateVhostUser(sock string, isServer bool, tag string) (uint32, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.next++
	f.create = append(f.create, sock)
	id := f.next
	f.names[id] = "vhost-user" // VPP 默认名，待改名
	f.socks[id] = sock
	return id, nil
}
func (f *fakeVhost) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	if f.err != nil {
		return 0, false, f.err
	}
	idx, ok := f.byName[ifname]
	return idx, ok, nil
}
func (f *fakeVhost) VhostUserSocket(idx uint32) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	s, ok := f.socks[idx]
	return s, ok, nil
}
func (f *fakeVhost) SetInterfaceName(idx uint32, name string) error {
	f.names[idx] = name
	f.byName[name] = idx
	return nil
}
func (f *fakeVhost) SetInterfaceMAC(idx uint32, mac string) error { f.macs[idx] = mac; return nil }
func (f *fakeVhost) SetState(idx uint32, up bool) error           { f.up[idx] = up; return nil }
func (f *fakeVhost) InterfaceStatus(idx uint32) (bool, bool, bool, error) {
	if f.err != nil {
		return false, false, false, f.err
	}
	if _, ok := f.names[idx]; !ok {
		return false, false, false, nil
	}
	return f.up[idx], f.links[idx], true, nil
}
func (f *fakeVhost) DeleteVhostUser(idx uint32) error {
	f.del = append(f.del, idx)
	delete(f.names, idx)
	delete(f.socks, idx)
	for n, i := range f.byName {
		if i == idx {
			delete(f.byName, n)
		}
	}
	return nil
}
func (f *fakeVhost) Close() {}

func vhostProvider(f *fakeVhost) *VhostUserProvider {
	return NewVhostUserProviderFunc(func() (VhostUserClient, error) { return f, nil })
}

func TestVhostUserApplyCreatesNamesMacUp(t *testing.T) {
	f := newFakeVhost()
	p := vhostProvider(f)
	port := orchestrator.VnfPort{
		VM: "fw-vm", Interface: "eth0", Type: "vhost-user",
		Socket: "/run/nfvis/vhost/fw-vm-eth0.sock", MAC: "52:54:00:aa:bb:cc",
	}
	if err := p.Apply(context.Background(), port); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	name := orchestrator.VnfIfaceName("fw-vm", "eth0")
	idx, ok := f.byName[name]
	if !ok {
		t.Fatalf("接口应改名为 %s: %v", name, f.names)
	}
	if len(f.create) != 1 || f.create[0] != port.Socket {
		t.Fatalf("应以 socket 建接口: %v", f.create)
	}
	if f.macs[idx] != port.MAC || !f.up[idx] {
		t.Fatalf("应设 MAC 并 up: mac=%q up=%v", f.macs[idx], f.up[idx])
	}

	// 幂等：再次 Apply 不重复创建。
	if err := p.Apply(context.Background(), port); err != nil {
		t.Fatalf("重复 Apply: %v", err)
	}
	if len(f.create) != 1 {
		t.Fatalf("幂等不应重复创建: %v", f.create)
	}
}

func TestVhostUserApplyRejectsWrongTypeAndMissingSocket(t *testing.T) {
	p := vhostProvider(newFakeVhost())
	err := p.Apply(context.Background(), orchestrator.VnfPort{VM: "vm", Interface: "e", Type: "sriov-vf"})
	if err == nil || !strings.Contains(err.Error(), "非 vhost-user") {
		t.Fatalf("非 vhost-user 应报错: %v", err)
	}
	err = p.Apply(context.Background(), orchestrator.VnfPort{VM: "vm", Interface: "e", Type: "vhost-user"})
	if err == nil || !strings.Contains(err.Error(), "socket") {
		t.Fatalf("缺 socket 应报错: %v", err)
	}
}

func TestVhostUserDeleteIdempotent(t *testing.T) {
	f := newFakeVhost()
	p := vhostProvider(f)
	if err := p.Delete(context.Background(), "fw-vm", "eth0"); err != nil {
		t.Fatalf("删除不存在的接口应幂等: %v", err)
	}
	if len(f.del) != 0 {
		t.Fatalf("不应调用 delete: %v", f.del)
	}
	// 建后删。
	_ = p.Apply(context.Background(), orchestrator.VnfPort{VM: "fw-vm", Interface: "eth0", Type: "vhost-user", Socket: "/s"})
	if err := p.Delete(context.Background(), "fw-vm", "eth0"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(f.del) != 1 {
		t.Fatalf("应删除一次: %v", f.del)
	}
}

// FR-NET-023：链路状态随客户端连接驱动（无客户端 → down）。
func TestVhostUserLinkState(t *testing.T) {
	f := newFakeVhost()
	p := vhostProvider(f)
	if err := p.Apply(context.Background(), orchestrator.VnfPort{VM: "fw-vm", Interface: "eth0", Type: "vhost-user", Socket: "/s"}); err != nil {
		t.Fatal(err)
	}
	idx := f.byName[orchestrator.VnfIfaceName("fw-vm", "eth0")]

	exists, up, err := p.LinkState(context.Background(), "fw-vm", "eth0")
	if err != nil || !exists || up {
		t.Fatalf("VM 未连接应 exists=true up=false: exists=%v up=%v err=%v", exists, up, err)
	}
	f.links[idx] = true // 模拟 QEMU 连接后 link up
	if exists, up, _ = p.LinkState(context.Background(), "fw-vm", "eth0"); !exists || !up {
		t.Fatalf("客户端连接后应 up: exists=%v up=%v", exists, up)
	}
	if exists, _, _ = p.LinkState(context.Background(), "other-vm", "eth0"); exists {
		t.Fatal("未下发 vNIC 应 exists=false")
	}
}

func TestVhostUserClientUnavailable(t *testing.T) {
	p := NewVhostUserProviderFunc(func() (VhostUserClient, error) { return nil, errors.New("VPP 未连接") })
	if err := p.Apply(context.Background(), orchestrator.VnfPort{VM: "vm", Interface: "e", Type: "vhost-user", Socket: "/s"}); err == nil {
		t.Fatal("客户端不可用应报错")
	}
}
