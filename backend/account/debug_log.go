// debug_log.go Kiro 载荷调试日志（debug_logger.py 的按需缩译）。
// debug 开启时把 Kiro 出站请求/响应载荷落文件（脱敏 token）；
// 关闭时 WrapDebugTransport 原样返回、调用点零开销。
// 每请求两个文件（debug 目录下时间戳命名）：请求 JSON + 响应原始字节
// （流式响应边读边 tee，上限 2MB 防失控）；并发请求天然隔离。
package account

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

var (
	debugKiroEnabled atomic.Bool
	debugDir         atomic.Value // string
	debugSeq         atomic.Int64
)

// SetKiroDebug 开关 Kiro 载荷调试日志（dir 为落盘目录）。
func SetKiroDebug(enabled bool, dir string) {
	debugKiroEnabled.Store(enabled)
	if dir == "" {
		dir = "kiro-debug"
	}
	debugDir.Store(dir)
}

func debugOn() bool { return debugKiroEnabled.Load() }

func debugDirPath() string {
	if v, ok := debugDir.Load().(string); ok && v != "" {
		return v
	}
	return "kiro-debug"
}

// WrapDebugTransport debug 开启时返回包装 next 的调试 transport
// （仅记录 Kiro host 流量）；关闭原样返回（零开销）。
func WrapDebugTransport(next http.RoundTripper) http.RoundTripper {
	if !debugOn() {
		return next
	}
	if next == nil {
		next = http.DefaultTransport
	}
	return &debugTransport{next: next}
}

// debugTransport 记录 Kiro 请求/响应载荷的 RoundTripper。
type debugTransport struct {
	next http.RoundTripper
}

func (t *debugTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !isCloudTargetHost(req.URL.Host) {
		return t.next.RoundTrip(req)
	}
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		req.Body.Close()
	}
	seq := debugSeq.Add(1)
	stamp := time.Now().Format("20060102-150405.000")
	base := filepath.Join(debugDirPath(), fmt.Sprintf("%s-%03d", stamp, seq))
	if err := os.MkdirAll(debugDirPath(), 0o755); err != nil {
		log.Printf("kiro debug: cannot create dir: %v", err)
	}
	t.writeRequest(base, req, body)

	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		t.writeText(base+".resp.txt", []byte(fmt.Sprintf("transport error: %v\n", err)))
		return nil, err
	}
	// 流式响应：tee 边读边写（上限 2MB），Close 时收尾
	f, ferr := os.Create(base + ".resp.bin")
	if ferr != nil {
		return resp, nil // 落盘失败不影响请求
	}
	fprintf(f, "HTTP %d %s\n", resp.StatusCode, resp.Status)
	for k, vs := range resp.Header {
		for _, v := range vs {
			fprintf(f, "%s: %s\n", k, maskSecret(v))
		}
	}
	fprintf(f, "\n")
	resp.Body = &teeBody{r: io.TeeReader(resp.Body, &limitWriter{w: f, remaining: debugTeeLimit}), body: resp.Body, f: f}
	return resp, nil
}

// debugTeeLimit 响应 tee 落盘上限（对齐头注释：防大响应失控）。
const debugTeeLimit = 2 << 20

// limitWriter 超限后静默丢弃后续字节。绝不返回错误：io.TeeReader
// 会把 writer 错误传播成读错误，打断正常响应流。
type limitWriter struct {
	w         *os.File
	remaining int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.remaining <= 0 {
		return len(p), nil
	}
	n := int64(len(p))
	if n > l.remaining {
		p = p[:l.remaining]
	}
	written, err := l.w.Write(p)
	if err != nil {
		return len(p), nil // 落盘失败不影响响应流
	}
	l.remaining -= int64(written)
	return len(p), nil
}

// writeRequest 请求载荷落盘（头脱敏、JSON 体递归脱敏）。
func (t *debugTransport) writeRequest(base string, req *http.Request, body []byte) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s\n", req.Method, req.URL.String())
	for k, vs := range req.Header {
		for _, v := range vs {
			if isSecretHeader(k) {
				v = maskSecret(v)
			}
			fmt.Fprintf(&sb, "%s: %s\n", k, v)
		}
	}
	sb.WriteString("\n")
	if redacted := redactJSONBody(body); len(redacted) > 0 {
		sb.Write(redacted)
	} else {
		sb.Write(body)
	}
	t.writeText(base+".req.txt", []byte(sb.String()))
}

func (t *debugTransport) writeText(path string, content []byte) {
	if err := os.WriteFile(path, content, 0o644); err != nil {
		log.Printf("kiro debug: write %s: %v", path, err)
	}
}

func fprintf(f *os.File, format string, args ...any) {
	_, _ = fmt.Fprintf(f, format, args...)
}

// teeBody 流式响应包装：读时 tee 到文件，关闭时一并收尾。
type teeBody struct {
	r    io.Reader
	body io.ReadCloser
	f    *os.File
}

func (b *teeBody) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *teeBody) Close() error {
	b.f.Close()
	return b.body.Close()
}

// secretKeys JSON 体脱敏的精确键名集合（小写比较；不做子串匹配——
// Kiro 载荷的 tokenLimits/maxInputTokens 含 "token" 但非敏感）。
var secretKeys = map[string]bool{
	"accesstoken": true, "refreshtoken": true, "sessiontoken": true,
	"token": true, "apikey": true, "api_key": true, "clientsecret": true,
	"authorization": true, "password": true, "secret": true, "cloudkey": true,
}

func isSecretHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "x-kiro-authorization", "x-cloud-key", "x-amz-security-token":
		return true
	}
	return false
}

// redactJSONBody JSON 体递归脱敏（敏感键的值打码）；非 JSON 返回 nil。
func redactJSONBody(body []byte) []byte {
	if len(body) == 0 || !json.Valid(body) {
		return nil
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil
	}
	redactJSON(v)
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil
	}
	return out
}

// redactJSON 就地脱敏 map/slice 递归。
func redactJSON(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if secretKeys[strings.ToLower(k)] {
				if s, ok := val.(string); ok {
					t[k] = maskSecret(s)
				}
				continue
			}
			redactJSON(val)
		}
	case []any:
		for _, item := range t {
			redactJSON(item)
		}
	}
}
