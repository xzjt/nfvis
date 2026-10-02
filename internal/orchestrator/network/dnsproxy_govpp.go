package network

// govpp punt socket 客户端（决策 #345）：唯一使用 punt binapi 的地方。
//
// binapi（govpp v0.13.0，`go doc go.fd.io/govpp/binapi/punt` 实测字段）：
//   - punt_socket_register   { HeaderVersion u32; Punt punt; Pathname string[108] }
//   - punt_socket_deregister { Punt punt }
//   - punt = { PuntType type; PuntUnion punt }；L4 联合体 = { Af u8; Protocol u8; Port u16 }
//
// 语义（round124 真机实证，见 docs/evidence/v2-round124-d345-dns-punt-spike.txt）：
//   - 注册实现是 `udp_register_dst_port`——只对「目的地址 ∈ VPP 本机」的 UDP/<port> 生效；
//   - `punt { socket <path> }` 必须在 startup.conf 里配置，否则注册被 VPP 以 "socket is not
//     configured" 拒绝（运行期无 API 可补）；由 startup.go 生成器写入；
//   - Pathname 是 **nfvisd 自己的 client socket**（VPP 用 sendmsg 把上行包发到该地址）。

import (
	"fmt"

	"go.fd.io/govpp/api"
	"go.fd.io/govpp/binapi/ip_types"
	"go.fd.io/govpp/binapi/punt"
)

// PuntClientFunc 返回随当前连接获取 punt 客户端的工厂。
func (m *Manager) PuntClientFunc() func() (PuntClient, error) {
	return func() (PuntClient, error) {
		ch, err := m.APIChannel()
		if err != nil {
			return nil, err
		}
		return &govppPuntClient{ch: ch}, nil
	}
}

type govppPuntClient struct{ ch api.Channel }

func (g *govppPuntClient) Close() { g.ch.Close() }

// puntL4 ipv4/udp/<port> 的 punt 描述（注册与注销共用）。
func puntL4(port uint16) punt.Punt {
	return punt.Punt{
		Type: punt.PUNT_API_TYPE_L4,
		Punt: punt.PuntUnionL4(punt.PuntL4{
			Af:       ip_types.ADDRESS_IP4,
			Protocol: ip_types.IP_API_PROTO_UDP,
			Port:     port,
		}),
	}
}

func (g *govppPuntClient) Register(clientPath string, port uint16) error {
	reply := &punt.PuntSocketRegisterReply{}
	if err := g.ch.SendRequest(&punt.PuntSocketRegister{
		HeaderVersion: 1, // punt 元数据头版本（当前 1）
		Punt:          puntL4(port),
		Pathname:      clientPath,
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("punt_socket_register(ipv4 udp %d socket %s) retval=%d", port, clientPath, reply.Retval)
	}
	return nil
}

func (g *govppPuntClient) Deregister(port uint16) error {
	reply := &punt.PuntSocketDeregisterReply{}
	if err := g.ch.SendRequest(&punt.PuntSocketDeregister{Punt: puntL4(port)}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("punt_socket_deregister(ipv4 udp %d) retval=%d", port, reply.Retval)
	}
	return nil
}
