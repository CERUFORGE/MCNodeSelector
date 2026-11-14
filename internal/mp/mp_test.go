package mp

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

func TestReorderDedupInOrder(t *testing.T) {
	r := newReorder()
	if out := r.push(2, []byte("c")); out != nil {
		t.Fatalf("乱序片段应暂存，got %v", out)
	}
	out := r.push(0, []byte("a"))
	if len(out) != 1 || string(out[0]) != "a" {
		t.Fatalf("应交付 a，got %v", out)
	}
	out = r.push(1, []byte("b"))
	if len(out) != 2 || string(out[0]) != "b" || string(out[1]) != "c" {
		t.Fatalf("应交付 b,c，got %v", out)
	}
	if out := r.push(1, []byte("b")); out != nil {
		t.Fatalf("重复片段应被丢弃，got %v", out)
	}
}

func echoServer(t *testing.T) net.Listener {
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
			go func(c net.Conn) { _, _ = io.Copy(c, c); _ = c.Close() }(c)
		}
	}()
	return ln
}

func blackhole(t *testing.T) net.Listener {
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
			go func(c net.Conn) { time.Sleep(60 * time.Second); _ = c.Close() }(c)
		}
	}()
	return ln
}

// 全量冗余：两条可靠路径，4MB 数据完整往返。
func TestMultipathFullDuplication(t *testing.T) {
	echo := echoServer(t)
	defer echo.Close()
	agentLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = RunAgentListener(agentLn, echo.Addr().String()) }()

	a, b := net.Pipe()
	defer b.Close()
	go RunClientSession(0x1111, a, []string{agentLn.Addr().String(), agentLn.Addr().String()})

	payload := make([]byte, 4*1024*1024)
	for i := range payload {
		payload[i] = byte(i*31 + 7)
	}
	go func() { _, _ = b.Write(payload) }()
	got := make([]byte, len(payload))
	_ = b.SetReadDeadline(time.Now().Add(25 * time.Second))
	if _, err := io.ReadFull(b, got); err != nil {
		t.Fatalf("全量冗余往返失败：%v", err)
	}
	if !bytes.Equal(payload, got) {
		t.Fatal("数据不一致")
	}
}

// 一条好路径 + 一条假死路径：冗余副本仍能让数据完整往返。
func TestMultipathDeadPathRedundancy(t *testing.T) {
	echo := echoServer(t)
	defer echo.Close()
	agentLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = RunAgentListener(agentLn, echo.Addr().String()) }()
	bad := blackhole(t)
	defer bad.Close()

	a, b := net.Pipe()
	defer b.Close()
	go RunClientSession(0x2222, a, []string{agentLn.Addr().String(), bad.Addr().String()})

	payload := make([]byte, 2*1024*1024)
	for i := range payload {
		payload[i] = byte(i*13 + 5)
	}
	go func() { _, _ = b.Write(payload) }()
	got := make([]byte, len(payload))
	_ = b.SetReadDeadline(time.Now().Add(25 * time.Second))
	if _, err := io.ReadFull(b, got); err != nil {
		t.Fatalf("坏路径未被冗余屏蔽：%v", err)
	}
	if !bytes.Equal(payload, got) {
		t.Fatal("数据不一致")
	}
}

// 一条路径中途掉线：另一条持续工作，数据完整。
func TestMultipathPathLossRecovery(t *testing.T) {
	echo := echoServer(t)
	defer echo.Close()
	agentLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = RunAgentListener(agentLn, echo.Addr().String()) }()

	flaky, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer flaky.Close()
	go func() {
		for {
			c, err := flaky.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { time.Sleep(200 * time.Millisecond); _ = c.Close() }(c)
		}
	}()

	a, b := net.Pipe()
	defer b.Close()
	go RunClientSession(0x3333, a, []string{agentLn.Addr().String(), flaky.Addr().String()})

	payload := make([]byte, 2*1024*1024)
	for i := range payload {
		payload[i] = byte(i*17 + 3)
	}
	go func() { _, _ = b.Write(payload) }()
	got := make([]byte, len(payload))
	_ = b.SetReadDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.ReadFull(b, got); err != nil {
		t.Fatalf("路径掉线后数据未完整：%v", err)
	}
	if !bytes.Equal(payload, got) {
		t.Fatal("数据不一致")
	}
}

// TestPathsCorrelated 验证「共用瓶颈检测」：同步涨落的路径判为相关，反相的判为独立。
func TestPathsCorrelated(t *testing.T) {
	a := newPath(nil, "a", 0)
	b := newPath(nil, "b", 0)
	for _, v := range []int64{100, 200, 150, 300, 250, 400} {
		a.hist = append(a.hist, v)
		b.hist = append(b.hist, v*2+v/3) // 线性正相关
	}
	if !pathsCorrelated(a, b, correlationThreshold) {
		t.Fatal("同步涨落的两条路径应判为相关（共用瓶颈）")
	}
	c := newPath(nil, "c", 0)
	for _, v := range []int64{400, 100, 350, 150, 300, 120} {
		c.hist = append(c.hist, v)
	}
	if pathsCorrelated(a, c, correlationThreshold) {
		t.Fatal("反相路径不应判为相关")
	}
}

// TestEligible 验证「近延迟过滤」：慢很多的路不参与冗余，只作故障切换。
func TestEligible(t *testing.T) {
	b := &broadcaster{}
	p1 := newPath(nil, "p1", 0)
	p1.rttUs.Store(60_000)
	p2 := newPath(nil, "p2", 1)
	p2.rttUs.Store(70_000) // 60*1.5+20=110ms 以内 → 合格
	p3 := newPath(nil, "p3", 2)
	p3.rttUs.Store(500_000) // 远慢于最优 → 不合格
	el := b.eligible([]*path{p1, p2, p3})
	if len(el) != 2 {
		t.Fatalf("合格路径应为 2 条，实际 %d 条", len(el))
	}
	if el[0] != p1 {
		t.Fatal("最优路径应排第一")
	}
}
