package switcher

import (
	"testing"
	"time"

	"mcnode/internal/config"
	"mcnode/internal/model"
)

// TestFourGates 四道闸门：候选优势不足 → 不切；连续满足 3 轮 → 切
func TestFourGates(t *testing.T) {
	cfg := config.Default()
	s := New(func() *config.Config { return cfg })
	now := time.Now()
	// 提前清空 lastSwitch（让它处于“可切”初始态）
	s.lastSwitch = now.Add(-2 * time.Hour) // 远早于驻留
	s.cooldownUntil = now.Add(-time.Hour)  // 已过冷却

	cur := &model.NodeRuntime{Name: "当前", Score: &model.ScoreResult{Total: 50, Eligible: true}}
	cand := &model.NodeRuntime{Name: "候选", Score: &model.ScoreResult{Total: 80, Eligible: true}} // 80 > 50+15

	// 连续 3 轮优势，前 2 轮应不切，第 3 轮切
	for i := 1; i <= 2; i++ {
		d := s.Evaluate(cur, cand)
		if d.Switch {
			t.Errorf("第 %d 轮不应切换（连胜不足），原因：%s", i, d.Reason)
		}
	}
	d := s.Evaluate(cur, cand)
	if !d.Switch || d.Channel != "B" {
		t.Errorf("第 3 轮应切换（通道B），got Switch=%v Channel=%s 原因：%s", d.Switch, d.Channel, d.Reason)
	}
}

// TestAdvantageMargin 优势不足（cand <= cur+margin）→ 连胜清零、不切
func TestAdvantageMargin(t *testing.T) {
	cfg := config.Default()
	s := New(func() *config.Config { return cfg })
	s.lastSwitch = time.Now().Add(-2 * time.Hour)
	s.cooldownUntil = time.Now().Add(-time.Hour)

	cur := &model.NodeRuntime{Name: "当前", Score: &model.ScoreResult{Total: 50, Eligible: true}}
	cand := &model.NodeRuntime{Name: "候选", Score: &model.ScoreResult{Total: 60, Eligible: true}} // 60 <= 50+15
	d := s.Evaluate(cur, cand)
	if d.Switch {
		t.Errorf("优势不足不应切换：%s", d.Reason)
	}
	if s.consecutiveWins != 0 {
		t.Errorf("优势不足应清零连胜，got %d", s.consecutiveWins)
	}
}

// TestCooldown 冷却期内不切
func TestCooldown(t *testing.T) {
	cfg := config.Default()
	s := New(func() *config.Config { return cfg })
	s.lastSwitch = time.Now().Add(-2 * time.Hour)
	s.cooldownUntil = time.Now().Add(5 * time.Minute) // 还在冷却
	cur := &model.NodeRuntime{Name: "当前", Score: &model.ScoreResult{Total: 50, Eligible: true}}
	cand := &model.NodeRuntime{Name: "候选", Score: &model.ScoreResult{Total: 90, Eligible: true}}
	for i := 0; i < 3; i++ { // 连续 3 轮仍不切
		d := s.Evaluate(cur, cand)
		if d.Switch {
			t.Errorf("冷却期内不应切换：%s", d.Reason)
		}
	}
}

// TestMinDwell 最小驻留未满不切
func TestMinDwell(t *testing.T) {
	cfg := config.Default()
	s := New(func() *config.Config { return cfg })
	s.lastSwitch = time.Now().Add(-1 * time.Minute) // 距上次切换 1 分钟 < 10 分钟
	s.cooldownUntil = time.Now().Add(-time.Hour)
	cur := &model.NodeRuntime{Name: "当前", Score: &model.ScoreResult{Total: 50, Eligible: true}}
	cand := &model.NodeRuntime{Name: "候选", Score: &model.ScoreResult{Total: 90, Eligible: true}}
	for i := 0; i < 3; i++ {
		d := s.Evaluate(cur, cand)
		if d.Switch {
			t.Errorf("最小驻留未满不应切换：%s", d.Reason)
		}
	}
}
