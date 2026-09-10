package strategy

import (
	"net/http"
	"testing"
	"time"
)

func TestFromResponse_RateLimitHeaderPriority(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    time.Duration
		source  string
	}{
		{"retry-after seconds", map[string]string{"Retry-After": "7"}, 7 * time.Second, "retry-after"},
		{"retry-after beats anthropic", map[string]string{
			"Retry-After":                        "3",
			"anthropic-ratelimit-requests-reset": time.Now().Add(9 * time.Minute).UTC().Format(time.RFC3339),
		}, 3 * time.Second, "retry-after"},
		{"anthropic rfc3339", map[string]string{
			"anthropic-ratelimit-requests-reset": time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339),
		}, 2 * time.Minute, "anthropic-ratelimit-reset"},
		{"x-ratelimit duration", map[string]string{"x-ratelimit-reset-requests": "6m0s"}, 6 * time.Minute, "x-ratelimit-reset"},
		{"fallback 60s", map[string]string{}, 60 * time.Second, "heuristic"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tc.headers {
				h.Set(k, v)
			}
			s := FromResponse(429, h, nil)
			if s.RateLimit == nil {
				t.Fatal("expected RateLimit")
			}
			if tc.name == "anthropic rfc3339" {
				if s.RateLimit.RetryAfter < 110*time.Second || s.RateLimit.RetryAfter > 121*time.Second {
					t.Fatalf("got %v", s.RateLimit.RetryAfter)
				}
			} else if s.RateLimit.RetryAfter != tc.want {
				t.Fatalf("want %v got %v", tc.want, s.RateLimit.RetryAfter)
			}
			if s.RateLimit.Source != tc.source {
				t.Fatalf("source want %s got %s", tc.source, s.RateLimit.Source)
			}
			if s.Fatal || s.OK {
				t.Fatal("429 must be neither fatal nor ok")
			}
		})
	}
}

func TestFromResponse_FatalClassification(t *testing.T) {
	h := http.Header{}
	if s := FromResponse(401, h, nil); !s.Fatal {
		t.Fatal("401 must be fatal")
	}
	if s := FromResponse(403, h, nil); !s.Fatal {
		t.Fatal("403 must be fatal")
	}
	body := []byte(`{"error":{"message":"You exceeded your current quota, please check your plan"}}`)
	if s := FromResponse(500, h, body); !s.Fatal {
		t.Fatal("quota keyword in body must be fatal")
	}
	body = []byte(`{"error":{"message":"Your credit balance is too low"}}`)
	if s := FromResponse(400, h, body); !s.OK {
		t.Fatal("400 passthrough must not count against upstream")
	}
}

func TestFromResponse_ClientErrorsAreNeutral(t *testing.T) {
	for _, code := range []int{400, 404} {
		s := FromResponse(code, http.Header{}, []byte(`{"error":"bad model"}`))
		if !s.OK || s.Fatal || s.RateLimit != nil {
			t.Fatalf("%d must be neutral passthrough, got %+v", code, s)
		}
	}
}

func TestFromResponse_GenericFailureNotFatal(t *testing.T) {
	s := FromResponse(503, http.Header{}, []byte("upstream overloaded"))
	if s.OK || s.Fatal || s.RateLimit != nil {
		t.Fatalf("503 must be plain failure, got %+v", s)
	}
}

func TestFromResponse_RateLimitInBody(t *testing.T) {
	s := FromResponse(500, http.Header{}, []byte(`{"error":{"type":"rate_limit_error","message":"rate limit exceeded"}}`))
	if s.RateLimit == nil {
		t.Fatal("rate limit keyword in body must produce RateLimit signal")
	}
	if s.Fatal {
		t.Fatal("rate limit is not fatal")
	}
}
