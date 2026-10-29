package providers

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 线上那句 http2: timeout awaiting response headers 的最小复现。
//
// 两件事一起钉住：
//  1. GetClient 造出来的 transport 真的会走 HTTP/2（否则线上永远见到的是 HTTP/1.1 文案，
//     service/upstream_diag.go 的归类与 stats 的归类都白写）；
//  2. 上游迟迟不发响应头时，错误文案就是那句 h2 原文——归类与统计都是按这句字面量
//     匹配的，哪天 Go 改了措辞，这里先红。
//
// 第二个请求刻意发往同一台服务器：走的就是"复用连接池里那条 h2 连接"的生产形态。
func TestHTTP2HeaderTimeoutReproducesProductionError(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/warm", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/slow", func(_ http.ResponseWriter, _ *http.Request) {
		<-release // 一个字节的响应头都不发，模拟排队中的上游
	})

	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	defer close(release)

	// 唯一的 timeout 值：client 是按 (timeout, proxy) 缓存的，用特有取值避免串到别的用例
	client := GetClient(137*time.Millisecond, "")
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型 = %T，want *http.Transport", client.Transport)
	}
	// 只改这一条连接池的证书信任，测试服务器的证书不在系统根里
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 仅仅为了让测试服务器可连

	get := func(path string) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatalf("建请求失败：%v", err)
		}
		return client.Do(req)
	}

	res, err := get("/warm")
	if err != nil {
		t.Fatalf("预热请求失败：%v", err)
	}
	_, _ = res.Body.Read(make([]byte, 1))
	res.Body.Close()
	if res.ProtoMajor != 2 {
		t.Fatalf("协商到的是 %s，want HTTP/2（ForceAttemptHTTP2 或 ALPN 出问题了）", res.Proto)
	}

	start := time.Now()
	res, err = get("/slow")
	if err == nil {
		res.Body.Close()
		t.Fatal("上游不发响应头，却拿到了响应")
	}
	elapsed := time.Since(start)

	if !strings.Contains(err.Error(), "http2: timeout awaiting response headers") {
		t.Fatalf("错误文案变了（归类规则要跟着改）：%v", err)
	}
	if elapsed < 137*time.Millisecond {
		t.Fatalf("只等了 %v 就报错，ResponseHeaderTimeout 没生效", elapsed)
	}
}
