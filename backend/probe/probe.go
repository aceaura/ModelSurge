package probe

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"relayd/backend/catalog"
	"relayd/strategy"
)

// Prober executes one active check. Decision-making stays in strategy.
type Prober interface {
	Name() string
	Interval() time.Duration
	Probe(ctx context.Context, t Target) strategy.Signal
}

type Target struct {
	Name    string
	BaseURL string
	APIKey  string
	Models  []string // canonical names
	Native  map[string]string
	Client  *http.Client
}

// NewProber builds a Prober from config; nil when the config is unusable.
func NewProber(upstreamName string, cfg catalog.ProbeConfig) (Prober, error) {
	switch cfg.Type {
	case "balance":
		if cfg.Request == nil || cfg.Extract == nil {
			return nil, fmt.Errorf("balance probe requires request and extract")
		}
		return &balanceProber{cfg: cfg}, nil
	case "ping":
		return &pingProber{cfg: cfg}, nil
	}
	return nil, fmt.Errorf("unknown probe type %q", cfg.Type)
}

const maxProbeBody = 64 << 10

func doProbeRequest(ctx context.Context, client *http.Client, baseURL, apiKey string, req catalog.HTTPProbeReq) ([]byte, error) {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	path := expandTemplate(req.Path, apiKey)
	full := strings.TrimSuffix(baseURL, "/") + path

	u, err := url.Parse(full)
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	for k, v := range req.Headers {
		r.Header.Set(k, expandTemplate(v, apiKey))
	}
	if req.Auth.Name != "" {
		val := expandTemplate(req.Auth.Template, apiKey)
		switch req.Auth.In {
		case "query":
			q := r.URL.Query()
			q.Set(req.Auth.Name, val)
			r.URL.RawQuery = q.Encode()
		default: // header
			r.Header.Set(req.Auth.Name, val)
		}
	}

	resp, err := client.Do(r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("probe status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxProbeBody+1))
}

// expandTemplate supports {api_key} and {date±Nd} placeholders.
func expandTemplate(s, apiKey string) string {
	s = strings.ReplaceAll(s, "{api_key}", apiKey)
	for {
		start := strings.Index(s, "{date")
		if start < 0 {
			return s
		}
		end := strings.Index(s[start:], "}")
		if end < 0 {
			return s
		}
		expr := s[start+1 : start+end] // date-100d / date+1d / date
		s = s[:start] + resolveDate(expr) + s[start+end+1:]
	}
}

func resolveDate(expr string) string {
	// expr: "date", "date-100d", "date+1d"
	offset := 0
	if len(expr) > 4 {
		sign := 1
		num := expr[4:]
		if strings.HasPrefix(num, "-") {
			sign = -1
			num = num[1:]
		} else if strings.HasPrefix(num, "+") {
			num = num[1:]
		}
		num = strings.TrimSuffix(num, "d")
		if n, err := parseInt(num); err == nil {
			offset = sign * n
		}
	}
	return time.Now().AddDate(0, 0, offset).Format("2006-01-02")
}

func parseInt(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

func extractValue(body []byte, ex catalog.Extract) (float64, error) {
	res := gjson.GetBytes(body, ex.Value)
	if !res.Exists() {
		return 0, fmt.Errorf("path %q not found", ex.Value)
	}
	scale := ex.Scale
	if scale == 0 {
		scale = 1
	}
	return res.Float() * scale, nil
}
