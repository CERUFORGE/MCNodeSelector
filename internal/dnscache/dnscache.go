// Package dnscache 提供带 TTL 的域名解析缓存，用于拨号与测速：
//
//   - 命中缓存直接返回，避免每次拨号都走 DNS（Linux 纯 Go 解析器不缓存域名，
//     DNS 抖动/慢解析会串进延迟测量，造成假尖峰）；
//   - TTL（默认 60 秒）到期自动重解析，节点换 IP 后最多 60 秒内跟上；
//   - 拨号失败时立即失效并重解析（应对节点“自动换 IP”），并逐个尝试多个 A 记录；
//   - host 本身是 IP 时原样使用，零开销。
package dnscache

import (
	"context"
	"net"
	"sync"
	"time"
)

// ttl 解析缓存有效期（节点换 IP 时最多滞后这么久）
var ttl = 60 * time.Second

type entry struct {
	ips []string
	at  time.Time
}

var (
	mu    sync.Mutex
	cache = map[string]entry{}
)

// Lookup 返回 host 的 IP 列表（host 是 IP 时原样返回）。带 TTL 缓存；解析失败时沿用旧值。
func Lookup(host string) []string {
	if ip := net.ParseIP(host); ip != nil {
		return []string{host}
	}
	mu.Lock()
	if e, ok := cache[host]; ok && time.Since(e.at) < ttl && len(e.ips) > 0 {
		ips := append([]string(nil), e.ips...)
		mu.Unlock()
		return ips
	}
	mu.Unlock()

	ips, err := net.LookupHost(host)
	if err != nil || len(ips) == 0 {
		mu.Lock()
		old := append([]string(nil), cache[host].ips...)
		mu.Unlock()
		if len(old) > 0 {
			return old // 解析失败：先沿用旧 IP，避免直接不可用
		}
		return []string{host} // 完全解析不出来：交给底层再试一次
	}
	mu.Lock()
	cache[host] = entry{ips: ips, at: time.Now()}
	mu.Unlock()
	return ips
}

// First 返回 host 的首个 IP（多 A 记录时用于 mcping 这类只接受单地址的场景）
func First(host string) string {
	ips := Lookup(host)
	if len(ips) == 0 {
		return host
	}
	return ips[0]
}

// Invalidate 主动失效某域名缓存
func Invalidate(host string) {
	mu.Lock()
	delete(cache, host)
	mu.Unlock()
}

// DialContext 解析 host 后拨号：逐个尝试缓存的 IP；全失败则失效缓存、重解析后再试一轮。
func DialContext(ctx context.Context, network, hostPort string, timeout time.Duration) (net.Conn, error) {
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return (&net.Dialer{Timeout: timeout}).DialContext(ctx, network, hostPort)
	}
	dial := func(ip string) (net.Conn, error) {
		d := net.Dialer{Timeout: timeout}
		return d.DialContext(ctx, network, net.JoinHostPort(ip, port))
	}
	var lastErr error
	for _, ip := range Lookup(host) {
		c, e := dial(ip)
		if e == nil {
			return c, nil
		}
		lastErr = e
	}
	Invalidate(host) // 可能刚换了 IP：失效后重解析再试一轮
	for _, ip := range Lookup(host) {
		c, e := dial(ip)
		if e == nil {
			return c, nil
		}
		lastErr = e
	}
	return nil, lastErr
}
