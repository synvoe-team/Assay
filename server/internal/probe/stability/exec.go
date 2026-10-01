package stability

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Yukiho0287/assay/server/internal/probe/protocol"
)

// outcome 一次压测请求的原始观测（未落 Sample 的中间态）。
type outcome struct {
	Ok         bool
	HTTPStatus int
	ErrorClass string
	Error      string

	TTFB    time.Duration
	TTFD    time.Duration // 首个非空增量（推理或正文）
	HasTTFD bool
	TTFT    time.Duration // 首个非空正文增量
	HasTTFT bool
	Total   time.Duration
	HasBody bool // 拿到 200 并读过响应体（TTFB/Total 有意义）

	Usage     protocol.Usage
	HTTPProto string      // 实际协商的协议版本（resp.Proto，如 HTTP/1.1）
	Header    http.Header // 所有响应都留存，供 RPM probe 读限速头（2xx 也带余量头）
}

// timingReader 包裹响应体，在首个非空 Read 打 TTFB 点，其余透传。
type timingReader struct {
	r     io.Reader
	start time.Time
	ttfb  time.Duration
	got   bool
}

func (t *timingReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if !t.got && n > 0 {
		t.ttfb = time.Since(t.start)
		t.got = true
	}
	return n, err
}

// doRequest 发一次最小压测请求并观测时序 + usage。
// TTFB 由 timingReader 打点、TTFD/TTFT 由 codec 的两个钩子打点、total 为整流耗时。
func doRequest(ctx context.Context, client *http.Client, codec protocol.Codec, baseURL, apiKey, model, content string, maxTokens, timeoutMs int) outcome {
	var o outcome
	body, err := codec.LoadBody(model, content, maxTokens)
	if err != nil {
		o.ErrorClass = ErrTransport
		o.Error = "构造请求失败: " + err.Error()
		return o
	}
	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, baseURL+codec.Path(), bytes.NewReader(body))
	if err != nil {
		o.ErrorClass = ErrTransport
		o.Error = "构造请求失败: " + err.Error()
		return o
	}
	req.Header.Set("Content-Type", "application/json")
	codec.Auth(req, apiKey)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		o.ErrorClass = ErrTransport
		if errors.Is(err, context.DeadlineExceeded) {
			o.Error = "超时（" + strconv.Itoa(timeoutMs) + "ms 无响应）"
		} else {
			o.Error = "连接失败: " + truncateOneLine(err.Error())
		}
		return o
	}
	defer resp.Body.Close()

	o.HTTPStatus = resp.StatusCode
	o.HTTPProto = resp.Proto
	o.Header = resp.Header // 限速头在成功响应上也可能带余量（x-ratelimit-remaining-*）
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		o.ErrorClass = classifyHTTP(resp.StatusCode)
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		msg := strings.TrimSpace(string(snippet))
		if msg == "" {
			msg = resp.Status
		}
		o.Error = "HTTP " + strconv.Itoa(resp.StatusCode) + ": " + truncateOneLine(msg)
		return o
	}

	tr := &timingReader{r: resp.Body, start: start}
	res, err := codec.ScanStream(tr, protocol.Hooks{
		OnFirstDelta: func() {
			o.TTFD = time.Since(start)
			o.HasTTFD = true
		},
		OnFirstContent: func() {
			o.TTFT = time.Since(start)
			o.HasTTFT = true
		},
	})
	o.Total = time.Since(start)
	o.HasBody = true
	if tr.got {
		o.TTFB = tr.ttfb
	}
	o.Usage = res.Usage
	if err != nil {
		o.ErrorClass = ErrStreamAnomaly
		o.Error = "读取响应流失败: " + truncateOneLine(err.Error())
		return o
	}
	o.ErrorClass, o.Error = classifyStream(res, maxTokens)
	o.Ok = o.ErrorClass == ""
	return o
}

// classifyStream HTTP 200 且流扫描无错之后的判定，返回空分类 = 成功。顺序即优先级：
// 流没走完 → 断流；有正文 → 成功；没正文时先看是不是我们给的生成上限被耗尽（砝码不足、
// 非渠道故障），再看是不是只吐了推理，最后才是什么都没有的空响应。
// 预算耗尽有两个信号：协议原生的「因上限结束」，以及 completion 达到上限的计数兜底
// （有的中转站丢 finish_reason；隐藏推理的模型连增量都不给）。
func classifyStream(r protocol.Result, maxTokens int) (class, msg string) {
	switch {
	case !r.Completed:
		return ErrStreamAnomaly, "流未结束即中断（无结束帧）"
	case r.SawContent:
		return "", ""
	case r.HitMaxTokens || (r.Usage.Ok && r.Usage.Completion >= int64(maxTokens)):
		return ErrBudgetExhausted, fmt.Sprintf("生成上限 %d 被耗尽仍无正文（推理模型砝码不足，非渠道故障）", maxTokens)
	case r.SawReasoning:
		return ErrReasoningOnly, "只有推理增量、没有正文就结束了"
	default:
		return ErrSemanticEmpty, "HTTP 200 但响应无任何增量"
	}
}

func classifyHTTP(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return ErrRateLimited
	case status >= 500:
		return ErrHTTP5xx
	case status >= 400:
		return ErrHTTP4xx
	default:
		return ErrHTTP4xx // 3xx 等非 2xx 归 4xx 类（罕见，不单开分类）
	}
}

// sampleFrom outcome → Sample：测到什么记什么，没测到的取负值（落库转 NULL）。
// 失败但拿到 200 的样本（预算耗尽/只有推理/断流）也保留时序与计量，作证据链；
// 评估期的延迟分位与吞吐只取成功样本，不受影响。
func sampleFrom(stage string, stageIndex, seq int, warmup bool, dispatchedAt time.Time, proto string, o outcome) Sample {
	s := Sample{
		Stage:        stage,
		StageIndex:   stageIndex,
		Seq:          seq,
		Protocol:     proto,
		DispatchedAt: dispatchedAt,
		Warmup:       warmup,
		Ok:           o.Ok,
		HTTPStatus:   o.HTTPStatus,
		HTTPProto:    o.HTTPProto,
		ErrorClass:   o.ErrorClass,
		Error:        o.Error,
		TTFBms:       -1,
		TTFDms:       -1,
		TTFTms:       -1,
		TotalMs:      -1,
		InputTokens:  -1,
		OutputTokens: -1,
		CachedTokens: -1,
	}
	if o.HasBody {
		s.TTFBms = int(o.TTFB.Milliseconds())
		s.TotalMs = int(o.Total.Milliseconds())
	}
	if o.HasTTFD {
		s.TTFDms = int(o.TTFD.Milliseconds())
	}
	if o.HasTTFT {
		s.TTFTms = int(o.TTFT.Milliseconds())
	}
	if o.Usage.Ok {
		s.InputTokens = int(o.Usage.Prompt)
		s.OutputTokens = int(o.Usage.Completion)
		s.CachedTokens = int(o.Usage.Cached)
	}
	return s
}

// uniquePrompt 给压测 prompt 拼唯一前缀「[nonce 档位-序号] 」。前缀放最前面：
// 前缀缓存按开头匹配，开头不同才破得掉；档位标签各 probe 互异（c/r/t 开头），任务内必唯一。
func uniquePrompt(nonce, stage string, seq int, base string) string {
	return fmt.Sprintf("[%s %s-%d] %s", nonce, stage, seq, base)
}

func truncateOneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}
