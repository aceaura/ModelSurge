// export_test.go 测试注入缝（仅测试二进制内存在，生产代码不可见）：
// kiro host/refresh URL 模板指针 + Manager 试探率。供 account_test 外部
// 测试（e2e_kiro_test.go）把流量指向 mock 服务。
package account

// 测试可替换的 kiro host/refresh 模板（取址暴露）。
var (
	RuntimeHostTemplate    = &runtimeHostTemplate
	QHostTemplate          = &qHostTemplate
	KiroRefreshURLTemplate = &kiroRefreshURLTemplate
)

// SetProbeRateForTest 注入熔断试探概率（0 = 冷却严格跳过，消随机性）。
func (m *Manager) SetProbeRateForTest(rate float64) { m.probeRate = rate }
