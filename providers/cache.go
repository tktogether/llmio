package providers

import (
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type clientKey struct {
	timeout time.Duration
	proxy   string
}

type clientCache struct {
	mu      sync.RWMutex
	clients map[clientKey]*http.Client
}

var cache = &clientCache{
	clients: make(map[clientKey]*http.Client),
}

var dialer = &net.Dialer{
	Timeout:   30 * time.Second,
	KeepAlive: 30 * time.Second,
}

// HTTP/2 活性探测参数。
//
// 不打开它的话，net/http 连健康检查的定时器都不会挂（h2_bundle.go 里只有
// SendPingTimeout 非 0 才起 readIdleTimer）。后果很具体：一条被对端或中间设备
// 悄悄丢弃的连接（既没 FIN 也没 RST）会一直留在连接池里，复用它的请求只能老老实实
// 等满 ResponseHeaderTimeout，报出来的正是那句无从下手的
// "http2: timeout awaiting response headers"——而超时后补发的那次 PING 没有计时器
// 看管，连接也不会因此被摘掉，于是一条死连接可以连续烧掉好几次完整超时。
//
// 15s 无帧发一次 PING：正常流式响应每个 chunk 都是一帧，只有真正静默的等待
// （上游排队/思考，或者连接已经死了）才会触发。PING ACK 由 RFC 9113 强制要求，
// 活着但不干活的上游照样会立刻回，所以它不会打断一个正在慢慢思考的请求；反过来，
// 一个 PingTimeout 内没有 ACK 就说明这条连接没救了，与其继续耗着不如立刻摘掉、
// 让重试换一条连接。顺带的好处是"上游慢"与"连接死了"从此是两种不同错误。
const (
	h2PingInterval = 15 * time.Second
	h2PingTimeout  = 15 * time.Second
)

// h2CountError 把 HTTP/2 连接层的关键事件落到控制台。
//
// 唯一值得出手的是 conn_close_lost_ping：PING 发出去没等到 ACK，说明这条连接
// 已经静默失联（对端消失、中间设备丢了状态），于是我们主动把它摘掉。这正是
// "http2: timeout awaiting response headers" 的另一半解释——没有它，日志里只看得
// 见有人超时，看不见连接其实早就死了。
// 其余几类（read_frame_eof 等）是对端正常关闭连接时的收尾，属于日常噪音，不记。
func h2CountError(responseHeaderTimeout time.Duration, proxyURL string) func(string) {
	return func(errType string) {
		if errType != "conn_close_lost_ping" {
			return
		}
		slog.Warn("HTTP/2 连接 PING 无应答，已摘除该连接",
			"response_header_timeout", responseHeaderTimeout.String(),
			"proxy", proxyURL)
	}
}

// GetClient returns an http.Client with the specified responseHeaderTimeout and proxy.
// If a client with the same timeout and proxy already exists, it returns the cached one.
// Otherwise, it creates a new client and caches it.
func GetClient(responseHeaderTimeout time.Duration, proxyURL string) *http.Client {
	key := clientKey{timeout: responseHeaderTimeout, proxy: proxyURL}

	cache.mu.RLock()
	if client, exists := cache.clients[key]; exists {
		cache.mu.RUnlock()
		return client
	}
	cache.mu.RUnlock()

	cache.mu.Lock()
	defer cache.mu.Unlock()

	// Double-check after acquiring write lock
	if client, exists := cache.clients[key]; exists {
		return client
	}

	proxyFunc := http.ProxyFromEnvironment
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			proxyFunc = http.ProxyURL(u)
		}
	}

	transport := &http.Transport{
		Proxy:                 proxyFunc,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
		// 只有设了 HTTP2 才会把探测参数传给内建的 h2 transport，
		// 参数含义与取舍见上面的 h2PingInterval
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: h2PingInterval,
			PingTimeout:     h2PingTimeout,
			CountError:      h2CountError(responseHeaderTimeout, proxyURL),
		},
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   0, // No overall timeout, let ResponseHeaderTimeout control header timing
	}

	cache.clients[key] = client
	return client
}
