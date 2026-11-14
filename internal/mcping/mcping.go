// Package mcping 自实现 Java 版 Minecraft SLP（Server List Ping）协议（§7.6）。
// 只用标准库，不引第三方依赖。
//
// 协议流程：
//  1. TCP 连接 host:port（Dialer.Timeout）；
//  2. 连接后设置读写总超时（SetDeadline）；
//  3. 发送 Handshake（协议版本 -1 兼容 1.7+ 任意版本，下一状态 = Status）；
//  4. 发送 Status Request（0x01 0x00）；
//  5. 读取响应：VarInt 包长 → VarInt 包ID(=0) → VarInt JSON 长度 → JSON 正文；
//  6. RTT = 从发出第一个字节到读满 body 的耗时（应用层 RTT）。
package mcping

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// 协议级错误（§7.6 / F11.3），文案直接面向用户
var (
	// ErrMCTimeout 握手超时：对端可能不是 MC 服务器，或链路丢包严重
	ErrMCTimeout = errors.New("Minecraft 握手超时：对端可能不是 MC 服务器，或链路丢包严重")
	// ErrMCBadPacket 响应格式非法：对端可能不是 Java 版 MC 服务器
	ErrMCBadPacket = errors.New("Minecraft 响应格式非法：对端可能不是 Java 版 MC 服务器")
	// ErrAddrTooLong 服务器地址超过 SLP 协议 255 字节限制
	ErrAddrTooLong = errors.New("服务器地址过长（SLP 协议限制 255 字节）")
)

// maxPacketLen 响应包长上限 8MB（§7.6）
const maxPacketLen = 8 * 1024 * 1024

// Result SLP 应答解析结果
type Result struct {
	VersionName   string  // 服务端版本名（如 "1.20.4"）
	Protocol      int     // 协议版本号
	PlayersOnline int     // 当前在线人数
	PlayersMax    int     // 最大在线人数
	RTTMs         float64 // 应用层 RTT（毫秒）
}

// slpJSON SLP 应答 JSON 结构（§7.6）
type slpJSON struct {
	Version struct {
		Name     string `json:"name"`
		Protocol int    `json:"protocol"`
	} `json:"version"`
	Players struct {
		Max    int `json:"max"`
		Online int `json:"online"`
	} `json:"players"`
}

// appendVarInt 向缓冲写入 VarInt（每字节低 7 位为数据，最高位=1 表示后续还有，最多 5 字节）
func appendVarInt(buf []byte, v int32) []byte {
	u := uint32(v) // 必须按无符号处理（-1 → 0xFFFFFFFF）
	for {
		b := byte(u & 0x7F)
		u >>= 7
		if u != 0 {
			b |= 0x80
		}
		buf = append(buf, b)
		if u == 0 {
			break
		}
	}
	return buf
}

// readVarInt 从读取器解析 VarInt；超过 5 字节视为非法包
func readVarInt(r *bufio.Reader) (int32, error) {
	var result uint32
	for i := 0; i < 5; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		result |= uint32(b&0x7F) << (7 * uint(i))
		if b&0x80 == 0 {
			return int32(result), nil
		}
	}
	return 0, ErrMCBadPacket
}

// Ping 对 host:port 执行一次 SLP 探测。
// TCP 连接失败原样上抛（由 neterr 分类）；协议层错误返回本包定义的 ErrXXX。
func Ping(host string, port int, timeout time.Duration) (*Result, error) {
	if len(host) > 255 {
		return nil, ErrAddrTooLong
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.Dial("tcp", net.JoinHostPort(host, fmt.Sprintf("%d", port)))
	if err != nil {
		return nil, err // 交给 neterr 分类（DNS/拒绝/超时）
	}
	defer conn.Close()
	// 连接后总超时（读写共用一个 deadline）
	_ = conn.SetDeadline(time.Now().Add(timeout))

	// ---- 构造 Handshake 载荷（§7.6） ----
	payload := []byte{0x00}             // 包ID = Handshake
	payload = appendVarInt(payload, -1) // 协议版本 -1（兼容 1.7+ 任意版本）
	payload = appendVarInt(payload, int32(len(host)))
	payload = append(payload, host...) // 地址（UTF-8）
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], uint16(port)) // 2 字节无符号大端
	payload = append(payload, portBytes[:]...)
	payload = append(payload, 0x01) // 下一状态 = Status

	// 帧 = VarInt(载荷长) + 载荷
	frame := appendVarInt(nil, int32(len(payload)))
	frame = append(frame, payload...)

	// RTT 计时起点：即将发出第一个字节
	start := time.Now()
	if _, err := conn.Write(frame); err != nil {
		return nil, wrapIOErr(err)
	}
	// Status Request：帧长 1 + 包ID 0
	if _, err := conn.Write([]byte{0x01, 0x00}); err != nil {
		return nil, wrapIOErr(err)
	}

	// ---- 读取响应 ----
	br := bufio.NewReader(conn)
	pktLen, err := readVarInt(br)
	if err != nil {
		return nil, wrapIOErr(err)
	}
	if pktLen <= 0 || pktLen > maxPacketLen {
		return nil, ErrMCBadPacket
	}
	pktID, err := readVarInt(br)
	if err != nil {
		return nil, wrapIOErr(err)
	}
	if pktID != 0 {
		return nil, ErrMCBadPacket // 状态应答包 ID 必须为 0
	}
	jsonLen, err := readVarInt(br)
	if err != nil {
		return nil, wrapIOErr(err)
	}
	if jsonLen <= 0 || jsonLen > pktLen {
		return nil, ErrMCBadPacket
	}
	body := make([]byte, jsonLen)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, wrapIOErr(err)
	}
	rtt := float64(time.Since(start).Microseconds()) / 1000.0

	// ---- 解析 JSON ----
	var resp slpJSON
	if err := unmarshalBody(body, &resp); err != nil {
		return nil, ErrMCBadPacket
	}
	return &Result{
		VersionName:   resp.Version.Name,
		Protocol:      resp.Version.Protocol,
		PlayersOnline: resp.Players.Online,
		PlayersMax:    resp.Players.Max,
		RTTMs:         rtt,
	}, nil
}

// wrapIOErr 把读写阶段的 I/O 错误归类为超时或原样上抛
func wrapIOErr(err error) error {
	if err == nil {
		return nil
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("%w（%v）", ErrMCTimeout, err)
	}
	return err
}
