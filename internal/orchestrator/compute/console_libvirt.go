package compute

// 串口 console 接入（FR-CMP-014）。
//
// 实现选择：直接读写 libvirt 为域串口分配的 **pty**（domain XML
// `<serial type='pty'><source path='/dev/pts/N'/>`），pty 路径解析见 console.go。
//
// 为何不用 `DomainOpenConsoleBidirectional`：go-libvirt v0.0.0-20260814 的该流式
// RPC 在本环境实测阻塞在内部 channel 发送（console 打开即挂起，5 分钟超时），
// pty 直连语义等价（QEMU 持 master，客户端开 slave），且不依赖第三方流实现。
// nfvisd 以 root 运行，可打开 libvirt-qemu 所属 pty。
//
// 文件名 _libvirt.go → 覆盖率排除，由 nfvis-vm 集成测试覆盖。

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"time"
)

// OpenConsole 打开域串口的双向流。
// 域未运行或未启用串口时无串口源，返回明确错误。
//
// 控制台形态（决策 #485/Task 10）按**域 XML**分派：
//   - serial（缺省）：`<serial type='pty'>` ⇒ 打开 libvirt 分配的 pty（既有路径）；
//   - telnet：`<serial type='tcp'>`（仅回环）⇒ 直接拨该 TCP 端点，字节流语义与 pty 等价。
//
// **必须带 O_NOCTTY**（决策 #183）：nfvisd 由 systemd 拉起时是「无控制终端的会话首进程」，
// 此时打开 tty **不带** O_NOCTTY 会让它成为本进程的**控制终端**。此后该 pty 的 master
// （QEMU）一关闭——例如这台 VM 被 stop/destroy——内核就向会话前台进程组发 SIGHUP，
// **nfvisd 当场退出**（真机实测：console 用过之后，停 VM 会让守护进程死掉、请求方收到 EOF、
// systemd 再把它拉起来）。O_NOCTTY 让 pty 只是普通文件描述符，与终端语义彻底解耦。
func (c *Conn) OpenConsole(ctx context.Context, name string) (io.ReadWriteCloser, error) {
	xml, err := c.DumpXML(ctx, name)
	if err != nil {
		return nil, err
	}
	// telnet 控制台：串口是 tcp 源，拨它（地址从域 XML 读出，以域为单一事实源）。
	if addr, isTCP, aerr := SerialTCPAddr(xml); aerr != nil {
		return nil, fmt.Errorf("VM %s: %w", name, aerr)
	} else if isTCP {
		d := net.Dialer{Timeout: 5 * time.Second}
		conn, derr := d.DialContext(ctx, "tcp", addr)
		if derr != nil {
			return nil, fmt.Errorf("连接 VM %s 的 telnet 串口 %s: %w", name, addr, derr)
		}
		return conn, nil
	}
	pty, err := SerialPtyPath(xml)
	if err != nil {
		return nil, fmt.Errorf("VM %s: %w", name, err)
	}
	f, err := os.OpenFile(pty, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("打开 VM %s 串口 %s: %w", name, pty, err)
	}
	return f, nil
}
