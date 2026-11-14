// Package forwarder 实现本地 TCP 转发：在本地开一个监听端口，
// 把玩家连接转发到当前最优节点的 FRP 隧道端口（server_addr:remote_port）。
// 切换节点时只需更新转发目标，新连接自动走新节点；旧连接保持不断直到玩家自己断。
// 玩家始终连 127.0.0.1:local_port，无需感知后端节点变化。
package forwarder

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"mcnode/internal/logger"
	"mcnode/internal/mp"
)

// Forwarder 本地 TCP 转发器
type Forwarder struct {
	mu        sync.RWMutex
	listen    string       // 主监听地址 127.0.0.1:25565
	extra     []string     // 额外监听地址（多端口转发）
	target    atomic.Value // string，当前转发目标 host:port
	lns       []net.Listener
	cancel    context.CancelFunc
	done      chan struct{}
	running   atomic.Bool
	conns     sync.WaitGroup // 活跃连接计数（优雅关闭用）
	active    atomic.Int64   // 当前活跃玩家连接数（供“有人在线时暂停更优切换”）
	connMu    sync.Mutex
	connSet   map[net.Conn]struct{} // 活跃连接集合（供“强制重连”关闭全部连接）
	mpEnabled atomic.Bool
	mpPaths   atomic.Value // []string：多路冗余路径
	mpSeq     atomic.Uint64
}

// New 创建转发器。listen 为本地监听地址（如 127.0.0.1:25565）。
func New(listen string) *Forwarder {
	f := &Forwarder{listen: listen}
	f.target.Store("")
	return f
}

// SetExtraListens 设置额外监听地址（多端口转发）。
func (f *Forwarder) SetExtraListens(addrs []string) {
	f.mu.Lock()
	f.extra = append([]string(nil), addrs...)
	f.mu.Unlock()
}

// Start 启动本地监听（主端口 + 额外端口）。已有监听则先停掉。
func (f *Forwarder) Start() error {
	f.Stop()
	f.mu.RLock()
	addrs := append([]string{f.listen}, f.extra...)
	f.mu.RUnlock()

	var lns []net.Listener
	for _, a := range addrs {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			for _, x := range lns {
				_ = x.Close()
			}
			return fmt.Errorf("本地转发端口监听失败（%s）：%w", a, err)
		}
		lns = append(lns, ln)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.mu.Lock()
	f.lns = lns
	f.cancel = cancel
	f.done = make(chan struct{})
	f.mu.Unlock()
	f.running.Store(true)
	var wg sync.WaitGroup
	for _, ln := range lns {
		wg.Add(1)
		go func(ln net.Listener) {
			defer wg.Done()
			f.acceptLoop(ctx, ln)
		}(ln)
	}
	go func() {
		wg.Wait()
		close(f.done)
	}()
	logger.Infof("本地转发已启动：监听 %v → 自动转发到最优节点隧道", addrs)
	return nil
}

// SetTarget 设置转发目标（host:port）。新连接自动走新目标；旧连接不断。
func (f *Forwarder) SetTarget(target string) {
	old, _ := f.target.Load().(string)
	f.target.Store(target)
	if old != target && old != "" {
		logger.Infof("转发目标已切换：%s → %s（新连接走新节点，旧连接保持）", old, target)
	} else if old == "" {
		logger.Infof("转发目标已设置：%s", target)
	}
}

// Target 当前转发目标
func (f *Forwarder) Target() string {
	t, _ := f.target.Load().(string)
	return t
}

// Running 是否在运行
func (f *Forwarder) Running() bool { return f.running.Load() }

// ActiveConns 当前活跃玩家连接数（用于“有人在线时暂停更优切换”）。
func (f *Forwarder) ActiveConns() int { return int(f.active.Load()) }

// SetMultipath 设置多路冗余路径（为空则关闭，走单目标转发）。
func (f *Forwarder) SetMultipath(paths []string) {
	if len(paths) == 0 {
		f.mpEnabled.Store(false)
		f.mpPaths.Store([]string(nil))
		return
	}
	f.mpPaths.Store(append([]string(nil), paths...))
	f.mpEnabled.Store(true)
}

// MultipathEnabled 当前是否处于多路冗余模式
func (f *Forwarder) MultipathEnabled() bool { return f.mpEnabled.Load() }

// trackConn 记录活跃连接（供 CloseAll 使用）
func (f *Forwarder) trackConn(c net.Conn) {
	f.connMu.Lock()
	if f.connSet == nil {
		f.connSet = make(map[net.Conn]struct{})
	}
	f.connSet[c] = struct{}{}
	f.connMu.Unlock()
}

// untrackConn 移除活跃连接记录
func (f *Forwarder) untrackConn(c net.Conn) {
	f.connMu.Lock()
	delete(f.connSet, c)
	f.connMu.Unlock()
}

// CloseAll 关闭当前全部玩家连接（用于“节点持续高延迟时强制其重连到新节点”），返回关闭数量。
func (f *Forwarder) CloseAll() int {
	f.connMu.Lock()
	cs := make([]net.Conn, 0, len(f.connSet))
	for c := range f.connSet {
		cs = append(cs, c)
	}
	f.connMu.Unlock()
	for _, c := range cs {
		_ = c.Close()
	}
	return len(cs)
}

// acceptLoop 接受玩家连接，每条连接独立转发
func (f *Forwarder) acceptLoop(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return // 正常关闭
			}
			logger.Warnf("本地转发接受连接失败：%v", err)
			continue
		}
		f.conns.Add(1)
		f.active.Add(1)
		f.trackConn(conn)
		go f.handleConn(ctx, conn)
	}
}

// handleConn 处理单条玩家连接：连到当前目标，双向复制字节
func (f *Forwarder) handleConn(ctx context.Context, playerConn net.Conn) {
	defer f.conns.Done()
	defer f.active.Add(-1)
	defer f.untrackConn(playerConn)
	defer playerConn.Close()

	// 多路冗余模式：把这条玩家连接同时走 N 条路径（任一不卡/不断即可）
	if f.mpEnabled.Load() {
		paths, _ := f.mpPaths.Load().([]string)
		mp.RunClientSession(f.mpSeq.Add(1), playerConn, paths)
		return
	}

	target := f.Target()
	if target == "" {
		logger.Warnf("玩家连接 %s 到来，但转发目标未设置，已断开", playerConn.RemoteAddr())
		return
	}

	remoteConn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		logger.Warnf("转发到 %s 失败（玩家 %s 已断开）：%v", target, playerConn.RemoteAddr(), err)
		return
	}
	defer remoteConn.Close()

	logger.Debugf("转发建立：%s → %s → %s", playerConn.RemoteAddr(), f.listen, target)

	// 双向复制：玩家→远程，远程→玩家
	// 任一方向结束即关闭两条连接（玩家断开或隧道断开都触发）
	done := make(chan struct{}, 2)
	go func() { io.Copy(remoteConn, playerConn); done <- struct{}{} }()
	go func() { io.Copy(playerConn, remoteConn); done <- struct{}{} }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// Stop 停止转发器：关监听，等活跃连接结束（5 秒超时后强制）
func (f *Forwarder) Stop() {
	f.mu.Lock()
	lns := f.lns
	cancel := f.cancel
	done := f.done
	f.lns = nil
	f.cancel = nil
	f.mu.Unlock()
	if len(lns) == 0 {
		return
	}
	f.running.Store(false)
	if cancel != nil {
		cancel()
	}
	for _, ln := range lns {
		_ = ln.Close()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
	// 给活跃连接一点时间自然结束
	f.conns.Wait()
}
