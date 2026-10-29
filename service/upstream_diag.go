package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptrace"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/atopos31/llmio/models"
)

// 上游「等响应头超时」的取证与归因。
//
// 这条错误只有一处来源：http.Transport.ResponseHeaderTimeout 在「请求已经发完、
// 响应头一个字节都没回来」时触发（net/http 的 h2_bundle.go 里的 respHeaderTimer，
// HTTP/2 连接的文案带 "http2: " 前缀）。llmio 正是设它的人：取值是模型配置的
// TimeOut，流式请求再除以 3（见 BalanceChat 里的 responseHeaderTimeout）。
//
// 它本身不携带任何「为什么没回来」的信息，而两种完全不同的现场会落在同一句话上：
//
//	A. 上游真的慢——排队、思考型模型迟迟不出首 token。该动的是调大 TimeOut；
//	B. 复用了一条已经死掉的 HTTP/2 连接——对端或中间设备悄悄丢了状态，既没 FIN
//	   也没 RST，写出去的数据石沉大海。该动的是连接活性探测（providers.GetClient
//	   里的 HTTP2.SendPingTimeout）。
//
// 两种现场唯一的分水岭是「这条连接是不是复用的、复用前空闲了多久」，而那是
// httptrace.GotConn 才拿得到的事实。所以失败时把它连同生效超时一起打进控制台日志，
// 并补在落库的错误文本后面——下一次再出现，看一眼日志就知道该调超时还是该查连接。

// UpstreamAttempt 描述一次上游尝试的上下文。
//
// 只放失败取证要用到的字段，避免把 BalanceChat 的局部变量在自愈闭环里搬来搬去。
type UpstreamAttempt struct {
	TraceID       string
	Provider      string
	ProviderModel string
	UpstreamStyle string
	Stream        bool
	Retry         int
	Backup        bool
	TimeOut       time.Duration // 模型配置的 TimeOut 原值
	HeaderTimeout time.Duration // 本次真正生效的「等响应头」预算（流式是 TimeOut/3）
}

// NewUpstreamAttempt 由这次尝试的 ChatLog 快照与超时参数建出取证上下文。
//
// 用 ChatLog 而不是逐字段传参，是为了让新增字段时只需要改这里一处：
// 日志行、错误文本、库里的记录三者本来就该描述同一次尝试。
func NewUpstreamAttempt(log models.ChatLog, stream bool, timeOutSeconds int, headerTimeout time.Duration) UpstreamAttempt {
	return UpstreamAttempt{
		TraceID:       log.TraceID,
		Provider:      log.ProviderName,
		ProviderModel: log.ProviderModel,
		UpstreamStyle: log.UpstreamStyle,
		Stream:        stream,
		Retry:         log.Retry,
		Backup:        log.Backup,
		TimeOut:       time.Duration(timeOutSeconds) * time.Second,
		HeaderTimeout: headerTimeout,
	}
}

// ConnFacts 记录一次尝试实际用到的连接事实，由 TraceConn 通过 httptrace 填充。
//
// 三个字段足够区分上面 A/B 两种现场：复用且空闲已久＝连接很可能已经死了；
// 非复用＝嫌疑在上游本身。加锁是因为 GotConn 跑在 transport 的 goroutine 上，
// 而读取发生在 client.Do 返回之后。
type ConnFacts struct {
	mu       sync.Mutex
	got      bool
	reused   bool
	wasIdle  bool
	idleTime time.Duration
}

func (c *ConnFacts) record(info httptrace.GotConnInfo) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got, c.reused, c.wasIdle, c.idleTime = true, info.Reused, info.WasIdle, info.IdleTime
}

// connSnapshot 是 ConnFacts 的一致性快照：日志与错误文本都只读它。
type connSnapshot struct {
	got      bool
	reused   bool
	wasIdle  bool
	idleTime time.Duration
}

func (c *ConnFacts) snapshot() connSnapshot {
	if c == nil {
		return connSnapshot{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return connSnapshot{got: c.got, reused: c.reused, wasIdle: c.wasIdle, idleTime: c.idleTime}
}

// TraceConn 把连接观测挂到 ctx 上。
//
// 必须在 BuildReq **之前**调用：httptrace 随 context 一路传到 transport，
// 请求建好之后再挂就来不及了。返回派生 ctx，不改调用方的原 ctx。
func TraceConn(ctx context.Context, facts *ConnFacts) context.Context {
	if facts == nil {
		return ctx
	}
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: facts.record})
}

// 上游失败码。只进控制台日志，不改变落库的原始错误文本。
const (
	failCodeHeaderTimeoutH2 = "header_timeout_h2"
	failCodeHeaderTimeoutH1 = "header_timeout_h1"
	failCodeClientCancel    = "client_cancel"
	failCodeDial            = "dial"
	failCodeTLS             = "tls"
	failCodeTimeoutOther    = "timeout_other"
	failCodeOther           = "other"
)

// headerTimeoutKind 判定「等响应头超时」，返回对应的失败码；不是这一类返回空串。
//
// HTTP/2 的文案必须排在前面判：它整句包含了 HTTP/1.1 那半句。
func headerTimeoutKind(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "http2: timeout awaiting response headers"):
		return failCodeHeaderTimeoutH2
	case strings.Contains(msg, "timeout awaiting response headers"):
		return failCodeHeaderTimeoutH1
	}
	return ""
}

// classifyUpstreamFailure 给一次 client.Do 失败归一个稳定失败码。
// 顺序即优先级：等响应头超时同样含 "timeout" 字样，必须先于通用超时判定。
func classifyUpstreamFailure(reqCtx context.Context, err error) string {
	if kind := headerTimeoutKind(err); kind != "" {
		return kind
	}
	if err == nil {
		return failCodeOther
	}
	msg := strings.ToLower(err.Error())
	switch {
	case IsClientCancel(reqCtx, err):
		return failCodeClientCancel
	case strings.Contains(msg, "dial tcp"), strings.Contains(msg, "connect: connection"):
		return failCodeDial
	case strings.Contains(msg, "tls:"), strings.Contains(msg, "x509:"):
		return failCodeTLS
	case os.IsTimeout(err), errors.Is(err, context.DeadlineExceeded), strings.Contains(msg, "i/o timeout"):
		return failCodeTimeoutOther
	}
	return failCodeOther
}

// Report 把一次失败的上游尝试打进控制台。
//
// 这条日志的存在理由很具体：失败尝试原先只进 chat_logs，控制台里连一行都没有，
// 于是线上看到 error 列那句 http2 超时无从下手——既不知道等了多久、生效超时是多少，
// 也不知道是不是撞在一条复用的死连接上。每个失败尝试一条 Warn，与失败行落库同频。
func (a UpstreamAttempt) Report(reqCtx context.Context, err error, elapsed time.Duration, facts *ConnFacts) {
	snap := facts.snapshot()
	code := classifyUpstreamFailure(reqCtx, err)
	attrs := []any{
		"traceID", a.TraceID,
		"provider", a.Provider,
		"model", a.ProviderModel,
		"upstream_style", a.UpstreamStyle,
		"stream", a.Stream,
		"retry", a.Retry,
		"backup", a.Backup,
		"error_class", code,
		"elapsed", elapsed.Round(time.Millisecond).String(),
		"time_out", a.TimeOut.String(),
		"header_timeout", a.HeaderTimeout.String(),
	}
	if snap.got {
		attrs = append(attrs,
			"conn_reused", snap.reused,
			"conn_was_idle", snap.wasIdle,
			"conn_idle", snap.idleTime.Round(time.Millisecond).String(),
		)
	}
	if hint := failureHint(code, a, snap); hint != "" {
		attrs = append(attrs, "hint", hint)
	}
	attrs = append(attrs, "error", err)
	slog.Warn("上游尝试失败", attrs...)
}

// failureHint 只给「等响应头超时」写处置建议：其余失败的原因在 error 里已经写清楚了。
func failureHint(code string, a UpstreamAttempt, snap connSnapshot) string {
	if code != failCodeHeaderTimeoutH2 && code != failCodeHeaderTimeoutH1 {
		return ""
	}
	switch {
	case !snap.got:
		return "没抓到连接信息，请求没走到建连；先确认上游地址可解析"
	case snap.reused && snap.wasIdle:
		return fmt.Sprintf("复用了空闲 %s 的连接，疑似连接已被对端回收：查上游与中间设备的连接空闲回收时间",
			snap.idleTime.Round(time.Millisecond))
	case snap.reused:
		return "复用了活跃连接；同一连接上还有别的请求在等，重点看上游排队或限流"
	default:
		return fmt.Sprintf("新建连接，上游 %s 内没回响应头：思考型模型可调大该模型的超时时间", a.HeaderTimeout)
	}
}

// Annotate 给「等响应头超时」的错误文本补上定位三要素：流式标记、生效超时、连接是否复用，
// 让 chat_logs.error 自己就说得清；其他错误原样返回，不动库里既有的文案。
//
// 用 %w 包装，errors.Is/As 与 MarkStage 的幂等判定照常工作。
func (a UpstreamAttempt) Annotate(err error, facts *ConnFacts) error {
	if headerTimeoutKind(err) == "" {
		return err
	}
	snap := facts.snapshot()
	conn := "unknown"
	switch {
	case !snap.got:
	case snap.reused && snap.wasIdle:
		conn = "reused,idle=" + snap.idleTime.Round(time.Millisecond).String()
	case snap.reused:
		conn = "reused"
	default:
		conn = "fresh"
	}
	return fmt.Errorf("%w (stream=%t, header_timeout=%s, conn=%s)", err, a.Stream, a.HeaderTimeout, conn)
}
