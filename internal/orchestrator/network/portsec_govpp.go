package network

// 决策 #389：端口安全的 govpp 薄适配。
//
// macip 消息属 acl 插件——客户端实现与 ACL 客户端**同一结构体**（govppAclClient），
// 三条端口安全消息的适配（MacipACLAddReplaceRules / MacipACLByTag / MacipBoundACL）
// 与 #341 的单规则封装同放 acl_govpp.go（同一批 binapi 消息、同一处 retval 口径）；
// 本文件只给工厂与编译期断言。依赖集中在 *_govpp.go（由真机集成测试覆盖，不入本地
// 覆盖率门槛），业务与规则构造在 portsec.go（纯函数，可跨平台单测）。

// PortSecClientFunc 返回随当前连接获取端口安全客户端的工厂（与 ACL 客户端同一通道，
// 独立工厂让 Provider 依赖最小能力集）。
func (m *Manager) PortSecClientFunc() func() (PortSecClient, error) {
	return func() (PortSecClient, error) {
		ch, err := m.APIChannel()
		if err != nil {
			return nil, err
		}
		return &govppAclClient{ch: ch}, nil
	}
}

// 编译期断言：govpp ACL 客户端实现端口安全能力集（决策 #389）。
var _ PortSecClient = (*govppAclClient)(nil)
