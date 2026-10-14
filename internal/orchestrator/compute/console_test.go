package compute

import (
	"strings"
	"testing"
)

func TestSerialPtyPath(t *testing.T) {
	xml := `<domain type='kvm'><devices>
    <serial type='pty'>
      <source path='/dev/pts/3'/>
      <target type='isa-serial' port='0'/>
    </serial>
    <console type='pty' tty='/dev/pts/3'>
      <source path='/dev/pts/3'/>
    </console>
  </devices></domain>`
	got, err := SerialPtyPath(xml)
	if err != nil || got != "/dev/pts/3" {
		t.Fatalf("应解析出 /dev/pts/3: %q err=%v", got, err)
	}
	if _, err := SerialPtyPath(`<domain><devices></devices></domain>`); err == nil {
		t.Fatal("无串口应报错")
	}
	if _, err := SerialPtyPath(`<domain><devices><serial type='pty'><target port='0'/></serial></devices></domain>`); err == nil {
		t.Fatal("串口无 pty 源应报错")
	}
	if _, err := SerialPtyPath(""); err == nil || !strings.Contains(err.Error(), "串口") {
		t.Fatalf("空 XML 应报清晰错误: %v", err)
	}
}

// 控制台形态 telnet（决策 #485，Task 10）：`<serial type='tcp'>` 的访问目标是仅回环的
// host:service；pty 串口与非串口域都不是 tcp 源；tcp 但缺端口须如实报错（不猜端口）。
func TestSerialTCPAddr(t *testing.T) {
	telnet := `<domain type='kvm'><devices>
    <serial type='tcp'>
      <source mode='bind' host='127.0.0.1' service='6173'/>
      <protocol type='telnet'/>
      <target type='isa-serial' port='0'/>
    </serial>
    <console type='tcp'>
      <source mode='bind' host='127.0.0.1' service='6173'/>
    </console>
  </devices></domain>`
	addr, ok, err := SerialTCPAddr(telnet)
	if err != nil || !ok || addr != "127.0.0.1:6173" {
		t.Fatalf("telnet 串口应解析出 127.0.0.1:6173: %q ok=%v err=%v", addr, ok, err)
	}

	// pty 串口不是 tcp 源（走 SerialPtyPath 的既有路径）。
	pty := `<domain><devices><serial type='pty'><source path='/dev/pts/3'/></serial></devices></domain>`
	if addr, ok, err := SerialTCPAddr(pty); ok || err != nil || addr != "" {
		t.Fatalf("pty 串口不应被判为 tcp: %q ok=%v err=%v", addr, ok, err)
	}
	// 非 telnet 协议的 tcp 串口（如裸 tcp）：type='tcp' 仍按 tcp 拨号。
	raw := `<domain><devices><serial type='tcp'><source mode='bind' host='127.0.0.1' service='7000'/></serial></devices></domain>`
	if addr, ok, _ := SerialTCPAddr(raw); !ok || addr != "127.0.0.1:7000" {
		t.Fatalf("tcp 串口应解析出 127.0.0.1:7000: %q ok=%v", addr, ok)
	}
	// 无串口：不是 tcp 源（交给 SerialPtyPath 报「未配置串口」）。
	if _, ok, err := SerialTCPAddr(`<domain><devices></devices></domain>`); ok || err != nil {
		t.Fatalf("无串口不应判为 tcp: ok=%v err=%v", ok, err)
	}
	// tcp 但缺端口：如实报错。
	if _, ok, err := SerialTCPAddr(`<domain><devices><serial type='tcp'><source mode='bind' host='127.0.0.1'/></serial></devices></domain>`); ok || err == nil {
		t.Fatalf("tcp 串口缺端口应报错: ok=%v err=%v", ok, err)
	}
}

// 控制台形态 vnc（决策 #485）：从运行中的域 XML 读 `<graphics type='vnc'>` 的实际监听端口。
// 定义期 port='-1'、运行后 libvirt 规范化成真实端口；非 vnc 域不判为 vnc；vnc 但端口未分配须如实报错。
func TestVNCTCPAddr(t *testing.T) {
	// 运行后：libvirt 把 port='-1' 规范化成真实端口。
	running := `<domain type='kvm'><devices><graphics type='vnc' port='5901' autoport='yes' listen='127.0.0.1'>
      <listen type='address' address='127.0.0.1'/>
    </graphics></devices></domain>`
	addr, ok, err := VNCTCPAddr(running)
	if err != nil || !ok || addr != "127.0.0.1:5901" {
		t.Fatalf("vnc 应解析出 127.0.0.1:5901: %q ok=%v err=%v", addr, ok, err)
	}

	// 自闭合形式（无子元素）也应解析。
	selfClose := `<domain><devices><graphics type='vnc' port='5900' autoport='yes' listen='127.0.0.1'/></devices></domain>`
	if addr, ok, err := VNCTCPAddr(selfClose); err != nil || !ok || addr != "127.0.0.1:5900" {
		t.Fatalf("自闭合 vnc 应解析出 127.0.0.1:5900: %q ok=%v err=%v", addr, ok, err)
	}

	// 无 listen/address：回落 127.0.0.1。
	noListen := `<domain><devices><graphics type='vnc' port='5902' autoport='yes'/></devices></domain>`
	if addr, ok, err := VNCTCPAddr(noListen); err != nil || !ok || addr != "127.0.0.1:5902" {
		t.Fatalf("缺 listen 应回落 127.0.0.1:5902: %q ok=%v err=%v", addr, ok, err)
	}

	// spice 图形不是 vnc：不判为 vnc。
	spice := `<domain><devices><graphics type='spice' port='5930' autoport='yes'/></devices></domain>`
	if addr, ok, err := VNCTCPAddr(spice); ok || err != nil || addr != "" {
		t.Fatalf("spice 不应判为 vnc: %q ok=%v err=%v", addr, ok, err)
	}
	// 多种图形并存（spice 在前、vnc 在后）：取 vnc。
	mixed := `<domain><devices><graphics type='spice' port='5930'/><graphics type='vnc' port='5903' listen='127.0.0.1'/></devices></domain>`
	if addr, ok, err := VNCTCPAddr(mixed); err != nil || !ok || addr != "127.0.0.1:5903" {
		t.Fatalf("并存应取 vnc: %q ok=%v err=%v", addr, ok, err)
	}
	// 复现（多图形域）：前一个图形元素自闭合、后一个 vnc 元素带子元素（有 `</graphics>`）——
	// 旧实现把 block 无条件扩到「首个 `</graphics>`」，于是自闭合元素把后面的 vnc 元素一并吞掉，
	// idx 越过 vnc 后返回 not-vnc。正确行为：只界定「以本元素起始」的那一个元素。
	swallow := `<domain><devices>
    <graphics type='spice' port='5930' autoport='yes'/>
    <graphics type='vnc' port='5904' autoport='no' listen='127.0.0.1'>
      <listen type='address' address='127.0.0.1'/>
    </graphics>
  </devices></domain>`
	if addr, ok, err := VNCTCPAddr(swallow); err != nil || !ok || addr != "127.0.0.1:5904" {
		t.Fatalf("自闭合元素不应吞掉其后的 vnc 元素: %q ok=%v err=%v", addr, ok, err)
	}
	// 自闭合 vnc 元素本身在前、带子元素的 vnc 在后：取到后面的（带子元素那个）。
	selfThenChild := `<domain><devices><graphics type='vnc' port='5905' listen='127.0.0.1'/><graphics type='vnc' port='5906' listen='127.0.0.1'><listen type='address' address='127.0.0.1'/></graphics></devices></domain>`
	if addr, ok, err := VNCTCPAddr(selfThenChild); err != nil || !ok || addr != "127.0.0.1:5905" {
		t.Fatalf("应取第一个 vnc 元素（自闭合那个）: %q ok=%v err=%v", addr, ok, err)
	}

	// 无 graphics：不是 vnc（交给上层如实报「未配置 VNC」）。
	if _, ok, err := VNCTCPAddr(`<domain><devices></devices></domain>`); ok || err != nil {
		t.Fatalf("无图形设备不应判为 vnc: ok=%v err=%v", ok, err)
	}

	// vnc 但端口未分配（域未运行，port=-1）：如实报错，不猜端口。
	unstarted := `<domain><devices><graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1'/></devices></domain>`
	if addr, ok, err := VNCTCPAddr(unstarted); !ok || err == nil || addr != "" {
		t.Fatalf("vnc 端口未分配应报错: %q ok=%v err=%v", addr, ok, err)
	}
	// vnc 但完全无 port 属性：同样如实报错。
	if _, ok, err := VNCTCPAddr(`<domain><devices><graphics type='vnc' autoport='yes'/></devices></domain>`); !ok || err == nil {
		t.Fatalf("vnc 缺端口应报错: ok=%v err=%v", ok, err)
	}
}

// IsLoopbackAddr：仅回环地址（127.0.0.0/8、::1、localhost）为真；其余（含空 host、
// 非回环 IP、域名、无端口/非法 host:port）一律为假——OpenVNC 据此拒绝把字节流拨向外部。
func TestIsLoopbackAddr(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:5900", "127.0.0.5:5901", "[::1]:5900", "localhost:5900", "LOCALHOST:5900"} {
		if !IsLoopbackAddr(addr) {
			t.Errorf("%q 应判为回环", addr)
		}
	}
	for _, addr := range []string{
		"10.0.0.1:5900", "192.168.1.1:5900", "0.0.0.0:5900", "8.8.8.8:5900",
		"example.com:5900", ":5900", "127.0.0.1", "not-an-addr",
	} {
		if IsLoopbackAddr(addr) {
			t.Errorf("%q 不应判为回环", addr)
		}
	}
}
