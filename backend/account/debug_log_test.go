package account

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withDebug 安装 Kiro 调试日志并测试毕恢复。
func withDebug(t *testing.T, dir string) {
	t.Helper()
	SetKiroDebug(true, dir)
	t.Cleanup(func() { SetKiroDebug(false, "") })
}

// TestDebugTransport_Redaction：请求头与 JSON 体敏感字段脱敏落盘，
// 非敏感字段（含 tokenLimits/maxInputTokens 这类含 "token" 的键名）原样；
// 响应流 tee 到 .resp.bin。
func TestDebugTransport_Redaction(t *testing.T) {
	dir := t.TempDir()
	withDebug(t, dir)

	// next 层拦截（不打真网络），URL host 为 kiro 形态即可触发记录
	next := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200, Body: io.NopCloser(strings.NewReader("stream-bytes-here")),
			Header: http.Header{}, Request: r,
		}, nil
	})
	client := &http.Client{Transport: &debugTransport{next: next}}
	body := `{"conversationState":{"currentMessage":{"userInputMessage":{"content":"hi","modelId":"m"}}},
		"accessToken":"at-secret-token-1234","refreshToken":"rt-rotated-9999","apiKey":"sk-abcdef",
		"tokenLimits":{"maxInputTokens":200000}}`
	req, _ := http.NewRequest(http.MethodPost, "https://q.us-east-1.amazonaws.com/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer at-secret-token-1234")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(respBody) != "stream-bytes-here" {
		t.Fatalf("resp = %s", respBody)
	}

	// 请求文件：头脱敏 + JSON 体递归脱敏
	reqFile := findDebugFile(t, dir, ".req.txt")
	content := readDebugFile(t, reqFile)
	if strings.Contains(content, "at-secret-token-1234") || strings.Contains(content, "rt-rotated-9999") {
		t.Errorf("secret leaked in %s:\n%s", reqFile, content)
	}
	for _, want := range []string{"****1234", "****9999", "maxInputTokens", "200000"} {
		if !strings.Contains(content, want) {
			t.Errorf("req log missing %q:\n%s", want, content)
		}
	}
	if !strings.Contains(content, "Authorization: ****1234") {
		t.Errorf("Authorization header not masked:\n%s", content)
	}
	// 响应文件：状态行 + tee 的流字节
	respFile := findDebugFile(t, dir, ".resp.bin")
	respLog := readDebugFile(t, respFile)
	if !strings.Contains(respLog, "HTTP 200") || !strings.Contains(respLog, "stream-bytes-here") {
		t.Errorf("resp log incomplete:\n%s", respLog)
	}
}

// TestDebugTransport_DisabledZeroOverhead：关闭时包装函数原样透传
// （行为断言：透传对象即调用者传入的 sentinel transport）。
func TestDebugTransport_DisabledZeroOverhead(t *testing.T) {
	SetKiroDebug(false, "")
	var called bool
	custom := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: r}, nil
	})
	for name, wrap := range map[string]func(http.RoundTripper) http.RoundTripper{
		"debug": WrapDebugTransport,
		"cloud": WrapCloudTransport,
	} {
		got := wrap(custom)
		req, _ := http.NewRequest(http.MethodGet, "https://example.com/", nil)
		resp, err := got.RoundTrip(req)
		if err != nil || resp.StatusCode != 204 || !called {
			t.Errorf("%s: disabled wrap must pass through to caller transport (called=%v, resp=%v, err=%v)",
				name, called, resp, err)
		}
	}
}

// findDebugFile 找 debug 目录下带后缀的唯一文件。
func findDebugFile(t *testing.T, dir, suffix string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*"+suffix))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no *%s file in %s (err=%v)", suffix, dir, err)
	}
	return matches[0]
}

func readDebugFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
