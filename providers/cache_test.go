package providers

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 健康检查参数是这个包唯一"看不见但会改变线上行为"的配置：一旦丢了，
// h2 连接就退化成"悄悄死掉也只能靠耗满 ResponseHeaderTimeout 才暴露"。
func TestGetClientEnablesHTTP2HealthCheck(t *testing.T) {
	t.Parallel()

	client := GetClient(31*time.Second, "")
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型 = %T，want *http.Transport", client.Transport)
	}
	if tr.HTTP2 == nil {
		t.Fatal("HTTP2 配置为空：h2 连接不会做任何活性探测")
	}
	// 期望值写成字面量而不是引用包内常量：这个用例的价值在于"在没打开探测的代码上
	// 能编译再失败"，引用常量会让它退化成编译错误。
	if tr.HTTP2.SendPingTimeout != 15*time.Second {
		t.Errorf("SendPingTimeout = %v, want 15s（空闲多久发 PING）", tr.HTTP2.SendPingTimeout)
	}
	if tr.HTTP2.PingTimeout != 15*time.Second {
		t.Errorf("PingTimeout = %v, want 15s（PING 多久没 ACK 判定连接已死）", tr.HTTP2.PingTimeout)
	}
	if tr.HTTP2.CountError == nil {
		t.Error("CountError 为空：连接被判定失联时不会有任何痕迹")
	}

	// 打开探测不能顺手改掉既有的超时/协议语义
	if tr.ResponseHeaderTimeout != 31*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want 31s", tr.ResponseHeaderTimeout)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 被关掉了：自定义 DialContext 下就不会走 h2 了")
	}
	if client.Timeout != 0 {
		t.Errorf("Client.Timeout = %v，应当为 0（由 ResponseHeaderTimeout 控制等响应头）", client.Timeout)
	}
}

func TestGetClientCachesByKey(t *testing.T) {
	t.Parallel()

	a := GetClient(32*time.Second, "")
	b := GetClient(32*time.Second, "")
	if a != b {
		t.Error("同 timeout/同代理应当复用同一个 client（否则每个模型都开一套连接池）")
	}

	c := GetClient(33*time.Second, "")
	if a == c {
		t.Error("timeout 不同应当是不同的 client")
	}

	d := GetClient(32*time.Second, "http://127.0.0.1:18080")
	if a == d {
		t.Error("代理不同应当是不同的 client")
	}
	tr := d.Transport.(*http.Transport)
	if tr.Proxy == nil {
		t.Fatal("指定了代理但 Transport.Proxy 为空")
	}
	u, err := tr.Proxy(nil)
	if err != nil || u == nil || u.String() != "http://127.0.0.1:18080" {
		t.Errorf("Proxy 解析结果 = %v, %v；want http://127.0.0.1:18080", u, err)
	}
}

// conn_close_lost_ping 是"连接静默失联"的唯一直接证据，必须留痕；
// 其余几类是对端正常关连接的收尾，记了只会淹没真问题。
func TestH2CountErrorLogsOnlyLostPing(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	report := h2CountError(20*time.Second, "http://127.0.0.1:18080")

	for _, quiet := range []string{
		"read_frame_eof",
		"read_frame_unexpected_eof",
		"read_frame_other",
		"read_frame_too_large",
		"some_future_type",
	} {
		report(quiet)
	}
	if buf.Len() != 0 {
		t.Fatalf("日常事件不该打日志：%s", buf.String())
	}

	report("conn_close_lost_ping")
	line := buf.String()
	for _, want := range []string{"PING 无应答", "response_header_timeout=20s", "proxy=http://127.0.0.1:18080"} {
		if !strings.Contains(line, want) {
			t.Fatalf("失联日志里没有 %q：%s", want, line)
		}
	}
}

// 代理地址写坏了不能让 GetClient 崩掉：退回到环境变量代理即可，
// 否则一条录错的 provider 配置会炸掉整个转发路径。
func TestGetClientToleratesUnparsableProxy(t *testing.T) {
	t.Parallel()

	client := GetClient(35*time.Second, "http://[::1")
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型 = %T，want *http.Transport", client.Transport)
	}
	if tr.Proxy == nil {
		t.Fatal("代理地址解析失败后应当退回环境变量代理，而不是留空")
	}
}
