package strategy

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var fatalKeywords = []string{
	"credit balance is too low",
	"exceeded your current quota",
	"invalid_api_key",
	"invalid x-api-key",
	"account is not active",
	"insufficient balance",
	"余额不足",
	"额度不足",
}

// FromResponse classifies an upstream response into a Signal.
// status <400 is handled by the caller (success path); this focuses on errors.
func FromResponse(status int, h http.Header, body []byte) Signal {
	s := Signal{Kind: KindTraffic, At: time.Now()}

	if status == http.StatusTooManyRequests {
		s.OK = false
		s.RateLimit = &RateLimit{RetryAfter: parseRetryAfter(h), Source: retryAfterSource(h)}
		return s
	}

	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		s.OK = false
		s.Fatal = true
		s.Reason = "auth rejected: " + strconv.Itoa(status)
		return s
	}

	if status == http.StatusBadRequest || status == http.StatusNotFound {
		// client-side problem; caller must not failover, but also must not
		// count it against the upstream
		s.OK = true
		s.Reason = "client error passthrough: " + strconv.Itoa(status)
		return s
	}

	s.OK = false
	s.Reason = "upstream status " + strconv.Itoa(status)
	if len(body) > 0 {
		lower := bytes.ToLower(body)
		for _, kw := range fatalKeywords {
			if bytes.Contains(lower, []byte(kw)) {
				s.Fatal = true
				s.Reason = "fatal keyword: " + kw
				break
			}
		}
		if !s.Fatal && bytes.Contains(lower, []byte("rate limit")) ||
			bytes.Contains(lower, []byte("too many requests")) {
			s.RateLimit = &RateLimit{RetryAfter: parseRetryAfter(h), Source: retryAfterSource(h)}
			s.Reason = "rate limited (body)"
		}
	}
	return s
}

func retryAfterSource(h http.Header) string {
	if h.Get("Retry-After") != "" {
		return "retry-after"
	}
	if h.Get("anthropic-ratelimit-requests-reset") != "" || h.Get("anthropic-ratelimit-tokens-reset") != "" {
		return "anthropic-ratelimit-reset"
	}
	if h.Get("x-ratelimit-reset-requests") != "" || h.Get("x-ratelimit-reset-tokens") != "" {
		return "x-ratelimit-reset"
	}
	return "heuristic"
}

// parseRetryAfter resolves a wait duration from rate-limit headers,
// in priority order; falls back to 60s.
func parseRetryAfter(h http.Header) time.Duration {
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
			return time.Duration(secs * float64(time.Second))
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
		}
	}
	for _, name := range []string{
		"anthropic-ratelimit-requests-reset",
		"anthropic-ratelimit-tokens-reset",
	} {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				if d := time.Until(t); d > 0 {
					return d
				}
			}
			if epoch, err := strconv.ParseInt(v, 10, 64); err == nil {
				if d := time.Until(time.Unix(epoch, 0)); d > 0 {
					return d
				}
			}
		}
	}
	for _, name := range []string{
		"x-ratelimit-reset-requests",
		"x-ratelimit-reset-tokens",
	} {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			if d, err := time.ParseDuration(v); err == nil && d > 0 {
				return d
			}
			// some providers send e.g. "6m0s" fine; others send plain seconds
			if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
				return time.Duration(secs * float64(time.Second))
			}
		}
	}
	return 60 * time.Second
}
