package prober

import (
	"math"
	"testing"

	"mcnode/internal/model"
)

func approx(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// TestRobustDeSpike 单次高延迟尖峰不应拖垮均值/P95/抖动（启用稳健去尖峰时）
func TestRobustDeSpike(t *testing.T) {
	rtts := []float64{45, 47, 48, 50, 52, 53, 55, 60, 62, 300} // 最后一个为尖峰
	enabled := true
	rp := model.RobustParams{Enabled: enabled, MadK: 3.5, FloorMs: 30, TrimPct: 20}
	s := ComputeStats(rtts, 0, rp)

	if s.SpikeCount != 1 {
		t.Errorf("应识别出 1 个尖峰，got %d（SpikePct=%.1f%%）", s.SpikeCount, s.SpikePct)
	}
	if s.AvgMs > 70 {
		t.Errorf("去尖峰后均值应明显低于原始均值，got AvgMs=%.1f RawAvgMs=%.1f MaxMs=%.1f", s.AvgMs, s.RawAvgMs, s.MaxMs)
	}
	if s.P95Ms > 70 {
		t.Errorf("去尖峰后 P95 不应被 300ms 尖峰主导，got %.1f", s.P95Ms)
	}
	if s.MaxMs != 300 {
		t.Errorf("MaxMs 应保留原始最大值 300，got %.1f", s.MaxMs)
	}
	if !approx(s.RawAvgMs, 77.2, 0.5) {
		t.Errorf("原始均值应约 77.2，got %.1f", s.RawAvgMs)
	}
}

// TestRobustDisabled 关闭稳健去尖峰时，均值即为原始均值（向后兼容）
func TestRobustDisabled(t *testing.T) {
	rtts := []float64{45, 50, 55, 300}
	rp := model.RobustParams{Enabled: false}
	s := ComputeStats(rtts, 0, rp)
	want := (45.0 + 50 + 55 + 300) / 4
	if !approx(s.AvgMs, want, 1e-6) {
		t.Errorf("关闭稳健时 AvgMs 应为原始均值 %.1f，got %.1f", want, s.AvgMs)
	}
	if s.SpikeCount != 0 {
		t.Errorf("关闭稳健时不应统计尖峰，got %d", s.SpikeCount)
	}
}
