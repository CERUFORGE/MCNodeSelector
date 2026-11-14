// Package history 管理各节点 EWMA 历史分（F5 / §7.3）。
//
//	每轮全量测速后：S_new = S_old × (1−α) + x × α
//	  x = 本轮总分；节点本轮离线/超时 → x = 0（离线自然拉低历史分）
//	  新节点（无历史）初始 S = neutral_score（中性，避免高估或歧视）
//
// 持久化到 data/history.json，原子写入（.tmp + rename）；
// 加载时 JSON 损坏 → 自动备份为 *.corrupt.<时间戳> 并从空数据重建（仅 WARN，不崩溃）。
package history

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"mcnode/internal/fsutil"
	"mcnode/internal/logger"
)

// Entry 单节点历史记录
type Entry struct {
	Score     float64   `json:"score"`      // EWMA 平滑分
	Rounds    int       `json:"rounds"`     // 已记录轮数
	UpdatedAt time.Time `json:"updated_at"` // 最近更新时间
}

// fileData history.json 的序列化结构
type fileData struct {
	Version int              `json:"version"` // 格式版本，便于将来升级
	Nodes   map[string]Entry `json:"nodes"`
}

// Store 历史分存储（并发安全：监控协程与重测协程都会读写）
type Store struct {
	mu      sync.RWMutex
	path    string
	alpha   float64 // EWMA 系数
	neutral float64 // 新节点中性分
	entries map[string]Entry
}

// New 创建历史存储并从磁盘加载（损坏自动备份重建）
// alpha/neutral 来自 scoring.history 配置，热加载后可调用 SetParams 更新。
func New(path string, alpha, neutral float64) *Store {
	s := &Store{
		path:    path,
		alpha:   alpha,
		neutral: neutral,
		entries: make(map[string]Entry),
	}
	s.load()
	return s
}

// SetParams 热加载后更新 EWMA 参数
func (s *Store) SetParams(alpha, neutral float64) {
	s.mu.Lock()
	s.alpha = alpha
	s.neutral = neutral
	s.mu.Unlock()
}

// load 加载磁盘数据；文件不存在属正常情况（首次运行）静默初始化；
// JSON 损坏 → 备份 *.corrupt.<ts> 并从空数据重建，仅记 WARN（F5.4/F5.5）
func (s *Store) load() {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return // 首次运行，正常
		}
		logger.Warnf("读取历史数据失败（%s）：%v，将从空数据重建", s.path, err)
		return
	}
	var d fileData
	if err := json.Unmarshal(raw, &d); err != nil {
		// 数据损坏自愈：备份后重建，绝不崩溃
		if backup, berr := fsutil.BackupCorrupt(s.path); berr == nil {
			logger.Warnf("历史数据文件损坏，已自动备份为 %s 并从空数据重建（不影响运行）", backup)
		} else {
			logger.Warnf("历史数据文件损坏且备份失败（%v），已忽略旧数据从空数据重建", berr)
		}
		return
	}
	if d.Nodes != nil {
		s.entries = d.Nodes
	}
}

// Get 查询节点历史分；无记录时返回中性分 neutral_score（F5.3）
func (s *Store) Get(node string) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e, ok := s.entries[node]; ok {
		return e.Score
	}
	return s.neutral
}

// Rounds 查询节点已记录轮数（无记录返回 0，用于 --history 展示）
func (s *Store) Rounds(node string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e, ok := s.entries[node]; ok {
		return e.Rounds
	}
	return 0
}

// Update 记录一轮：EWMA 平滑（§7.3）。offline=true 时 x=0（离线自然拉低历史分）。
func (s *Store) Update(node string, x float64, offline bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if offline {
		x = 0
	}
	old, ok := s.entries[node]
	if !ok {
		// 新节点：旧分取中性值起步
		old = Entry{Score: s.neutral}
	}
	newScore := old.Score*(1-s.alpha) + x*s.alpha
	s.entries[node] = Entry{
		Score:     newScore,
		Rounds:    old.Rounds + 1,
		UpdatedAt: time.Now(),
	}
}

// Save 原子写入磁盘（.tmp + rename，防断电损坏）
func (s *Store) Save() error {
	s.mu.RLock()
	d := fileData{Version: 1, Nodes: s.entries}
	s.mu.RUnlock()
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化历史数据失败：%w", err)
	}
	if err := fsutil.WriteAtomic(s.path, data, 0o644); err != nil {
		return fmt.Errorf("写入历史数据失败：%w", err)
	}
	return nil
}

// Snapshot 返回全部条目副本（状态屏 / --history 展示用）
func (s *Store) Snapshot() map[string]Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Entry, len(s.entries))
	for k, v := range s.entries {
		out[k] = v
	}
	return out
}
