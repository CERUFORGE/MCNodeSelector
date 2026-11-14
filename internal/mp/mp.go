// Package mp 实现「分级可靠多路隧道」——弱网下兼顾低延迟、低抖动与低带宽占用。
//
// 核心策略：
//   - 小包（移动/操作等，≤ smallFrameThreshold）→ 多路冗余复制，任一路径先到即用，掩盖抖动/丢包；
//   - 大包（区块等大流量）→ 只走「最稳的 2 条路径」且复制（取先到者），既不等最慢路径、也不铺到差路径；
//   - 全方向累积确认（ACK）+ 未确认重传（含路径断开时经其他路径补发）；
//   - 「按序取出 + 写回本地」在同一把锁内完成，确保字节流绝不乱序（否则 MC 解压会失败掉线）；
//   - 未确认帧过多时对上游施加背压，绝不丢帧；路径断开后台自动重连、本地连接不关（会话保持）。
package mp

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"mcnode/internal/dnscache"
)

const (
	frameData  = 1
	frameHello = 2
	frameAck   = 3
	frameBye   = 4
	framePing  = 5
	framePong  = 6
)

const (
	chunkSize   = 16 * 1024
	maxFrameLen = 4 * 1024 * 1024
	pathQueue   = 2048
	maxPending  = 1024 // 未确认帧上限：降低以避免断链时攒一大坨、恢复时洪泛
)

var (
	// 分级发送：≤tiny 的关键小包全路径冗余；普通包发 redundantPaths 条；
	// 大包发 largeFramePaths 条（默认 1，减少共用的家宽上行被翻倍放大）。
	tinyFrameThreshold  = 256
	smallFrameThreshold = 1200
	redundantPaths      = 2

	retransmitInterval = 100 * time.Millisecond
	retransmitRTO      = 250 * time.Millisecond // 基础重传超时（缩短队头阻塞）
	retransmitRTOMax   = 2 * time.Second        // 指数退避上限
	fastRetransmitWait = 60 * time.Millisecond  // 快速重传：ACK 停滞多久即补发首帧
	ackMinInterval     = 20 * time.Millisecond
	reconnectInterval  = 1500 * time.Millisecond
	keepaliveInterval  = 500 * time.Millisecond // 每路径质量探测周期（会话中持续采样，才能抓到瞬时尖峰）
	pathDeadLossTicks  = 5                      // 连续多少秒 100% 丢包则强制重连该路径
	noPathGrace        = 60 * time.Second
)

// largeFramePaths 大包使用的路径条数（默认 1；可由配置设置）
var largeFramePaths atomic.Int64

// failoverOnly 为 true 时永不复制，只走最优一条路径（弱网/共用瓶径下最稳）
var failoverOnly atomic.Bool

// autoRedundancy 为 true 时按“当前网络质量”自动决定是否复制（自校准，通用）
var autoRedundancy atomic.Bool

// 通用判定阈值（不与具体网络绑定，配默认值即可对所有人工作）：
//
//	correlationThreshold：两条路径 RTT 同步变化的相关系数 ≥ 此值 = 共用瓶径 → 复制无意义；
//	defaultQueueRiseUs：  RTT 比“该路径自身基线”高这么多 = 排队/拥塞 → 禁止再放大流量；
//	defaultJitterUs：     抖动超过此值 = 需要冗余的弱链路（但要“路径独立”才值得复制）。
const (
	correlationThreshold = 0.7
	defaultQueueRiseUs   = 30_000 // 30ms
	defaultJitterUs      = 25_000 // 25ms
)

var (
	queueRiseUs  atomic.Int64 // 排队抬升判定阈值（微秒），0=用默认
	weakJitterUs atomic.Int64 // 抖动判定阈值（微秒），0=用默认
)

// SetLargeFramePaths 设置大包冗余路径数（>=1）
func SetLargeFramePaths(n int) {
	if n < 1 {
		n = 1
	}
	largeFramePaths.Store(int64(n))
}

// SetFailoverOnly 设置「只故障切换、不冗余复制」模式
func SetFailoverOnly(v bool) { failoverOnly.Store(v) }

// SetAutoRedundancy 设置「按当前网络质量自动决定是否复制」
func SetAutoRedundancy(v bool) { autoRedundancy.Store(v) }

// SetWeakThresholds 设置阈值（毫秒）：queueRiseMs=排队抬升阈值，jitterMs=抖动阈值
func SetWeakThresholds(queueRiseMs, jitterMs float64) {
	queueRiseUs.Store(int64(queueRiseMs * 1000))
	weakJitterUs.Store(int64(jitterMs * 1000))
}

// spikeThresholdUs 隧道往返尖峰判定阈值（微秒），0=不报
var spikeThresholdUs atomic.Int64

// SetSpikeThresholdMs 设置隧道尖峰阈值（毫秒），0=不报
func SetSpikeThresholdMs(ms float64) { spikeThresholdUs.Store(int64(ms * 1000)) }

var (
	Debugf = func(format string, args ...any) {}
	Warnf  = func(format string, args ...any) {}

	// OnPathQuality 周期性回报一条路径的质量（供状态屏展示）：RTT、抖动、窗口内最大RTT、丢包、队列。
	OnPathQuality = func(addr string, rttMs, jitterMs, maxMs, lossPct float64, queue int) {}

	// OnPathDown 路径断开时回调（供上层触发定向重测、替换掉这条路径）。
	OnPathDown = func(addr string) {}

	// OnPathSpike 隧道往返出现尖峰时回调（供上层按真实会话统计尖峰）。
	OnPathSpike = func(addr string, rttMs float64) {}
)

// agent/client 共享密钥：agent 校验握手 token，防止公网任何人都能连上 agent 转发进 MC。
var (
	clientToken atomic.Value // string
	agentToken  atomic.Value // string
)

// agentPassthrough 是否允许非多线连接做原始 TCP 透传（默认 true，兼容 MC 状态 ping）。
var agentPassthrough atomic.Bool

func init() { agentPassthrough.Store(true) }

// SetAgentPassthrough 设置 agent 是否允许原始透传（false = 只接受带 token 的多线会话）。
func SetAgentPassthrough(v bool) { agentPassthrough.Store(v) }

// SetClientToken 设置客户端握手 token（与 agent 一致）
func SetClientToken(t string) { clientToken.Store(t) }

// SetAgentToken 设置 agent 要求的 token（为空则不校验）
func SetAgentToken(t string) { agentToken.Store(t) }

func currentClientToken() string {
	s, _ := clientToken.Load().(string)
	return s
}
func currentAgentToken() string {
	s, _ := agentToken.Load().(string)
	return s
}

// ---------- 帧编解码 ----------

func encodeFrame(typ byte, seq uint64, payload []byte) []byte {
	buf := make([]byte, 13+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], uint32(9+len(payload)))
	buf[4] = typ
	binary.BigEndian.PutUint64(buf[5:13], seq)
	copy(buf[13:], payload)
	return buf
}

func writeFrame(w io.Writer, typ byte, seq uint64, payload []byte) error {
	_, err := w.Write(encodeFrame(typ, seq, payload))
	return err
}

func readFrame(br *bufio.Reader) (typ byte, seq uint64, payload []byte, err error) {
	var lb [4]byte
	if _, err = io.ReadFull(br, lb[:]); err != nil {
		return
	}
	l := binary.BigEndian.Uint32(lb[:])
	if l < 9 || l > maxFrameLen {
		err = fmt.Errorf("非法帧长 %d", l)
		return
	}
	buf := make([]byte, l)
	if _, err = io.ReadFull(br, buf); err != nil {
		return
	}
	typ = buf[0]
	seq = binary.BigEndian.Uint64(buf[1:9])
	payload = buf[9:]
	return
}

// ---------- 去重 / 按序还原 ----------

type reorder struct {
	mu   sync.Mutex
	next uint64
	buf  map[uint64][]byte
}

func newReorder() *reorder { return &reorder{buf: make(map[uint64][]byte)} }

func (r *reorder) push(seq uint64, payload []byte) [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if seq < r.next {
		return nil
	}
	if _, ok := r.buf[seq]; ok {
		return nil
	}
	r.buf[seq] = payload
	var out [][]byte
	for {
		p, ok := r.buf[r.next]
		if !ok {
			break
		}
		out = append(out, p)
		delete(r.buf, r.next)
		r.next++
	}
	return out
}

func (r *reorder) peekNext() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.next
}

// ---------- 单条路径 ----------

type path struct {
	conn net.Conn
	addr string
	prio int
	ch   chan []byte
	once sync.Once
	done chan struct{}

	// 质量遥测（用于动态选路，避免把大包发给卡住的路径）
	rttUs     atomic.Int64 // 平滑 RTT（微秒），0=未知
	baseUs    atomic.Int64 // 该路径 RTT 基线（历史最小值，缓慢上调）＝排队基准
	lastUs    atomic.Int64 // 最近一次往返样本（微秒，未平滑，用于尖峰判定）
	jitterUs  atomic.Int64 // 平滑抖动（相邻 RTT 差的绝对值，微秒）
	lossPct   atomic.Int64 // 近似丢包率 0~100
	pingsOut  atomic.Int64 // 已发出、尚未回收的 ping 数
	pongSeen  atomic.Int64 // 累计收到的 pong 数（0=对端可能不支持，视为未知不报丢包）
	lossTicks atomic.Int64 // 连续 100% 丢包秒数（达阈值强制重连）
	sent      atomic.Int64 // 成功入队帧数
	dropped   atomic.Int64 // 队列满丢弃次数

	histMu sync.Mutex
	hist   []int64 // 最近若干次 RTT 样本（微秒），用于路径相关性检测
}

func newPath(c net.Conn, addr string, prio int) *path {
	return &path{conn: c, addr: addr, prio: prio, ch: make(chan []byte, pathQueue), done: make(chan struct{})}
}

// quality 路径质量分（越小越好，单位近似微秒）：RTT + 抖动 + 队列积压 + 丢包惩罚。
func (p *path) quality() int64 {
	rtt := p.rttUs.Load()
	if rtt <= 0 {
		rtt = 150_000 // 未知暂按 150ms 估计
	}
	q := int64(len(p.ch)) * 1000 // 每积压一帧约 +1ms
	return rtt + 2*p.jitterUs.Load() + q + p.lossPct.Load()*2000
}

// onPong 收到本路径 pong：回收在途 ping、更新平滑 RTT
func (p *path) onPong(pl []byte) {
	p.pongSeen.Add(1)
	if v := p.pingsOut.Add(-1); v < 0 {
		p.pingsOut.Store(0)
	}
	if len(pl) < 8 {
		return
	}
	ts := int64(binary.BigEndian.Uint64(pl[:8]))
	sampleNs := time.Now().UnixNano() - ts
	if sampleNs <= 0 || sampleNs > int64(10*time.Second) {
		return
	}
	sample := sampleNs / 1000 // ns → µs
	p.lastUs.Store(sample)
	old := p.rttUs.Load()
	if old <= 0 {
		p.rttUs.Store(sample)
	} else {
		dev := sample - old
		if dev < 0 {
			dev = -dev
		}
		oj := p.jitterUs.Load()
		if oj == 0 {
			p.jitterUs.Store(dev)
		} else {
			p.jitterUs.Store((oj*3 + dev) / 4)
		}
		p.rttUs.Store((old*3 + sample) / 4)
	}
	// 更新基线：向下立即取最小值；向上极缓慢（适配路由永久变化，不把偶发低值当基线）
	if b := p.baseUs.Load(); b <= 0 || sample < b {
		p.baseUs.Store(sample)
	} else {
		p.baseUs.Store(b + (sample-b)/200)
	}
	p.histMu.Lock()
	p.hist = append(p.hist, sample)
	if len(p.hist) > 8 {
		p.hist = p.hist[len(p.hist)-8:]
	}
	p.histMu.Unlock()
}

// rttMs 平滑 RTT（毫秒），0=未知
func (p *path) rttMs() float64 { return float64(p.rttUs.Load()) / 1000.0 }

// queueMs 排队抬升（毫秒）= 平滑 RTT − 该路径基线；>0 表示链路在排队（bufferbloat 指标）。
func (p *path) queueMs() float64 {
	base := p.baseUs.Load()
	rtt := p.rttUs.Load()
	if base <= 0 || rtt <= base {
		return 0
	}
	return float64(rtt-base) / 1000.0
}

// rttSeries 返回最近若干次 RTT 样本（微秒）的副本
func (p *path) rttSeries() []int64 {
	p.histMu.Lock()
	defer p.histMu.Unlock()
	return append([]int64(nil), p.hist...)
}

func (p *path) close() {
	p.once.Do(func() {
		close(p.done)
		_ = p.conn.Close()
	})
}

func (p *path) writer() {
	for {
		select {
		case <-p.done:
			return
		case frame := <-p.ch:
			if _, err := p.conn.Write(frame); err != nil {
				p.close()
				return
			}
		}
	}
}

func (p *path) trySend(frame []byte) bool {
	select {
	case p.ch <- frame:
		p.sent.Add(1)
		return true
	case <-p.done:
		return true
	default:
		p.dropped.Add(1)
		return false
	}
}

func (p *path) blockSend(frame []byte) bool {
	select {
	case p.ch <- frame:
		return true
	case <-p.done:
		return false
	}
}

// ---------- 广播器 ----------

type broadcaster struct {
	mu        sync.Mutex
	paths     []*path
	stallBias atomic.Int64 // 近期卡顿次数（在线自适应：越有卡顿越积极冗余）
}

func (b *broadcaster) add(p *path) { b.mu.Lock(); b.paths = append(b.paths, p); b.mu.Unlock() }
func (b *broadcaster) count() int  { b.mu.Lock(); defer b.mu.Unlock(); return len(b.paths) }

func (b *broadcaster) remove(p *path) {
	b.mu.Lock()
	out := b.paths[:0]
	for _, x := range b.paths {
		if x != p {
			out = append(out, x)
		}
	}
	b.paths = out
	b.mu.Unlock()
}

func (b *broadcaster) snapshot() []*path {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*path(nil), b.paths...)
}

// sendNoBlock 非阻塞发送（用于 ACK 等可容忍丢失的小控制帧）。
func (b *broadcaster) sendNoBlock(frame []byte) {
	for _, p := range b.snapshot() {
		p.trySend(frame)
	}
}

// byQuality 按当前质量（RTT+积压+丢包，越小越好）升序返回路径快照；同分按原始序号。
func (b *broadcaster) byQuality() []*path {
	ps := b.snapshot()
	sort.SliceStable(ps, func(i, j int) bool {
		qi, qj := ps[i].quality(), ps[j].quality()
		if qi != qj {
			return qi < qj
		}
		return ps[i].prio < ps[j].prio
	})
	return ps
}

// eligible 只保留「与最优路径延迟接近」的路径用于冗余；慢很多的路只作故障切换，
// 不参与复制/重排（避免慢路径把整条流拖住）。
func (b *broadcaster) eligible(paths []*path) []*path {
	best := paths[0]
	out := []*path{best}
	br := best.rttMs()
	if br <= 0 {
		return out
	}
	for i := 1; i < len(paths); i++ {
		r := paths[i].rttMs()
		if r > 0 && r <= br*1.5+20 { // 最优的 1.5 倍 + 20ms 以内
			out = append(out, paths[i])
		}
	}
	return out
}

// pathCorrelation 两条路径最近 RTT 变化的 Pearson 相关系数（1=完全同步，0=无关）。
func pathCorrelation(a, b *path) float64 {
	as, bs := a.rttSeries(), b.rttSeries()
	n := len(as)
	if len(bs) < n {
		n = len(bs)
	}
	if n < 4 {
		return 0 // 样本不足，按独立处理
	}
	as, bs = as[len(as)-n:], bs[len(bs)-n:]
	var sa, sb, saa, sbb, sab float64
	for i := 0; i < n; i++ {
		xa, xb := float64(as[i]), float64(bs[i])
		sa += xa
		sb += xb
		saa += xa * xa
		sbb += xb * xb
		sab += xa * xb
	}
	nf := float64(n)
	cov := sab/nf - (sa/nf)*(sb/nf)
	va := saa/nf - (sa/nf)*(sa/nf)
	vb := sbb/nf - (sb/nf)*(sb/nf)
	if va <= 0 || vb <= 0 {
		return 0
	}
	return cov / math.Sqrt(va*vb)
}

// pathsCorrelated 两条路径是否高度正相关（相关=共用同一瓶颈）
func pathsCorrelated(a, b *path, thr float64) bool {
	return pathCorrelation(a, b) >= thr
}

// shared 两条路径是否共用同一瓶颈（RTT 同步变化，例如同一 ISP 骨干/同一接入段）。
func (b *broadcaster) shared(paths []*path) bool {
	return len(paths) >= 2 && pathCorrelation(paths[0], paths[1]) >= correlationThreshold
}

// selfBacklog 我们自己的发送队列是否积压（=我们把该路发满了，是真带宽瓶颈）。
// 与「外部排队」区分：外部拥塞（晚高峰骨干）不该关复制；自己的发满了才该关。
func (b *broadcaster) selfBacklog(paths []*path) bool {
	for _, p := range paths {
		if len(p.ch) > 48 {
			return true
		}
	}
	return false
}

// lossyOrJittery 是否存在随机丢包或明显抖动（有丢包/抖动时复制才真正有价值）。
func (b *broadcaster) lossyOrJittery(paths []*path) bool {
	jt := weakJitterUs.Load()
	if jt <= 0 {
		jt = defaultJitterUs
	}
	for _, p := range paths {
		if p.lossPct.Load() > 0 {
			return true
		}
	}
	return len(paths) > 0 && paths[0].jitterUs.Load() > jt
}

// hasLoss 是否存在真实丢包（大包只有在丢包时才值得多发，避免把吞吐翻倍）
func (b *broadcaster) hasLoss(paths []*path) bool {
	for _, p := range paths {
		if p.lossPct.Load() > 0 {
			return true
		}
	}
	return false
}

// queueHigh 最优路径相对自身基线排队明显（2×阈值）——大包复制会让吞吐翻倍，此时先别复制。
func (b *broadcaster) queueHigh(paths []*path) bool {
	if len(paths) == 0 {
		return false
	}
	thr := queueRiseUs.Load()
	if thr <= 0 {
		thr = defaultQueueRiseUs
	}
	best := paths[0]
	return best.baseUs.Load() > 0 && best.rttUs.Load()-best.baseUs.Load() > 2*thr
}

// send 通用自适应冗余（自校准、fail-safe）：
//
//	① 近延迟过滤：只在与最优路径延迟接近的路径上考虑冗余（慢路只作故障切换）；
//	② 关复制（只发最优一条）：两路共用瓶径 / 我们自己把某路发满（队列积压）/ 配置强制单线。
//	   注意：外部拥塞（晚高峰骨干）但两路独立时【不关】——复制能互相掩盖各自排队。
//	③ 开复制（两路独立）：关键小包全发（廉价保险）；普通包在丢包/抖动时多发；大包仅在丢包时补一条。
func (b *broadcaster) send(frame []byte) {
	paths := b.byQuality()
	if len(paths) == 0 {
		return
	}
	elig := b.eligible(paths)
	if len(elig) == 0 {
		elig = paths[:1]
	}
	suppress := failoverOnly.Load()
	if autoRedundancy.Load() && (b.shared(elig) || b.selfBacklog(paths)) {
		suppress = true
	}
	sendSet := elig[:1]
	if !suppress && len(elig) > 1 {
		n := len(frame)
		k := 1
		switch {
		case n <= tinyFrameThreshold:
			k = len(elig) // 关键小包：代价极小、当保险
		case n <= smallFrameThreshold:
			if b.lossyOrJittery(elig) {
				k = redundantPaths
			}
		default:
			k = int(largeFramePaths.Load())
			if k < 1 {
				k = 1
			}
			// 大包复制会让该路吞吐翻倍：仅在有丢包、且该路未明显排队时补一条
			if b.hasLoss(elig) && k < 2 && !b.queueHigh(elig) {
				k = 2
			}
		}
		if k > len(elig) {
			k = len(elig)
		}
		if k < 1 {
			k = 1
		}
		sendSet = elig[:k]
	}
	sent := 0
	for _, p := range sendSet {
		if p.trySend(frame) {
			sent++
		}
	}
	if sent > 0 {
		return
	}
	for _, p := range paths {
		if p.blockSend(frame) {
			return
		}
	}
}

// ---------- 端点 ----------

type pendFrame struct {
	frame  []byte
	sentAt time.Time
	tries  int
}

type endpoint struct {
	local net.Conn
	bc    *broadcaster
	ro    *reorder
	addrs []string

	dmu sync.Mutex // 串行化「按序取出 + 写回本地」

	smu      sync.Mutex
	next     uint64
	ackBase  uint64
	ackSince time.Time // ackBase 上次前进的时间（用于快速重传：ACK 停滞判定）
	pending  map[uint64]*pendFrame
	freed    chan struct{}

	ackMu   sync.Mutex
	lastAck time.Time

	lastUp atomic.Int64
	once   sync.Once
	closed chan struct{}

	statMaxPending atomic.Int64
	statRetrans    atomic.Int64
	spikeActive    bool // 是否处于一次尖峰事件中（上升沿计数，仅 qualityLoop 访问）
}

func newEndpoint(local net.Conn) *endpoint {
	e := &endpoint{
		local:   local,
		bc:      &broadcaster{},
		ro:      newReorder(),
		pending: make(map[uint64]*pendFrame),
		freed:   make(chan struct{}, 1),
		closed:  make(chan struct{}),
	}
	e.lastUp.Store(time.Now().UnixNano())
	return e
}

func (e *endpoint) addPath(p *path, br *bufio.Reader) {
	if br == nil {
		br = bufio.NewReader(p.conn)
	}
	e.bc.add(p)
	e.lastUp.Store(time.Now().UnixNano())
	go p.writer()
	go e.readPath(p, br)
}

func (e *endpoint) readPath(p *path, br *bufio.Reader) {
	for {
		typ, seq, pl, err := readFrame(br)
		if err != nil {
			Warnf("multipath：路径 %s 断开：%v", p.addr, err)
			p.close()
			e.bc.remove(p)
			OnPathDown(p.addr)
			return
		}
		switch typ {
		case frameData:
			if err := e.deliver(seq, pl); err != nil {
				Warnf("multipath：写入本地失败：%v", err)
				e.shutdown("本地写入失败")
				return
			}
		case frameAck:
			e.onAck(seq)
		case framePing:
			// 原路回显时间戳，供对端测量本路径 RTT
			p.trySend(encodeFrame(framePong, 0, pl))
		case framePong:
			p.onPong(pl)
		case frameBye:
			e.shutdown("对端 Bye")
			return
		}
	}
}

// deliver 按序取出并写回本地：整段持锁，保证多路径并发下不乱序。
func (e *endpoint) deliver(seq uint64, pl []byte) error {
	e.dmu.Lock()
	defer e.dmu.Unlock()
	for _, chunk := range e.ro.push(seq, pl) {
		if _, err := e.local.Write(chunk); err != nil {
			return err
		}
	}
	if e.ro.peekNext() != 0 {
		e.maybeAck()
	}
	return nil
}

func (e *endpoint) maybeAck() {
	now := time.Now()
	e.ackMu.Lock()
	if now.Sub(e.lastAck) < ackMinInterval {
		e.ackMu.Unlock()
		return
	}
	e.lastAck = now
	next := e.ro.peekNext()
	e.ackMu.Unlock()
	e.bc.sendNoBlock(encodeFrame(frameAck, next, nil))
}

func (e *endpoint) onAck(ack uint64) {
	e.smu.Lock()
	advanced := false
	for e.ackBase < ack {
		delete(e.pending, e.ackBase)
		e.ackBase++
		advanced = true
	}
	if advanced {
		e.ackSince = time.Now()
	}
	e.smu.Unlock()
	select {
	case e.freed <- struct{}{}:
	default:
	}
}

func (e *endpoint) pump() {
	defer e.shutdown("本地流结束")
	buf := make([]byte, chunkSize)
	blocked := false
	var sent int64
	for {
		for {
			e.smu.Lock()
			n := len(e.pending)
			e.smu.Unlock()
			if int64(n) > e.statMaxPending.Load() {
				e.statMaxPending.Store(int64(n))
			}
			if n < maxPending {
				blocked = false
				break
			}
			if !blocked {
				Warnf("multipath：发送背压（未确认 %d 帧，路径 %d 条）", n, e.bc.count())
				blocked = true
			}
			select {
			case <-e.freed:
			case <-e.closed:
				return
			}
		}
		n, err := e.local.Read(buf)
		if n > 0 {
			e.smu.Lock()
			if len(e.pending) == 0 {
				e.ackSince = time.Now()
			}
			seq := e.next
			e.next++
			frame := encodeFrame(frameData, seq, buf[:n])
			e.pending[seq] = &pendFrame{frame: frame, sentAt: time.Now()}
			e.smu.Unlock()
			sent += int64(n)
			e.bc.send(frame)
		}
		if err != nil {
			Warnf("multipath：本地连接读取结束：%v（本会话已发送 %d 字节）", err, sent)
			return
		}
	}
}

func (e *endpoint) retransmitLoop() {
	t := time.NewTicker(retransmitInterval)
	defer t.Stop()
	for {
		select {
		case <-e.closed:
			return
		case now := <-t.C:
			e.smu.Lock()
			budget := 64 // 每 100ms 最多补发 64 帧，避免恢复瞬间洪泛
			var resend [][]byte
			// 快速重传（方向正确）：ACK 长期停滞 = 首个未确认帧丢了 → 立即补发 ackBase
			if pf, ok := e.pending[e.ackBase]; ok && now.Sub(e.ackSince) >= fastRetransmitWait {
				pf.sentAt = now
				resend = append(resend, pf.frame)
				budget--
				e.ackSince = now      // 节流，避免连续刷屏
				e.bc.stallBias.Add(1) // 记录一次卡顿，触发更积极的冗余
			}
			// 超时重传：按序号升序（优先补齐最旧的空洞），限流避免洪泛
			seqs := make([]uint64, 0, len(e.pending))
			for s := range e.pending {
				seqs = append(seqs, s)
			}
			sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
			for _, s := range seqs {
				if budget <= 0 {
					break
				}
				pf := e.pending[s]
				shift := pf.tries
				if shift > 6 {
					shift = 6
				}
				rto := retransmitRTO << uint(shift)
				if rto > retransmitRTOMax {
					rto = retransmitRTOMax
				}
				if now.Sub(pf.sentAt) >= rto {
					pf.sentAt = now
					pf.tries++
					resend = append(resend, pf.frame)
					budget--
				}
			}
			e.smu.Unlock()
			if len(resend) > 0 {
				e.statRetrans.Add(int64(len(resend)))
			}
			for _, f := range resend {
				e.bc.send(f)
			}
		}
	}
}

// qualityLoop 周期性对每条路径发 ping 并回收 pong，更新 RTT/丢包并回报上层；
// 每 10 秒输出一次会话链路质量日志（便于定位是隧道重传还是线路本身的问题）。
func (e *endpoint) qualityLoop() {
	t := time.NewTicker(keepaliveInterval)
	defer t.Stop()
	tick := 0
	for {
		select {
		case <-e.closed:
			return
		case <-t.C:
			tick++
			for _, p := range e.bc.snapshot() {
				// 先按上一周期在途 ping 估计丢包并回报（对端从未回 pong 时视为未知，不报丢包）
				out := p.pingsOut.Load()
				loss := int64(0)
				if p.pongSeen.Load() > 0 {
					loss = out * 20 // 5 个未回收 = 100%
					if loss > 100 {
						loss = 100
					}
				}
				p.lossPct.Store(loss)
				// 死链检测：连续多秒 100% 丢包（TCP 假活）→ 强制重连该路径
				if loss >= 100 {
					if p.lossTicks.Add(1) >= int64(pathDeadLossTicks) {
						Warnf("multipath：路径 %s 连续 %d 秒无响应，强制重连", p.addr, pathDeadLossTicks)
						p.close() // readPath 会触发 OnPathDown，reconnectLoop 自动重连
					}
				} else {
					p.lossTicks.Store(0)
				}
				// 窗口内最大 RTT（让状态屏能看到被中位数掩盖的尖峰）
				mx := 0.0
				for _, v := range p.rttSeries() {
					if ms := float64(v) / 1000.0; ms > mx {
						mx = ms
					}
				}
				OnPathQuality(p.addr, float64(p.rttUs.Load())/1000.0, float64(p.jitterUs.Load())/1000.0, mx, float64(loss), len(p.ch))
				// 再发本轮 ping（对端原样回显时间戳以测本路径 RTT）
				var b8 [8]byte
				binary.BigEndian.PutUint64(b8[:], uint64(time.Now().UnixNano()))
				if p.trySend(encodeFrame(framePing, 0, b8[:])) {
					p.pingsOut.Add(1)
				}
			}
			// 玩家可见尖峰：按各路径中的【最小往返】计（复制时取先到），而不是每条路各自算，
			// 避免把“被另一条路掩盖掉”的单路卡顿也当成玩家体验到的尖峰。
			if thr := spikeThresholdUs.Load(); thr > 0 {
				best := int64(0)
				bestAddr := ""
				for _, p := range e.bc.snapshot() {
					r := p.lastUs.Load()
					if r <= 0 {
						r = p.rttUs.Load()
					}
					if r > 0 && (best == 0 || r < best) {
						best, bestAddr = r, p.addr
					}
				}
				if best > thr {
					// 上升沿计数：一次卡顿（哪怕持续几秒、排空多个样本）只计 1 次
					if !e.spikeActive {
						e.spikeActive = true
						OnPathSpike(bestAddr, float64(best)/1000.0)
					}
				} else if best > 0 {
					e.spikeActive = false
				}
			}
			// 在线自适应：卡顿偏置随时间衰减（近期有卡顿 → 更积极冗余；长期平稳 → 回落）
			if v := e.bc.stallBias.Load(); v > 0 {
				e.bc.stallBias.Store(v / 2)
			}
			if tick%10 == 0 {
				e.smu.Lock()
				pend := len(e.pending)
				e.smu.Unlock()
				sum := ""
				for i, p := range e.bc.snapshot() {
					if i > 0 {
						sum += "  "
					}
					addr := p.addr
					if addr == "" {
						addr = "path"
					}
					sum += fmt.Sprintf("%s %.0fms/%.0f%% 队列%d", addr,
						float64(p.rttUs.Load())/1000.0, float64(p.lossPct.Load()), len(p.ch))
				}
				Debugf("multipath：会话质量 在途%d帧 累计重传%d 峰值在途%d | %s",
					pend, e.statRetrans.Load(), e.statMaxPending.Load(), sum)
				// 冗余决策透明化：任意网络都能自解释「为何复制/为何不复制」
				ps := e.bc.byQuality()
				if len(ps) >= 2 {
					elig := e.bc.eligible(ps)
					r := pathCorrelation(ps[0], ps[1])
					queueBest := 0.0
					if len(elig) > 0 {
						queueBest = elig[0].queueMs()
					}
					mode := "开启复制（两路独立）"
					why := fmt.Sprintf("相关性 %.2f 排队 %.0fms", r, queueBest)
					switch {
					case !autoRedundancy.Load() && failoverOnly.Load():
						mode = "强制单线（配置）"
					case len(elig) < 2:
						mode = "关闭复制（第二路延迟过差）"
					case autoRedundancy.Load() && e.bc.shared(elig):
						mode = "关闭复制（两路共用瓶颈）"
					case autoRedundancy.Load() && e.bc.selfBacklog(ps):
						mode = "关闭复制（本地发送积压=带宽瓶颈）"
					case queueBest > 0:
						why = fmt.Sprintf("相关性 %.2f 排队 %.0fms（外部拥塞但两路独立→仍复制）", r, queueBest)
					}
					Debugf("multipath：冗余决策=%s（%s）", mode, why)
				}
			}
		}
	}
}

func (e *endpoint) monitorLoop() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-e.closed:
			return
		case <-t.C:
			if e.bc.count() > 0 {
				e.lastUp.Store(time.Now().UnixNano())
				continue
			}
			since := time.Since(time.Unix(0, e.lastUp.Load()))
			if since > noPathGrace {
				Warnf("multipath：已 %.0f 秒无可用路径，结束会话", since.Seconds())
				e.shutdown("无可用路径超时")
				return
			}
		}
	}
}

func (e *endpoint) shutdown(reason string) {
	e.once.Do(func() {
		Warnf("multipath：会话结束（%s）—— 峰值未确认 %d 帧，累计重传 %d 帧",
			reason, e.statMaxPending.Load(), e.statRetrans.Load())
		bye := encodeFrame(frameBye, 0, nil)
		for _, p := range e.bc.snapshot() {
			p.trySend(bye)
		}
		time.Sleep(80 * time.Millisecond)
		close(e.closed)
		_ = e.local.Close()
		for _, p := range e.bc.snapshot() {
			p.close()
		}
	})
}

// ---------- 客户端会话 ----------

func RunClientSession(sessionID uint64, local net.Conn, endpoints []string) {
	e := newEndpoint(local)
	e.addrs = endpoints
	tok := currentClientToken()
	hello := make([]byte, 9+len(tok))
	binary.BigEndian.PutUint64(hello[:8], sessionID)
	hello[8] = byte(len(tok))
	copy(hello[9:], tok)

	resCh := make(chan *path, len(endpoints))
	for i, ep := range endpoints {
		go func(ep string, prio int) {
			p, err := dialPath(ep, hello, prio)
			if err != nil {
				Warnf("multipath：连接路径 %s 失败：%v", ep, err)
				resCh <- nil
				return
			}
			resCh <- p
		}(ep, i)
	}
	go func() {
		for range endpoints {
			if p := <-resCh; p != nil {
				e.addPath(p, nil)
			}
		}
	}()

	go e.retransmitLoop()
	go e.monitorLoop()
	go e.qualityLoop()
	go e.reconnectLoop(hello)
	e.pump()
}

func dialPath(addr string, hello []byte, prio int) (*path, error) {
	c, err := dnscache.DialContext(context.Background(), "tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	p := newPath(c, addr, prio)
	if err := writeFrame(c, frameHello, 0, hello); err != nil {
		p.close()
		return nil, err
	}
	return p, nil
}

func (e *endpoint) reconnectLoop(hello []byte) {
	t := time.NewTicker(reconnectInterval)
	defer t.Stop()
	for {
		select {
		case <-e.closed:
			return
		case <-t.C:
			live := map[string]bool{}
			for _, p := range e.bc.snapshot() {
				live[p.addr] = true
			}
			for i, addr := range e.addrs {
				if live[addr] {
					continue
				}
				p, err := dialPath(addr, hello, i)
				if err != nil {
					continue
				}
				e.addPath(p, nil)
				Debugf("multipath：路径 %s 已（重）连", addr)
			}
		}
	}
}

// ---------- 服务端 Agent ----------

type agentRegistry struct {
	mu       sync.Mutex
	sessions map[uint64]*endpoint
	mcAddr   string
}

func RunAgent(listen, mcAddr string) error {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	return RunAgentListener(ln, mcAddr)
}

func RunAgentListener(ln net.Listener, mcAddr string) error {
	Debugf("multipath agent 监听 %s -> %s", ln.Addr(), mcAddr)
	reg := &agentRegistry{sessions: make(map[uint64]*endpoint), mcAddr: mcAddr}
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go reg.handleConn(c)
	}
}

func (r *agentRegistry) handleConn(c net.Conn) {
	br := bufio.NewReader(c)
	// 帧类型在长度前缀(4B)之后；不再硬编码长度（hello 现携带 token，长度可变）
	peek, err := br.Peek(5)
	if err != nil || peek[4] != frameHello {
		if !agentPassthrough.Load() {
			Debugf("multipath agent：拒绝非多线连接（agent_passthrough=false，来自 %s）", c.RemoteAddr())
			_ = c.Close()
			return
		}
		r.passThrough(c, br)
		return
	}
	typ, _, payload, err := readFrame(br)
	if err != nil || typ != frameHello || len(payload) < 9 {
		_ = c.Close()
		return
	}
	id := binary.BigEndian.Uint64(payload[:8])
	tokLen := int(payload[8])
	if 9+tokLen > len(payload) {
		_ = c.Close()
		return
	}
	gotTok := string(payload[9 : 9+tokLen])
	if want := currentAgentToken(); want != "" && gotTok != want {
		Warnf("multipath agent：会话 %d 鉴权失败（token 不匹配），拒绝连接", id)
		_ = c.Close()
		return
	}

	var ep *endpoint
	r.mu.Lock()
	ep = r.sessions[id]
	if ep == nil {
		mc, derr := net.DialTimeout("tcp", r.mcAddr, 5*time.Second)
		if derr != nil {
			r.mu.Unlock()
			Warnf("multipath agent：会话 %d 连接 MC 失败：%v", id, derr)
			_ = c.Close()
			return
		}
		ep = newEndpoint(mc)
		r.sessions[id] = ep
		go ep.retransmitLoop()
		go ep.monitorLoop()
		go ep.qualityLoop()
		go func() { ep.pump(); r.drop(id) }()
	}
	r.mu.Unlock()

	p := newPath(c, "", 0)
	ep.addPath(p, br)
	Debugf("multipath agent：会话 %d 加入一条路径（共 %d 条）", id, ep.bc.count())
}

func (r *agentRegistry) drop(id uint64) {
	r.mu.Lock()
	delete(r.sessions, id)
	r.mu.Unlock()
}

func (r *agentRegistry) passThrough(c net.Conn, br *bufio.Reader) {
	defer c.Close()
	up, err := net.DialTimeout("tcp", r.mcAddr, 5*time.Second)
	if err != nil {
		Warnf("multipath agent：透传连接 MC 失败：%v", err)
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
}
