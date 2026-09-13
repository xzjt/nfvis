package network

// govpp 真实连接实现（M3-1）。本文件是唯一 import govpp 的地方，上层只见
// Dialer/Session 接口；VPP 版本锁定与重连状态机在 vpp.go，便于用假实现单测。

import (
	"fmt"
	"time"

	"go.fd.io/govpp/adapter/socketclient"
	"go.fd.io/govpp/binapi/vpe"
	"go.fd.io/govpp/core"
)

// govppDialer 基于 govpp 的 Dialer 实现。
type govppDialer struct{}

// NewGovppDialer 返回 govpp 连接实现（NewManager 的缺省 dialer）。
func NewGovppDialer() Dialer { return govppDialer{} }

func (govppDialer) Dial(socket string, attempts int, interval time.Duration) (Session, <-chan Event, error) {
	client := socketclient.NewVppClient(socket)
	conn, evCh, err := core.AsyncConnect(client, attempts, interval)
	if err != nil {
		return nil, nil, err
	}
	events := make(chan Event, 8)
	go func() {
		defer close(events)
		for e := range evCh {
			events <- Event{State: mapConnState(e.State), Err: e.Error}
		}
	}()
	return &govppSession{conn: conn}, events, nil
}

func mapConnState(s core.ConnectionState) State {
	switch s {
	case core.Connected:
		return StateConnected
	case core.NotResponding:
		return StateNotResponding
	case core.Failed:
		return StateFailed
	default:
		return StateDisconnected
	}
}

type govppSession struct{ conn *core.Connection }

// Version 经 VPP binary API show_version 查询（FR-SYS-007 版本锁定的数据来源）。
func (s *govppSession) Version() (string, error) {
	ch, err := s.conn.NewAPIChannel()
	if err != nil {
		return "", err
	}
	defer ch.Close()
	reply := &vpe.ShowVersionReply{}
	if err := ch.SendRequest(&vpe.ShowVersion{}).ReceiveReply(reply); err != nil {
		return "", err
	}
	if reply.Retval != 0 {
		return "", fmt.Errorf("show_version 失败: retval=%d", reply.Retval)
	}
	return reply.Version, nil
}

func (s *govppSession) Disconnect() {
	if s.conn != nil {
		s.conn.Disconnect()
	}
}
