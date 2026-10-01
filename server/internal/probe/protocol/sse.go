package protocol

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Yukiho0287/assay/server/internal/probe"
)

// scanSSE 逐帧扫描 SSE 流：只认 data: 行，忽略 event:/id:/注释/空行。
// 每个 data payload 解析为对象后交 handle 处理（按 payload 内的 type 字段自行分发，
// 不依赖 event: 行）；handle 返回 err 即中止扫描（流内错误事件）。
// [DONE] 不进 handle，以返回值 done 报告（chat 协议的结束帧之一）。
// 分片坏损（非 JSON）判传输层失败返回 err。
// 缓冲区同 tokenaccounting：初始 64KB、上限 4MB，容纳偶发大帧。
func scanSSE(r io.Reader, handle func(m map[string]any) error) (done bool, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue // event:/id:/注释/空行一律跳过
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			done = true
			continue
		}
		doc, err := probe.DecodeUseNumber([]byte(payload))
		if err != nil {
			return done, fmt.Errorf("流式分片不是 JSON: %w", err)
		}
		if m, ok := doc.(map[string]any); ok {
			if err := handle(m); err != nil {
				return done, err
			}
		}
	}
	return done, sc.Err()
}

// decodeObject 非流式整块响应 → JSON 对象
func decodeObject(raw []byte) (map[string]any, error) {
	doc, err := probe.DecodeUseNumber(raw)
	if err != nil {
		return nil, fmt.Errorf("响应体不是 JSON: %w", err)
	}
	m, ok := doc.(map[string]any)
	if !ok {
		return nil, errors.New("响应体不是 JSON 对象")
	}
	if err := errorField(m); err != nil {
		return nil, err
	}
	return m, nil
}

// errorField 响应对象里的 error 字段 → err：标准的 {"error":{"type"|"code","message"}}，
// 也认不少网关回的 {"error":"…"} 纯字符串；没有或为空返回 nil
func errorField(m map[string]any) error {
	switch ev := m["error"].(type) {
	case map[string]any:
		kind, _ := strField(ev, "type")
		if kind == "" {
			kind, _ = strField(ev, "code")
		}
		msg, _ := strField(ev, "message")
		return streamError(kind, msg)
	case string:
		if ev != "" {
			return streamError("", ev)
		}
	}
	return nil
}

// streamError 流内错误事件 → err，文案带错误类型（判 stream_anomaly 时保留，便于定位哪一层拒的）
func streamError(kind, msg string) error {
	if kind == "" {
		kind = "unknown"
	}
	return fmt.Errorf("流内错误事件 %s: %s", kind, msg)
}

// intField 取整数字段（DecodeUseNumber 使数字为 json.Number，无精度损失）
func intField(m map[string]any, key string) (int64, bool) {
	n, ok := m[key].(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return i, true
}

// objField 取嵌套对象字段
func objField(m map[string]any, key string) (map[string]any, bool) {
	v, ok := m[key].(map[string]any)
	return v, ok
}

// strField 取字符串字段
func strField(m map[string]any, key string) (string, bool) {
	v, ok := m[key].(string)
	return v, ok
}
