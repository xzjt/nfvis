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
	"os"
	"syscall"
)

// OpenConsole 打开域串口的 pty 作为双向流。
// 域未运行或未启用串口时无 pty 源，返回明确错误。
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
