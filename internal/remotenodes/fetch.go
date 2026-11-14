package remotenodes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"mcnode/internal/config"
)

// remoteWrap 包装格式 {"nodes": [...]}
type remoteWrap struct {
	Nodes []config.Node `json:"nodes"`
}

// Fetch 拉取远程节点列表。支持 http(s) URL、file:// 或本地文件路径。
// 网络失败、HTTP 非 2xx、JSON 解析失败均返回中文错误（由调用方决定是否回退本地）。
func Fetch(ctx context.Context, url string, timeout time.Duration) ([]config.Node, error) {
	if url == "" {
		return nil, fmt.Errorf("远程节点列表 URL 为空，请在配置 remote.url 中填写")
	}
	// 非 http(s)：优先当本地文件；不存在且像域名则补 http:// 当订阅
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		p := strings.TrimPrefix(url, "file://")
		if _, err := os.Stat(p); err == nil {
			body, e := os.ReadFile(p)
			if e != nil {
				return nil, fmt.Errorf("读取本地节点文件失败（%s）：%w", p, e)
			}
			return parseNodes(body)
		}
		if strings.Contains(url, ".") && !strings.ContainsAny(url, " \\/") {
			url = "http://" + url // 裸域名 → 订阅 URL
		} else {
			return nil, fmt.Errorf("读取本地节点文件失败：%s 不存在", p)
		}
	}
	body, err := fetchHTTP(ctx, url, timeout)
	if err != nil {
		return nil, err
	}
	return parseNodes(body)
}

// fetchHTTP 拉取 URL；失败时在 http ↔ https 间自动回退一次。
func fetchHTTP(ctx context.Context, url string, timeout time.Duration) ([]byte, error) {
	try := func(u string) ([]byte, error) {
		c, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(c, "GET", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "MCNodeSelector/1.0")
		req.Header.Set("Accept", "application/json, text/plain")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	}
	body, err := try(url)
	if err == nil {
		return body, nil
	}
	var alt string
	if strings.HasPrefix(url, "http://") {
		alt = "https://" + strings.TrimPrefix(url, "http://")
	} else {
		alt = "http://" + strings.TrimPrefix(url, "https://")
	}
	if b2, e2 := try(alt); e2 == nil {
		return b2, nil
	}
	return nil, fmt.Errorf("拉取节点列表失败（%s）：%w", url, err)
}

// parseNodes 解析节点 JSON（纯数组或 {"nodes":[...]}，允许注释）
func parseNodes(body []byte) ([]config.Node, error) {
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF}) // 去掉可能的 UTF-8 BOM
	// 先剥离 JSONC 注释（远程文件也可能是带注释的 .jsonc）
	stripped := config.StripComments(body)
	// 尝试包装格式 {"nodes":[...]}
	var wrap remoteWrap
	if err := json.Unmarshal(stripped, &wrap); err == nil && wrap.Nodes != nil {
		return wrap.Nodes, nil
	}
	// 退化为纯数组格式 [...]
	var arr []config.Node
	if err := json.Unmarshal(stripped, &arr); err != nil {
		return nil, fmt.Errorf("远程节点列表 JSON 解析失败：%w（支持纯数组或 {\"nodes\":[...]} 两种格式）", err)
	}
	return arr, nil
}
