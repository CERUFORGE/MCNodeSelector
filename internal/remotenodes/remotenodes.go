package remotenodes

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mcnode/internal/config"
	"mcnode/internal/fsutil"
	"mcnode/internal/logger"
)

// remoteCacheFile 缓存文件结构
type remoteCacheFile struct {
	URL       string        `json:"url"`        // 拉取来源 URL（用于校验缓存归属）
	FetchedAt time.Time     `json:"fetched_at"` // 拉取时间
	Nodes     []config.Node `json:"nodes"`      // 远程节点列表
}

// Manager 远程节点列表管理器：周期性从 EdgeOne（或其他 HTTP 源）拉取节点列表，
// 拉取失败时按顺序回退：远程缓存文件 → 本地 config.jsonc 的 nodes[]（用户需求：读不了服务器时读本地）。
// 解析后的节点与本地节点合并去重（本地优先，同名节点以本地为准）。
// 每次远程拉取成功后把合并结果保存到 data/remote_cache.json（用户需求：每次拉完远程都保存在本地，以免下次拉不了）。
type Manager struct {
	mu        sync.RWMutex
	cfgFn     func() *config.Config // 取最新配置（含 remote.url/timeout/interval）
	nodes     []config.Node         // 当前生效节点（远程+本地合并去重）
	lastOK    bool                  // 上次拉取是否成功
	lastErr   string                // 上次失败原因（中文）
	lastTime  time.Time             // 上次拉取时间
	cachePath string                // 远程节点缓存文件路径（data/remote_cache.json）
}

// New 创建远程节点管理器
func New(cfgFn func() *config.Config) *Manager {
	cfg := cfgFn()
	return &Manager{
		cfgFn:     cfgFn,
		cachePath: filepath.Join(cfg.DataDir(), "remote_cache.json"),
	}
}

// Refresh 立即拉取一次远程节点列表并合并本地节点。
// 返回当前生效节点列表。拉取失败时按顺序回退：远程缓存文件 → 本地节点（用户需求：读不了服务器时读本地）。
func (m *Manager) Refresh() []config.Node {
	cfg := m.cfgFn()
	url := strings.TrimSpace(cfg.Remote.URL)
	if url == "" {
		// 未配置远程源：直接用本地节点
		m.mu.Lock()
		m.nodes = cfg.Nodes
		m.lastOK = true
		m.lastErr = ""
		m.lastTime = time.Now()
		out := m.nodes
		m.mu.Unlock()
		return out
	}
	timeout := time.Duration(cfg.Remote.Timeout) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	remote, err := Fetch(context.Background(), url, timeout)
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastTime = now
	if err != nil {
		m.lastOK = false
		m.lastErr = err.Error()
		// 回退顺序：远程缓存文件 → 本地节点（用户需求：读不了服务器时读本地）
		if cached, ok := m.loadCache(url); ok && len(cached) > 0 {
			m.nodes = m.merge(cfg, cached)
			logger.Warnf("远程节点列表拉取失败，已使用上次缓存（%d 个，时间 %s）：%v",
				len(cached), m.cacheTime(url).Format("2006-01-02 15:04:05"), err)
			return m.nodes
		}
		m.nodes = cfg.Nodes
		logger.Warnf("远程节点列表拉取失败且无缓存，已回退本地节点（%d 个）：%v", len(cfg.Nodes), err)
		return m.nodes
	}
	// 校验远程节点（中文报错，单个节点出错则跳过该节点而非整体失败）
	valid := make([]config.Node, 0, len(remote))
	seen := make(map[string]bool)
	for i := range remote {
		n := &remote[i]
		// 应用默认权重
		if n.Weight == 0 {
			n.Weight = 1.0
		}
		if err := validateRemoteNode(n, i); err != nil {
			logger.Warnf("远程节点[%d] 校验失败，已跳过：%v", i+1, err)
			continue
		}
		if seen[n.Name] {
			logger.Warnf("远程节点[%d]（%s）名称重复，已跳过", i+1, n.Name)
			continue
		}
		seen[n.Name] = true
		valid = append(valid, *n)
	}
	// 合并：远程为权威源，本地节点补充同名（本地优先，用于覆盖/微调远程节点）
	m.nodes = m.merge(cfg, valid)
	m.lastOK = true
	m.lastErr = ""
	// 保存到本地缓存（用户需求：每次拉完远程都保存在本地，以免下次拉不了）
	if err := m.saveCache(url, valid, now); err != nil {
		logger.Warnf("保存远程节点缓存失败（不影响运行）：%v", err)
	}
	logger.Infof("远程节点列表已更新：远程 %d 个（有效 %d），本地 %d 个，合并后 %d 个（已缓存到本地）",
		len(remote), len(valid), len(cfg.Nodes), len(m.nodes))
	return m.nodes
}

// merge 合并远程有效节点与本地节点（本地同名覆盖远程）
func (m *Manager) merge(cfg *config.Config, remote []config.Node) []config.Node {
	out := make([]config.Node, 0, len(remote)+len(cfg.Nodes))
	nameIdx := make(map[string]int)
	for i := range remote {
		out = append(out, remote[i])
		nameIdx[remote[i].Name] = i
	}
	for i := range cfg.Nodes {
		local := cfg.Nodes[i]
		if idx, ok := nameIdx[local.Name]; ok {
			out[idx] = local // 同名：本地覆盖远程
		} else {
			out = append(out, local) // 远程没有的本地节点：追加
			nameIdx[local.Name] = len(out) - 1
		}
	}
	return out
}

// saveCache 保存远程节点列表到本地缓存（原子写）
func (m *Manager) saveCache(url string, nodes []config.Node, t time.Time) error {
	d := remoteCacheFile{URL: url, FetchedAt: t, Nodes: nodes}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化缓存失败：%w", err)
	}
	return fsutil.WriteAtomic(m.cachePath, data, 0o644)
}

// loadCache 从本地缓存加载远程节点列表（URL 不匹配则视为不可用）
func (m *Manager) loadCache(url string) ([]config.Node, bool) {
	raw, err := os.ReadFile(m.cachePath)
	if err != nil {
		return nil, false
	}
	var d remoteCacheFile
	if err := json.Unmarshal(raw, &d); err != nil {
		logger.Warnf("远程节点缓存文件损坏，已忽略：%v", err)
		return nil, false
	}
	// URL 不匹配（换了远程源）：缓存不可用
	if d.URL != url {
		return nil, false
	}
	return d.Nodes, true
}

// cacheTime 读取缓存时间（用于日志展示）
func (m *Manager) cacheTime(url string) time.Time {
	raw, err := os.ReadFile(m.cachePath)
	if err != nil {
		return time.Time{}
	}
	var d remoteCacheFile
	if err := json.Unmarshal(raw, &d); err != nil {
		return time.Time{}
	}
	return d.FetchedAt
}

// Nodes 返回当前生效节点列表（快照副本）
func (m *Manager) Nodes() []config.Node {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]config.Node, len(m.nodes))
	copy(out, m.nodes)
	return out
}

// Status 远程拉取状态（状态屏展示用）
func (m *Manager) Status() (ok bool, lastErr string, lastTime time.Time) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastOK, m.lastErr, m.lastTime
}

// validateRemoteNode 校验单个远程节点（与本地校验规则一致，中文报错）
func validateRemoteNode(n *config.Node, idx int) error {
	where := fmt.Sprintf("远程节点[%d]", idx+1)
	if strings.TrimSpace(n.Name) != "" {
		where = fmt.Sprintf("远程节点[%d]（%s）", idx+1, n.Name)
	}
	if strings.TrimSpace(n.Name) == "" {
		return fmt.Errorf("%s 的 name 为空", where)
	}
	if strings.TrimSpace(n.ServerAddr) == "" {
		return fmt.Errorf("%s 的 server_addr 为空", where)
	}
	if n.ServerPort < 1 || n.ServerPort > 65535 {
		return fmt.Errorf("%s 的 server_port（%d）越界", where, n.ServerPort)
	}
	if n.RemotePort < 1 || n.RemotePort > 65535 {
		return fmt.Errorf("%s 的 remote_port（%d）越界", where, n.RemotePort)
	}
	if n.Weight < 0 {
		return fmt.Errorf("%s 的 weight（%.2f）为负", where, n.Weight)
	}
	return nil
}
