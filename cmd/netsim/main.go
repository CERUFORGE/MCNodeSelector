// netsim：可复现的网络损伤模拟器，用于按「网络质量档位」评估多线/单线。
// 注入 基础延迟 + 抖动 + 周期性拥塞（额外大延迟），测量玩家端实际往返。
package main

import (
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"sort"
	"sync"
	"time"

	"mcnode/internal/mp"
)

type simParams struct {
	base       time.Duration
	jitter     time.Duration
	stallEvery time.Duration
	stallDur   time.Duration
	stallExtra time.Duration
	offset     time.Duration
}

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

func startEcho() (string, net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
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
	return ln.Addr().String(), ln, nil
}

func startSimLink(target string, p simParams) (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
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
	return ln, nil
}

func pumpSim(src, dst net.Conn, p simParams) {
	defer src.Close()
	defer dst.Close()
	type item struct {
		at time.Time
		b  []byte
	}
	ch := make(chan item, 64)
	start := time.Now().Add(-p.offset)
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

type result struct {
	p50, p95, max time.Duration
	spikes        int
	loss          int
	n             int
}

func run(paths []simParams, secs int, interval time.Duration) result {
	echo, echoLn, err := startEcho()
	if err != nil {
		fmt.Println("ERR", err)
		os.Exit(1)
	}
	defer echoLn.Close()
	agentLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("ERR", err)
		os.Exit(1)
	}
	defer agentLn.Close()
	go mp.RunAgentListener(agentLn, echo)

	var links []net.Listener
	eps := make([]string, 0, len(paths))
	for _, p := range paths {
		ln, err := startSimLink(agentLn.Addr().String(), p)
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		links = append(links, ln)
		eps = append(eps, ln.Addr().String())
	}
	defer func() {
		for _, ln := range links {
			ln.Close()
		}
	}()

	a, b := net.Pipe()
	defer b.Close()
	go mp.RunClientSession(uint64(time.Now().UnixNano()), a, eps)

	const size = 32
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	sendAt := make([]time.Time, 0, secs*40)
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		msg := make([]byte, size)
		for time.Now().Before(deadline) {
			mu.Lock()
			sendAt = append(sendAt, time.Now())
			mu.Unlock()
			if _, err := b.Write(msg); err != nil {
				return
			}
			time.Sleep(interval)
		}
	}()

	_ = b.SetReadDeadline(deadline.Add(20 * time.Second))
	res := result{}
	var rtts []time.Duration
	buf := make([]byte, size)
	idx := 0
	for time.Now().Before(deadline) {
		if _, err := io.ReadFull(b, buf); err != nil {
			break
		}
		arr := time.Now()
		mu.Lock()
		var st time.Time
		if idx < len(sendAt) {
			st = sendAt[idx]
		}
		mu.Unlock()
		idx++
		if st.IsZero() {
			continue
		}
		d := arr.Sub(st)
		if d < 0 {
			d = 0
		}
		rtts = append(rtts, d)
		if d > 200*time.Millisecond {
			res.spikes++
		}
	}
	<-done
	res.n = len(rtts)
	res.loss = idx - len(rtts)
	sort.Slice(rtts, func(i, j int) bool { return rtts[i] < rtts[j] })
	if len(rtts) > 0 {
		res.p50 = rtts[len(rtts)*50/100]
		res.p95 = rtts[len(rtts)*95/100]
		res.max = rtts[len(rtts)-1]
	}
	return res
}

func main() {
	base := flag.Int("base", 30, "base delay ms (per direction)")
	jitter := flag.Int("jitter", 5, "jitter ms")
	stallEvery := flag.Int("stallEvery", 0, "stall period seconds (0=off)")
	stallDur := flag.Int("stallDur", 0, "stall duration ms")
	stallExtra := flag.Int("stallExtra", 0, "extra delay during stall ms")
	paths := flag.Int("paths", 1, "number of paths (1=single, 2=multi)")
	phase := flag.Int("phase", 0, "phase offset ms for 2nd path")
	secs := flag.Int("secs", 300, "test duration seconds")
	label := flag.String("label", "", "label")
	flag.Parse()

	sp := simParams{
		base: time.Duration(*base) * time.Millisecond, jitter: time.Duration(*jitter) * time.Millisecond,
		stallEvery: time.Duration(*stallEvery) * time.Second, stallDur: time.Duration(*stallDur) * time.Millisecond,
		stallExtra: time.Duration(*stallExtra) * time.Millisecond,
	}
	list := []simParams{sp}
	if *paths >= 2 {
		sp2 := sp
		sp2.offset = time.Duration(*phase) * time.Millisecond
		list = append(list, sp2)
	}
	r := run(list, *secs, 25*time.Millisecond)
	fmt.Printf("%s paths=%d secs=%d p50=%.0fms p95=%.0fms max=%.0fms spikes=%d n=%d loss=%d\n",
		*label, *paths, *secs,
		float64(r.p50.Microseconds())/1000, float64(r.p95.Microseconds())/1000, float64(r.max.Microseconds())/1000,
		r.spikes, r.n, r.loss)
}
