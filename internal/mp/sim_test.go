package mp

import (
	"fmt"
	"io"
	"math/rand"
	"net"
	"sync"
	"testing"
	"time"
)

// ---------- 网络损伤模拟器 ----------
//
// 说明：多线底层走 TCP（可靠），真实丢包在本层表现为「延迟尖峰/排队」，而非丢字节。
// 模拟器对每个字节块注入 基础延迟 + 抖动，并可周期性制造「拥塞窗口」（额外大延迟），
// 以逼近晚高峰/节点拥堵的真实观感。多路径可共享同一「拥塞时钟」= 共用瓶颈；
// 也可用相位偏移让拥塞错开 = 真正独立。转发用「延迟队列 + 小缓冲(32)」：施加延迟且有背压。

type simParams struct {
	base        time.Duration
	jitter      time.Duration
	stallEvery  time.Duration // 每多久来一次拥塞（0=不来）
	stallDur    time.Duration // 拥塞窗口时长
	stallExtra  time.Duration // 拥塞窗口内额外延迟
	startOffset time.Duration // 相位偏移（让两条路的拥塞错开=真独立）
	clock       *simClock     // 共享时钟（用于两路同步拥塞=共用瓶颈）
}

type simClock struct {
	mu    sync.Mutex
	start time.Time
}

func newSimClock() *simClock { return &simClock{start: time.Now()} }

func (p simParams) delay(start time.Time) time.Duration {
	d := p.base
	if p.jitter > 0 {
		d += time.Duration(rand.Int63n(int64(p.jitter)))
	}
	if p.stallEvery > 0 && time.Since(start)%p.stallEvery < p.stallDur {
		d += p.stallExtra
	}
	return d
}

func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { defer c.Close(); _, _ = io.Copy(c, c) }(c)
		}
	}()
	return ln.Addr().String()
}

// startSimLink 起一条带损伤的链路：客户端连它 → 它转发到 target（并施加延迟）。
func startSimLink(t *testing.T, target string, p simParams) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				u, err := net.Dial("tcp", target)
				if err != nil {
					c.Close()
					return
				}
				go pumpSim(c, u, p)
				pumpSim(u, c, p)
			}(c)
		}
	}()
	return ln.Addr().String()
}

func pumpSim(src, dst net.Conn, p simParams) {
	defer src.Close()
	defer dst.Close()
	type item struct {
		at time.Time
		b  []byte
	}
	ch := make(chan item, 32) // 小缓冲：有背压，模拟真实链路而非无限排队
	start := time.Now().Add(-p.startOffset)
	if p.clock != nil {
		p.clock.mu.Lock()
		start = p.clock.start
		p.clock.mu.Unlock()
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				b := append([]byte(nil), buf[:n]...)
				ch <- item{at: time.Now().Add(p.delay(start)), b: b}
			}
			if err != nil {
				close(ch)
				return
			}
		}
	}()
	for it := range ch {
		if d := time.Until(it.at); d > 0 {
			time.Sleep(d)
		}
		if _, err := dst.Write(it.b); err != nil {
			return
		}
	}
}

// ---------- 场景运行 ----------

type simResult struct {
	p50, p95, max time.Duration
	spikes        int
	loss          int
}

// runScenario 在给定各路径损伤参数下跑一局，测量「玩家端实际往返」。
func runScenario(t *testing.T, paths []simParams, msgs int) simResult {
	t.Helper()
	echo := startEcho(t)

	agentLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go RunAgentListener(agentLn, echo)

	eps := make([]string, len(paths))
	for i, p := range paths {
		eps[i] = startSimLink(t, agentLn.Addr().String(), p)
	}

	a, b := net.Pipe()
	defer b.Close()
	go RunClientSession(uint64(time.Now().UnixNano()), a, eps)

	const size = 32
	const interval = 25 * time.Millisecond
	sendAt := make([]time.Time, 0, msgs)
	var mu sync.Mutex
	sendDone := make(chan struct{})

	go func() {
		defer close(sendDone)
		msg := make([]byte, size)
		for i := 0; i < msgs; i++ {
			for k := range msg {
				msg[k] = byte(i*31 + k)
			}
			mu.Lock()
			sendAt = append(sendAt, time.Now())
			mu.Unlock()
			if _, err := b.Write(msg); err != nil {
				return
			}
			time.Sleep(interval)
		}
	}()

	thr := 200 * time.Millisecond
	res := simResult{}
	var rtts []time.Duration
	buf := make([]byte, size)
	deadline := time.Now().Add(time.Duration(msgs)*interval + 30*time.Second)
	_ = b.SetReadDeadline(deadline)
	for i := 0; i < msgs; i++ {
		if _, err := io.ReadFull(b, buf); err != nil {
			res.loss = msgs - i
			break
		}
		arr := time.Now()
		mu.Lock()
		var st time.Time
		if i < len(sendAt) {
			st = sendAt[i]
		}
		mu.Unlock()
		if st.IsZero() {
			continue
		}
		d := arr.Sub(st)
		if d < 0 {
			d = 0
		}
		rtts = append(rtts, d)
		if d > thr {
			res.spikes++
		}
	}

	sortDurations(rtts)
	if len(rtts) > 0 {
		res.p50 = pct(rtts, 0.50)
		res.p95 = pct(rtts, 0.95)
		res.max = rtts[len(rtts)-1]
	}
	<-sendDone
	return res
}

func sortDurations(d []time.Duration) {
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j] < d[j-1]; j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
}

func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)-1))
	return sorted[i]
}

func ms(d time.Duration) string { return fmt.Sprintf("%.0fms", float64(d.Microseconds())/1000.0) }

// TestScenarios 设计四种典型网络情景，评估多线是否达标（并与单路对照）。
func TestScenarios(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过")
	}
	const N = 400

	type scene struct {
		name  string
		paths []simParams
	}
	sharedClock := newSimClock()
	scenes := []scene{
		{"① 正常（2路独立干净）", []simParams{
			{base: 25 * time.Millisecond, jitter: 3 * time.Millisecond},
			{base: 28 * time.Millisecond, jitter: 3 * time.Millisecond},
		}},
		{"② 单节点拥堵（B卡A好）", []simParams{
			{base: 25 * time.Millisecond, jitter: 3 * time.Millisecond},
			{base: 120 * time.Millisecond, jitter: 20 * time.Millisecond, stallEvery: 5 * time.Second, stallDur: 1 * time.Second, stallExtra: 400 * time.Millisecond},
		}},
		{"③ 晚高峰共用瓶颈（两路同步拥塞）", []simParams{
			{base: 70 * time.Millisecond, jitter: 10 * time.Millisecond, stallEvery: 4 * time.Second, stallDur: 1200 * time.Millisecond, stallExtra: 250 * time.Millisecond, clock: sharedClock},
			{base: 75 * time.Millisecond, jitter: 10 * time.Millisecond, stallEvery: 4 * time.Second, stallDur: 1200 * time.Millisecond, stallExtra: 250 * time.Millisecond, clock: sharedClock},
		}},
		{"④ 网络差（2路独立、相位错开卡顿）", []simParams{
			{base: 60 * time.Millisecond, jitter: 40 * time.Millisecond, stallEvery: 3 * time.Second, stallDur: 900 * time.Millisecond, stallExtra: 350 * time.Millisecond},
			{base: 65 * time.Millisecond, jitter: 40 * time.Millisecond, stallEvery: 3 * time.Second, stallDur: 900 * time.Millisecond, stallExtra: 350 * time.Millisecond, startOffset: 1500 * time.Millisecond},
		}},
	}

	for _, sc := range scenes {
		multi := runScenario(t, sc.paths, N)
		single := runScenario(t, sc.paths[:1], N)
		fmt.Printf("%-34s 多线 p50=%s p95=%s max=%s 尖峰=%d || 单路 p50=%s p95=%s max=%s 尖峰=%d\n",
			sc.name,
			ms(multi.p50), ms(multi.p95), ms(multi.max), multi.spikes,
			ms(single.p50), ms(single.p95), ms(single.max), single.spikes)
	}
}
