package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/atopos31/llmio/consts"
	"github.com/atopos31/llmio/models"
	"gorm.io/gorm"
)

func sampleAttempt() UpstreamAttempt {
	return NewUpstreamAttempt(models.ChatLog{
		TraceID:       "tid-1",
		ProviderName:  "commandcode",
		ProviderModel: "deepseek/deepseek-v4.1-flash",
		Style:         "openai",
		UpstreamStyle: "openai",
		Retry:         2,
		Backup:        true,
	}, true, 60, 20*time.Second)
}

// NewUpstreamAttempt 从 ChatLog 快照取字段：三者描述同一次尝试，字段对不上就等于
// 日志把人领到别的请求上。
func TestNewUpstreamAttemptMapsLogSnapshot(t *testing.T) {
	t.Parallel()

	a := sampleAttempt()
	if a.TraceID != "tid-1" || a.Provider != "commandcode" || a.ProviderModel != "deepseek/deepseek-v4.1-flash" {
		t.Fatalf("标识字段没跟 ChatLog 对上：%+v", a)
	}
	if a.UpstreamStyle != "openai" || !a.Stream || a.Retry != 2 || !a.Backup {
		t.Fatalf("尝试上下文不对：%+v", a)
	}
	if a.TimeOut != 60*time.Second || a.HeaderTimeout != 20*time.Second {
		t.Fatalf("超时字段不对：time_out=%v header_timeout=%v", a.TimeOut, a.HeaderTimeout)
	}
}

func TestHeaderTimeoutKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", err: nil, want: ""},
		{
			name: "h2 真机原文",
			err:  &url.Error{Op: "Post", URL: "https://api.commandcode.ai/provider/v1/chat/completions", Err: errors.New("http2: timeout awaiting response headers")},
			want: failCodeHeaderTimeoutH2,
		},
		{
			name: "h1 真机原文",
			err:  &url.Error{Op: "Post", URL: "https://api.example.com/x", Err: errors.New("net/http: timeout awaiting response headers")},
			want: failCodeHeaderTimeoutH1,
		},
		{name: "别的 headers 字眼不算", err: errors.New("Client.Timeout exceeded while awaiting headers"), want: ""},
		{name: "无关错误", err: errors.New("boom"), want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := headerTimeoutKind(tc.err); got != tc.want {
				t.Fatalf("headerTimeoutKind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassifyUpstreamFailure(t *testing.T) {
	t.Parallel()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name   string
		reqCtx context.Context
		err    error
		want   string
	}{
		{name: "nil 错误归其他", reqCtx: context.Background(), err: nil, want: failCodeOther},
		{
			name:   "等响应头超时先于通用超时判定",
			reqCtx: context.Background(),
			err:    errors.New("http2: timeout awaiting response headers"),
			want:   failCodeHeaderTimeoutH2,
		},
		{
			name:   "客户端断开",
			reqCtx: canceled,
			err:    fmt.Errorf("Post %q: %w", "https://x", context.Canceled),
			want:   failCodeClientCancel,
		},
		{
			// ctx 未取消的 cancel 不算"客户端跑了"，应落到其他
			name:   "无请求侧取消证据的 cancel 不归客户端",
			reqCtx: context.Background(),
			err:    fmt.Errorf("Post %q: %w", "https://x", context.Canceled),
			want:   failCodeOther,
		},
		{name: "拨号失败", reqCtx: context.Background(), err: errors.New("dial tcp 1.2.3.4:443: connect: connection refused"), want: failCodeDial},
		{name: "TLS 失败", reqCtx: context.Background(), err: errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority"), want: failCodeTLS},
		{name: "整体超时", reqCtx: context.Background(), err: fmt.Errorf("Post: %w", context.DeadlineExceeded), want: failCodeTimeoutOther},
		{name: "无特征归其他", reqCtx: context.Background(), err: errors.New("something inexplicable happened"), want: failCodeOther},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyUpstreamFailure(tc.reqCtx, tc.err); got != tc.want {
				t.Fatalf("classifyUpstreamFailure = %q, want %q", got, tc.want)
			}
		})
	}
}

// Annotate 只动"等响应头超时"这一类：其它错误的文案已经够用，
// 顺手改文本会让既有日志的检索口径和断言一起漂移。
func TestAnnotateLeavesOtherErrorsAlone(t *testing.T) {
	t.Parallel()

	a := sampleAttempt()
	plain := errors.New("dial tcp 1.2.3.4:443: connect: connection refused")
	if got := a.Annotate(plain, &ConnFacts{}); got != plain {
		t.Fatalf("非等响应头超时被改写：%v", got)
	}
	if got := a.Annotate(nil, &ConnFacts{}); got != nil {
		t.Fatalf("nil 错误应当原样返回，got %v", got)
	}
}

func TestAnnotateAddsHeaderTimeoutFacts(t *testing.T) {
	t.Parallel()

	errTimeout := errors.New("http2: timeout awaiting response headers")

	fresh := &ConnFacts{}
	fresh.record(httptrace.GotConnInfo{})

	reused := &ConnFacts{}
	reused.record(httptrace.GotConnInfo{Reused: true})

	idle := &ConnFacts{}
	idle.record(httptrace.GotConnInfo{Reused: true, WasIdle: true, IdleTime: 87 * time.Second})

	tests := []struct {
		name  string
		facts *ConnFacts
		want  string
	}{
		{name: "没抓到连接信息", facts: &ConnFacts{}, want: "conn=unknown"},
		{name: "nil 事实不 panic", facts: nil, want: "conn=unknown"},
		{name: "新建连接", facts: fresh, want: "conn=fresh"},
		{name: "复用连接", facts: reused, want: "conn=reused)"},
		{name: "复用且空闲", facts: idle, want: "conn=reused,idle=1m27s"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sampleAttempt().Annotate(errTimeout, tc.facts).Error()
			if !strings.Contains(got, tc.want) {
				t.Fatalf("错误文本 %q 里没有 %q", got, tc.want)
			}
			if !strings.Contains(got, "stream=true") || !strings.Contains(got, "header_timeout=20s") {
				t.Fatalf("错误文本 %q 缺少流式标记或生效超时", got)
			}
		})
	}

	// %w 包装：上层用 errors.Is 判定原始错误的调用方必须照常工作
	if out := sampleAttempt().Annotate(errTimeout, fresh); !errors.Is(out, errTimeout) {
		t.Fatalf("包装丢了错误链：%v", out)
	}
}

// 控制台日志是这次改动的主要交付物：字段名就是排查协议，少一个都算回归。
func TestReportEmitsDiagnostics(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	idle := &ConnFacts{}
	idle.record(httptrace.GotConnInfo{Reused: true, WasIdle: true, IdleTime: 87 * time.Second})

	sampleAttempt().Report(
		context.Background(),
		errors.New("http2: timeout awaiting response headers"),
		20*time.Second,
		idle,
	)

	line := buf.String()
	for _, want := range []string{
		"上游尝试失败",
		"traceID=tid-1",
		"provider=commandcode",
		"model=deepseek/deepseek-v4.1-flash",
		"upstream_style=openai",
		"stream=true",
		"retry=2",
		"backup=true",
		"error_class=header_timeout_h2",
		"elapsed=20s",
		"time_out=1m0s",
		"header_timeout=20s",
		"conn_reused=true",
		"conn_was_idle=true",
		"conn_idle=1m27s",
		"hint=",
		"error=",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("日志缺字段 %q：%s", want, line)
		}
	}
}

// 非超时失败的日志不该带处置建议（那条 hint 只对等响应头超时成立），
// 也不该凭空造出连接字段。
func TestReportOmitsHintAndConnForPlainFailure(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	sampleAttempt().Report(context.Background(), errors.New("boom"), time.Millisecond, nil)

	line := buf.String()
	if strings.Contains(line, "hint=") {
		t.Fatalf("普通失败不该给处置建议：%s", line)
	}
	if strings.Contains(line, "conn_reused") || strings.Contains(line, "conn_idle") {
		t.Fatalf("没抓到连接时不该写连接字段：%s", line)
	}
	if !strings.Contains(line, "error_class=other") {
		t.Fatalf("失败码应当归 other：%s", line)
	}
}

// TraceConn 的价值全在"事后能看到复用与空闲时长"：真打一次上游，确认钩子确实被调。
func TestTraceConnRecordsConnectionReuse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	facts := &ConnFacts{}
	for i := range 2 {
		req, err := http.NewRequestWithContext(TraceConn(context.Background(), facts), http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatalf("建请求失败：%v", err)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("第 %d 次请求失败：%v", i+1, err)
		}
		_, _ = io.ReadAll(res.Body)
		res.Body.Close()

		snap := facts.snapshot()
		if !snap.got {
			t.Fatalf("第 %d 次请求没抓到连接信息", i+1)
		}
		if i == 1 && !snap.reused {
			t.Fatal("第二次请求应当复用连接")
		}
		if i == 0 && snap.reused {
			t.Fatal("第一次请求不该是复用连接")
		}
	}
}

func TestTraceConnAndFactsTolerateNil(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	if got := TraceConn(ctx, nil); got != ctx {
		t.Fatal("facts 为 nil 时应当原样返回 ctx")
	}

	var nilFacts *ConnFacts
	nilFacts.record(httptrace.GotConnInfo{Reused: true})
	if snap := nilFacts.snapshot(); snap.got {
		t.Fatalf("nil facts 不该产生连接信息：%+v", snap)
	}
}

// 端到端：BalanceChat 真的把取证接上了。
//
// 上游收下请求却一个响应头都不发，这正是线上那句错误的现场。断言三处：
// 控制台有一行带失败码的告警、收尾错误里带着真实原因与三要素、库里的记录
// 与客户端看到的都是同一句话。
func TestBalanceChatAnnotatesAndLogsHeaderTimeout(t *testing.T) {
	setupBalanceTestDB(t)

	// 上游永远不发响应头。Cleanup 是后进先出：先放开处理器，再关服务器
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(block) })

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	meta := ProvidersWithMeta{
		ModelWithProviderMap: map[uint]models.ModelWithProvider{
			201: {Model: gorm.Model{ID: 201}, ProviderID: 1, ProviderModel: "slow-model", Backup: new(bool)},
		},
		WeightItems: map[uint]int{201: 1},
		ProviderMap: map[uint]models.Provider{1: testOpenAIProvider(1, "slow-provider", srv.URL)},
		MaxRetry:    1,
		TimeOut:     3, // 流式下等响应头的预算是 TimeOut/3 = 1s
		Strategy:    consts.BalancerDefault,
	}
	before := Before{Model: "test-model", Stream: true, raw: []byte(balanceTestBody)}

	_, _, err := BalanceChat(context.Background(), time.Now(), consts.StyleOpenAI, before, meta, models.ReqMeta{
		Header:   http.Header{},
		RemoteIP: "127.0.0.1",
	}, NewBridgeNotes())
	if err == nil {
		t.Fatal("上游不发响应头，BalanceChat 不该成功")
	}

	// 客户端拿到的收尾错误必须带着真实原因，且原因自带三要素——
	// 没有它，"All retry failed" 之外什么都看不出来
	msg := err.Error()
	for _, want := range []string{
		"All retry failed",
		"[upstream_request]",
		"timeout awaiting response headers",
		"stream=true",
		"header_timeout=1s",
		"conn=fresh",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("收尾错误 %q 里没有 %q", msg, want)
		}
	}

	// 控制台那一行：没有它就只能进库翻，这正是"无法下手"的来源
	line := buf.String()
	for _, want := range []string{
		"上游尝试失败",
		"provider=slow-provider",
		"stream=true",
		"header_timeout=1s",
		"error_class=header_timeout_h1",
		"conn_reused=false",
		"hint=",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("控制台日志里没有 %q：%s", want, line)
		}
	}
}

// 处置建议是这条日志真正"能下手"的部分：四种连接现场各给一句，
// 非超时失败则一句都不给。
func TestFailureHint(t *testing.T) {
	t.Parallel()

	attempt := sampleAttempt()
	fresh := connSnapshot{got: true}
	reused := connSnapshot{got: true, reused: true}
	idle := connSnapshot{got: true, reused: true, wasIdle: true, idleTime: 90 * time.Second}
	unknown := connSnapshot{}

	tests := []struct {
		name    string
		code    string
		snap    connSnapshot
		wantSub string
	}{
		{name: "没抓到连接", code: failCodeHeaderTimeoutH2, snap: unknown, wantSub: "没抓到连接信息"},
		{name: "新连接", code: failCodeHeaderTimeoutH2, snap: fresh, wantSub: "新建连接"},
		{name: "复用活跃连接", code: failCodeHeaderTimeoutH1, snap: reused, wantSub: "复用了活跃连接"},
		{name: "复用空闲连接", code: failCodeHeaderTimeoutH2, snap: idle, wantSub: "空闲 1m30s"},
		{name: "非超时不给建议", code: failCodeDial, snap: idle, wantSub: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := failureHint(tc.code, attempt, tc.snap)
			if tc.wantSub == "" {
				if got != "" {
					t.Fatalf("不该给建议，got %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantSub) {
				t.Fatalf("建议 %q 里没有 %q", got, tc.wantSub)
			}
		})
	}
}
