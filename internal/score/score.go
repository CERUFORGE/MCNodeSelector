// Package score 实现六分项加权评分（§7.2 / F4）。
//
//	分项归一化 linear(v, ideal, max)：
//	    v ≤ ideal → 100；v ≥ max → 0；否则 100×(max−v)/(max−ideal)
//	合成 Total = ( Σ wᵢ·Sᵢ / Σ wᵢ ) × node.weight
//
// 规则：
//   - Phase1 全失败 → 不参与排名（由调用方保证不传入）；
//   - MC 验证启用且候选 MC 全部失败 → Total=0，一票否决（VetoedByMC）；
//   - MC 验证未启用（或该节点未进入 Top-K 未被验证）→ w.mc 从分子分母同时剔除（动态重归一化）；
//   - 分母 ≤ 0 时置 1（防御全零权重，正常情况已被配置校验拦截）。
package score

import (
	"fmt"
	"math"

	"mcnode/internal/config"
	"mcnode/internal/model"
)

// linear 分段线性归一化到 0~100（§7.2）
func linear(v, ideal, max float64) float64 {
	// 防御：max ≤ ideal 时（配置异常）退化为二值判定，避免除零 panic
	if max <= ideal {
		if v <= ideal {
			return 100
		}
		return 0
	}
	switch {
	case v <= ideal:
		return 100
	case v >= max:
		return 0
	default:
		return 100 * (max - v) / (max - ideal)
	}
}

// Compute 计算单节点综合评分。
// 入参：节点配置、Phase1 统计（必须非 nil 且有成功样本）、MC 结果（可为 nil = 未验证/未启用）、历史分。
// 出参：含各分项分数与中文原因的 ScoreResult。
func Compute(node *config.Node, stats *model.ProbeStats, mc *model.MCResult, historyScore float64, cfg *config.Config) *model.ScoreResult {
	res := &model.ScoreResult{
		NodeName: node.Name,
		Parts:    make(map[string]float64, 7),
		Eligible: true,
	}

	// MC 一票否决（F3.2）：该节点进入过 Top-K 深度验证且 MC ping 全部失败
	mcChecked := mc != nil
	if mcChecked && !mc.OK {
		// 该节点进入过 Top-K 且 MC ping 全部失败 → 一票否决
		res.Total = 0
		res.Eligible = false
		res.VetoedByMC = true
		res.Reason = fmt.Sprintf("Minecraft 握手全部失败，一票否决（检查 remote_port 与隧道）：%s", mc.ErrMsg)
		return res
	}

	// ---- 各分项分数 ----
	// 延迟分项：优先用「端到端 MC RTT」（含节点→服务器段，才是游戏真实延迟），
	// 未做 MC 验证时才退回 Phase1 的「客户端→节点」延迟。
	var sLat float64
	latUsed := stats.AvgMs
	if mcChecked && mc.OK {
		latUsed = mc.AvgRTTMs
		sLat = linear(mc.AvgRTTMs, cfg.Scoring.MC.IdealMs, cfg.Scoring.MC.MaxMs)
	} else {
		sLat = linear(stats.AvgMs, cfg.Scoring.Latency.IdealMs, cfg.Scoring.Latency.MaxMs)
	}
	// P95/抖动/丢包：有端到端 MC 实测时统一用 MC 口径（与延迟一致），
	// 否则退回 Phase1 TCP 口径。
	p95Used := stats.P95Ms
	jitUsed := stats.JitterMs
	lossUsed := stats.LossPct
	spikeUsed := stats.SpikePct
	if mcChecked && mc.OK {
		if mc.P95RTTMs > 0 {
			p95Used = mc.P95RTTMs
		}
		if mc.BurstP95RTTMs > p95Used {
			p95Used = mc.BurstP95RTTMs // 带负载后的 P95 更能反映繁忙时的手感
		}
		jitUsed = mc.JitterMs
		lossUsed = (1 - mc.SuccessRate) * 100
		if mc.BurstLossPct > lossUsed {
			lossUsed = mc.BurstLossPct
		}
		if mc.BurstSpikePct > spikeUsed {
			spikeUsed = mc.BurstSpikePct // 繁忙时才抽的节点，尖峰分项直接扣分
		}
	}
	sP95 := linear(p95Used, cfg.Scoring.P95.IdealMs, cfg.Scoring.P95.MaxMs)
	sJit := linear(jitUsed, cfg.Scoring.Jitter.IdealMs, cfg.Scoring.Jitter.MaxMs)
	sLoss := linear(lossUsed, cfg.Scoring.Loss.IdealPct, cfg.Scoring.Loss.MaxPct)
	res.Parts["latency"] = sLat
	res.Parts["p95"] = sP95
	res.Parts["jitter"] = sJit
	res.Parts["loss"] = sLoss
	// 尖峰率分项（抗抖动）：0% 满分，≥max 0 分 —— 越爱抽的节点分越低
	sSpike := linear(spikeUsed, cfg.Scoring.Spike.IdealPct, cfg.Scoring.Spike.MaxPct)
	res.Parts["spike"] = sSpike

	// MC 分项：linear(mc_rtt, ideal, max) × 成功率（§7.2）
	var sMC float64
	if mcChecked && mc.OK {
		sMC = linear(mc.AvgRTTMs, cfg.Scoring.MC.IdealMs, cfg.Scoring.MC.MaxMs) * mc.SuccessRate
	}
	res.Parts["mc"] = sMC
	// 历史分直接取 EWMA 平滑分
	res.Parts["history"] = historyScore

	// ---- 动态重归一化加权合成 ----
	w := cfg.Scoring.Weights
	num, den := 0.0, 0.0
	add := func(weight, val float64) {
		num += weight * val
		den += weight
	}
	add(w.Latency, sLat)
	add(w.P95, sP95)
	add(w.Jitter, sJit)
	add(w.Loss, sLoss)
	add(w.Spike, sSpike)
	// MC 分项剔除规则：节点未通过 MC 验证途径（未启用 / 未进 Top-K 未验证）→ 权重不计入分母
	if mcChecked {
		add(w.MC, sMC)
	}
	add(w.History, historyScore)
	if den <= 0 {
		den = 1 // 防御全零权重（配置校验已拦截，此处兜底防 panic）
	}
	total := num / den * node.Weight
	// 修正无效值，防止 NaN 污染排序
	if math.IsNaN(total) || math.IsInf(total, 0) {
		total = 0
	}
	res.Total = round1(total)

	// ---- 中文原因（“为什么选它”）----
	mcDesc := "未验证"
	if mcChecked && mc.OK {
		mcDesc = fmt.Sprintf("%.0fms/%d%%", mc.AvgRTTMs, int(mc.SuccessRate*100))
	} else if mcChecked && !mc.OK {
		mcDesc = "失败"
	} else if !cfg.Probing.MCCheck.IsEnabled() {
		mcDesc = "已关闭"
	}
	res.Reason = fmt.Sprintf(
		"延迟 %.0fms(%.0f分) P95 %.0fms(%.0f分) 抖动 %.0fms(%.0f分) 丢包 %.0f%%(%.0f分) 尖峰 %.0f%%(%.0f分) MC %s(%.0f分) 历史%.0f(%.0f分) 权重×%.1f = %.1f",
		latUsed, sLat, p95Used, sP95, jitUsed, sJit,
		lossUsed, sLoss, spikeUsed, sSpike, mcDesc, sMC, historyScore, historyScore, node.Weight, res.Total,
	)
	return res
}

// Phase1Score Phase1 排名用部分分（不含 MC 分项，用于挑选 Top-K 候选，§4.2）。
// 与最终评分同构：latency/p95/jitter/loss/history 加权，动态重归一化。
func Phase1Score(node *config.Node, stats *model.ProbeStats, historyScore float64, cfg *config.Config) float64 {
	w := cfg.Scoring.Weights
	sLat := linear(stats.AvgMs, cfg.Scoring.Latency.IdealMs, cfg.Scoring.Latency.MaxMs)
	sP95 := linear(stats.P95Ms, cfg.Scoring.P95.IdealMs, cfg.Scoring.P95.MaxMs)
	sJit := linear(stats.JitterMs, cfg.Scoring.Jitter.IdealMs, cfg.Scoring.Jitter.MaxMs)
	sLoss := linear(stats.LossPct, cfg.Scoring.Loss.IdealPct, cfg.Scoring.Loss.MaxPct)
	sSpike := linear(stats.SpikePct, cfg.Scoring.Spike.IdealPct, cfg.Scoring.Spike.MaxPct)
	num := w.Latency*sLat + w.P95*sP95 + w.Jitter*sJit + w.Loss*sLoss + w.Spike*sSpike + w.History*historyScore
	den := w.Latency + w.P95 + w.Jitter + w.Loss + w.Spike + w.History
	if den <= 0 {
		den = 1
	}
	total := num / den * node.Weight
	if math.IsNaN(total) || math.IsInf(total, 0) {
		return 0
	}
	return total
}

// round1 保留 1 位小数（展示与比较统一精度）
func round1(v float64) float64 {
	return math.Round(v*10) / 10
}
