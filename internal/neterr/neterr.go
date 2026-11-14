// Package neterr 把底层网络错误翻译成“可操作的中文提示”（F11.1 / §9）。
// 分类结果同时用于：节点状态机判定（OFFLINE vs TIMEOUT）与用户提示。
package neterr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
)

// 错误类别常量（供状态机使用，勿直接展示给用户）
const (
	ClassDNS      = "dns"      // DNS 解析失败
	ClassRefused  = "refused"  // 连接被拒绝
	ClassTimeout  = "timeout"  // 连接超时
	ClassCanceled = "canceled" // 主动取消（程序退出）
	ClassOther    = "other"    // 其他网络错误
)

// Classify 将 err 分类并返回中文可操作提示。
// class 用于状态机（refused→OFFLINE，其余→TIMEOUT），msg 直接面向用户。
func Classify(err error) (class, msg string) {
	if err == nil {
		return ClassOther, ""
	}
	// 主动取消（程序退出时中止测速），不算节点故障
	if errors.Is(err, context.Canceled) {
		return ClassCanceled, "操作已取消"
	}
	// DNS 解析失败：域名拼错或本机 DNS 异常
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ClassDNS, fmt.Sprintf("DNS 解析失败（%s）：请检查域名拼写或本机 DNS 设置", dnsErr.Name)
	}
	// 连接被拒绝：对端端口没开——最常见是 frps 没跑或安全组没放行
	if errors.Is(err, syscall.ECONNREFUSED) ||
		containsFold(err.Error(), "connection refused") ||
		containsFold(err.Error(), "actively refused") {
		return ClassRefused, "连接被拒绝：请检查 frps 是否已运行、云安全组/防火墙是否放行该端口"
	}
	// 超时：链路不通或丢包严重
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ClassTimeout, "连接超时：链路不通或丢包严重"
	}
	if errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, context.DeadlineExceeded) ||
		containsFold(err.Error(), "i/o timeout") {
		return ClassTimeout, "连接超时：链路不通或丢包严重"
	}
	return ClassOther, fmt.Sprintf("网络错误：%v", err)
}

// containsFold 不区分大小写的子串判断（错误消息匹配用）
func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
