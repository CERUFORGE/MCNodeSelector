package score

import (
	"testing"

	"mcnode/internal/config"
	"mcnode/internal/model"
)

// TestLinear 验证分段线性归一化的边界与中间值
func TestLinear(t *testing.T) {
	cases := []struct {
		v, ideal, max, want float64
	}{
		{0, 30, 300, 100},  // v ≤ ideal → 100
		{30, 30, 300, 100}, // v == ideal → 100
		{300, 30, 300, 0},  // v ≥ max → 0
		{500, 30, 300, 0},  // v > max → 0
		{165, 30, 300, 50}, // 中点 → 50
		{30, 30, 30, 100},  // max==ideal（异常配置）→ 二值，v<=ideal→100
		{40, 30, 30, 0},    // max==ideal 且 v>ideal → 0
	}
	for _, c := range cases {
		got := linear(c.v, c.ideal, c.max)
		if got != c.want {
			t.Errorf("linear(%g,%g,%g) = %g, want %g", c.v, c.ideal, c.max, got, c.want)
		}
	}
}

// TestMCVeto MC 一票否决：候选 MC 全失败 → Total=0、Eligible=false
func TestMCVeto(t *testing.T) {
	cfg := config.Default()
	cfg.Nodes = []config.Node{{Name: "测试", ServerAddr: "x", ServerPort: 7000, RemotePort: 25565, Weight: 1.0}}
	node := &cfg.Nodes[0]
	stats := &model.ProbeStats{MinMs: 30, AvgMs: 35, MaxMs: 50, P95Ms: 45, Received: 10, Sent: 10}
	mc := &model.MCResult{OK: false, ErrMsg: "握手失败"}
	res := Compute(node, stats, mc, 70, cfg)
	if res.Total != 0 || !res.VetoedByMC || res.Eligible {
		t.Errorf("一票否决应 Total=0/Veto=true/Eligible=false，got Total=%g Veto=%v Eligible=%v", res.Total, res.VetoedByMC, res.Eligible)
	}
}

// TestMCDisabledDropsFromDenominator MC 关闭时权重从分母剔除（动态重归一化）
func TestMCDisabledDropsFromDenominator(t *testing.T) {
	cfg := config.Default()
	cfg.Nodes = []config.Node{{Name: "测试", ServerAddr: "x", ServerPort: 7000, RemotePort: 25565, Weight: 1.0}}
	// 关闭 MC 验证
	f := false
	cfg.Probing.MCCheck.Enabled = &f
	node := &cfg.Nodes[0]
	stats := &model.ProbeStats{MinMs: 30, AvgMs: 35, MaxMs: 50, P95Ms: 45, Received: 10, Sent: 10}
	res := Compute(node, stats, nil, 70, cfg) // mc=nil 表示未验证
	if res.Total <= 0 {
		t.Errorf("MC 关闭且有正常测速时总分应 > 0，got %g", res.Total)
	}
}

// TestSpikePenalty 尖峰率越高总分越低（抗抖动：专挑不爱抽的节点）
func TestSpikePenalty(t *testing.T) {
	cfg := config.Default()
	f := false
	cfg.Probing.MCCheck.Enabled = &f
	node := &config.Node{Name: "n", ServerAddr: "x", ServerPort: 7000, RemotePort: 25565, Weight: 1.0}
	stable := &model.ProbeStats{MinMs: 30, AvgMs: 35, MaxMs: 60, P95Ms: 50, JitterMs: 5, LossPct: 0, SpikePct: 0, Received: 10, Sent: 10}
	spiky := &model.ProbeStats{MinMs: 30, AvgMs: 35, MaxMs: 268, P95Ms: 60, JitterMs: 5, LossPct: 0, SpikePct: 20, Received: 10, Sent: 10}
	a := Compute(node, stable, nil, 70, cfg)
	b := Compute(node, spiky, nil, 70, cfg)
	if !(a.Total > b.Total) {
		t.Errorf("尖峰率高应总分更低：stable=%.1f spiky=%.1f", a.Total, b.Total)
	}
}
