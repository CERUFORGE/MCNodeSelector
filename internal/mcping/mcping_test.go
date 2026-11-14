package mcping

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// TestPingMockServer 用本地 mock MC 服务器验证 SLP 协议实现（§7.6）
func TestPingMockServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		// 读取 Handshake 帧：VarInt(包长) + 包ID(0) + 协议版本 + 地址 + 端口 + 下一状态
		frameLen, err := readVarInt(br)
		if err != nil {
			return
		}
		// 丢弃 frameLen 字节的 Handshake 载荷
		io.CopyN(io.Discard, br, int64(frameLen))
		// 读取 Status Request 帧：VarInt(1) + 包ID(0)
		readVarInt(br) // 帧长 1
		readVarInt(br) // 包ID 0
		// 构造应答：{"version":{"name":"1.20","protocol":765},"players":{"max":20,"online":3}}
		body := []byte(`{"version":{"name":"1.20","protocol":765},"players":{"max":20,"online":3}}`)
		pkt := appendVarInt(appendVarInt(nil, 0), int32(len(body))) // pktID=0 + jsonLen
		pkt = append(pkt, body...)
		frame := appendVarInt(nil, int32(len(pkt)))
		frame = append(frame, pkt...)
		conn.Write(frame)
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	res, err := Ping("127.0.0.1", port, 3*time.Second)
	if err != nil {
		t.Fatalf("Ping 失败：%v", err)
	}
	if res.VersionName != "1.20" {
		t.Errorf("版本名 = %q，want 1.20", res.VersionName)
	}
	if res.Protocol != 765 {
		t.Errorf("协议 = %d，want 765", res.Protocol)
	}
	if res.PlayersOnline != 3 || res.PlayersMax != 20 {
		t.Errorf("玩家 = %d/%d，want 3/20", res.PlayersOnline, res.PlayersMax)
	}
	// 本地回环极快，RTT 可能为亚微秒级 → 只断言非负
	if res.RTTMs < 0 {
		t.Errorf("RTT 不应为负，got %f", res.RTTMs)
	}
}

// TestVarIntRoundTrip VarInt 编解码一致性（含 -1/0xFFFFFFFF 边界）
func TestVarIntRoundTrip(t *testing.T) {
	cases := []int32{0, 1, 127, 128, 255, 2147483647, -1}
	for _, v := range cases {
		buf := appendVarInt(nil, v)
		br := bufio.NewReader(bytes.NewReader(buf))
		got, err := readVarInt(br)
		if err != nil {
			t.Errorf("readVarInt(%d) err: %v", v, err)
			continue
		}
		if got != v {
			t.Errorf("readVarInt = %d, want %d", got, v)
		}
	}
	// -1 → 0xFFFFFFFF → 5 字节 FF FF FF FF 0F
	buf := appendVarInt(nil, -1)
	if !bytes.Equal(buf, []byte{0xff, 0xff, 0xff, 0xff, 0x0f}) {
		t.Errorf("-1 的 VarInt = %x，want ffffffffff0f", buf)
	}
}

// TestAddrTooLong 地址超长报错
func TestAddrTooLong(t *testing.T) {
	long := make([]byte, 256)
	for i := range long {
		long[i] = 'a'
	}
	_, err := Ping(string(long), 25565, time.Second)
	if err != ErrAddrTooLong {
		t.Errorf("超长地址应返回 ErrAddrTooLong，got %v", err)
	}
}

// TestRefused 端口拒绝连接 → 由 neterr 上抛（非 panic）
func TestRefused(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	_, err := Ping("127.0.0.1", port, time.Second)
	if err == nil {
		t.Fatal("拒绝连接应返回错误")
	}
	_ = fmt.Sprintf
	_ = io.EOF
}

// 确保二进制 BigEndian 引用不丢（编码用）
var _ = binary.BigEndian
