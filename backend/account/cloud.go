// cloud.go Kiro 上游云中转（extensions/cloud_connector.py 的 Go 翻译）。
// cloud.enabled 时把 Kiro 数据面/控制面/刷新端点的请求经 KiroaaS Cloud
// forward Lambda 转发：URL 重写为转发地址、X-Kiro-Target-Url 携带原始
// 地址、Authorization 挪到 X-Kiro-Authorization（CloudFront OAC 会以
// 自己的 SigV4 重写 Authorization 头）、x-amz-content-sha256 附载荷
// 哈希（Lambda 签名要求）；转发网络失败回退直连。
package account

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
)

// CloudConfig 云中转配置（进程级，main 装配一次；零值 = 关闭）。
type CloudConfig struct {
	ForwardURL string // KiroaaS Cloud forward Lambda 地址
	APIKey     string // 会话密钥（X-Cloud-Key）
}

var cloudCfg CloudConfig

// SetCloudConfig 安装云中转配置。
func SetCloudConfig(c CloudConfig) { cloudCfg = c }

// cloudEnabled 转发地址与密钥齐备才启用。
func cloudEnabled() bool { return cloudCfg.ForwardURL != "" && cloudCfg.APIKey != "" }

// WrapCloudTransport 云中转启用时返回包装 next 的转发 transport；
// 未启用原样返回（nil 也原样返回，调用方走默认 transport，零开销）。
func WrapCloudTransport(next http.RoundTripper) http.RoundTripper {
	if !cloudEnabled() {
		return next
	}
	if next == nil {
		next = http.DefaultTransport
	}
	return &cloudTransport{next: next}
}

// KiroTransport 组装 Kiro 出站 transport：云中转与调试日志按配置叠加
// （debug 在最外层——记录发给 Kiro 的原始请求而非云中转改写后的）。
// 两项都关闭时返回 nil（调用方走默认 transport）。
func KiroTransport() http.RoundTripper {
	return WrapDebugTransport(WrapCloudTransport(nil))
}

// cloudTransport Kiro 流量经云中转的 RoundTripper；非 Kiro host 直通。
type cloudTransport struct {
	next http.RoundTripper
}

// isCloudTargetHost 是否 Kiro 数据面/控制面/刷新端点
// （q.*.amazonaws.com、oidc.*.amazonaws.com、*.auth.desktop.kiro.dev、
// runtime.*.kiro.dev——cloud_connector._is_target_host 同款）。
func isCloudTargetHost(host string) bool {
	h := strings.ToLower(host)
	if strings.HasSuffix(h, ".amazonaws.com") &&
		(strings.HasPrefix(h, "q.") || strings.HasPrefix(h, "oidc.")) {
		return true
	}
	if strings.HasSuffix(h, ".auth.desktop.kiro.dev") {
		return true
	}
	if strings.HasPrefix(h, "runtime.") && strings.HasSuffix(h, ".kiro.dev") {
		return true
	}
	return false
}

func (t *cloudTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !isCloudTargetHost(req.URL.Host) {
		return t.next.RoundTrip(req)
	}
	// 载荷完整读出（sha256 + 失败重放直连）
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		req.Body.Close()
	}
	fwdURL, err := url.Parse(cloudCfg.ForwardURL)
	if err != nil {
		log.Printf("kiro cloud: bad forward url %q, going direct", cloudCfg.ForwardURL)
		return t.direct(req.Context(), req, body)
	}
	fwd := req.Clone(req.Context())
	fwd.Body = io.NopCloser(bytes.NewReader(body))
	fwd.ContentLength = int64(len(body))
	fwd.Header = fwd.Header.Clone()
	fwd.Header.Set("X-Kiro-Target-Url", req.URL.String())
	fwd.Header.Set("X-Cloud-Key", cloudCfg.APIKey)
	if auth := fwd.Header.Get("Authorization"); auth != "" {
		fwd.Header.Del("Authorization")
		fwd.Header.Set("X-Kiro-Authorization", auth)
	}
	sum := sha256.Sum256(body)
	fwd.Header.Set("x-amz-content-sha256", hex.EncodeToString(sum[:]))
	fwd.URL = fwdURL
	fwd.Host = fwdURL.Host

	resp, err := t.next.RoundTrip(fwd)
	if err == nil {
		return resp, nil
	}
	log.Printf("kiro cloud: forward to %s failed (%v), falling back to direct", fwdURL.Host, err)
	return t.direct(req.Context(), req, body)
}

// direct 回退直连：以原始 URL 与载荷重放一次。
func (t *cloudTransport) direct(ctx context.Context, orig *http.Request, body []byte) (*http.Response, error) {
	req := orig.Clone(ctx)
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	return t.next.RoundTrip(req)
}
