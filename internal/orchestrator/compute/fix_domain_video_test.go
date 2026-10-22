package compute

import (
	"strings"
	"testing"
)

// 缺陷（真机 v3.0.24，BUG-09）：缺省 `console serial` 生成的域 XML **没有显示设备**，
// Debian 13 / Ubuntu 云镜像（GRUB 用 `terminal_output gfxterm serial`）在 GRUB 阶段
// gfxterm 初始化失败 → 复位 → 无限重启；`virsh domstate` 恒 running、零告警。
// 因此**每一种控制台形态**都必须带一块可用的显示设备。
func TestFixAllConsoleFormsHaveDisplayDevice(t *testing.T) {
	cases := []struct{ name, console string }{
		{"empty", ""},
		{"serial", "serial"},
		{"serial-upper-and-spaces", "  SERIAL  "},
		{"telnet", "telnet"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := baseSpec()
			spec.Console = tc.console

			d, err := BuildDomain(spec)
			if err != nil {
				t.Fatalf("BuildDomain 失败: %v", err)
			}
			if len(d.Devices.Videos) != 1 {
				t.Fatalf("应恰好有一块显示设备，实际 %d 块", len(d.Devices.Videos))
			}
			if got := d.Devices.Videos[0].Model.Type; got != "vga" {
				t.Errorf("显示设备型号应为 vga（gfxterm 需要真实显示设备），实际 %q", got)
			}

			// 断言**生成的 XML**（不只是结构体）：libvirt 看到的就是这份文本。
			xml := mustXML(t, spec)
			assertContains(t, xml, `<video>`, `<model type="vga">`)
			assertNotContains(t, xml, `<model type="none">`)
		})
	}
}

// serial_console=false 只关产品侧的串口，与「guest 是否需要显示设备」是两件事：
// 把两者混在一起正是本缺陷的形态（无串口顺带无显卡 → guest 起不来）。
func TestFixSerialOffStillHasDisplayDevice(t *testing.T) {
	spec := baseSpec()
	off := false
	spec.VM.SerialConsole = &off

	xml := mustXML(t, spec)
	assertNotContains(t, xml, "<serial", "<console")
	assertContains(t, xml, `<model type="vga">`)
}

// 回归：console=vnc 仍是 vga（VNC 形态自己的既有行为不变）。
func TestFixVNCConsoleStillVGA(t *testing.T) {
	spec := baseSpec()
	spec.Console = "vnc"
	xml := mustXML(t, spec)

	assertContains(t, xml,
		`<graphics type="vnc"`,
		`listen="127.0.0.1"`,
		`<model type="vga">`,
	)
	assertNotContains(t, xml, `<model type="none">`, "<serial", "<console")
}

// 显示设备与串口/图形是正交的：serial 形态不应因为「无人值守」的旧注释而丢掉显卡，
// 也不应凭空多出 VNC 监听端口（只加显卡，不开放远程图形）。
func TestFixSerialDoesNotOpenVNC(t *testing.T) {
	spec := baseSpec()
	spec.Console = "serial"
	xml := mustXML(t, spec)

	assertContains(t, xml, `<serial type="pty">`, `<model type="vga">`)
	assertNotContains(t, xml, "<graphics")
	if strings.Count(xml, "<video>") != 1 {
		t.Fatalf("应只有一块显示设备:\n%s", xml)
	}
}
