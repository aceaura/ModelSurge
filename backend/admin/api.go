package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"relayd/backend/obs"
	"relayd/backend/probe"
	"relayd/strategy"
)

type Server struct {
	Reg      *strategy.Registry
	Ring     *obs.Ring
	Tokens   map[string]bool
	ProberOf func(name string) []probe.Prober
	Client   *http.Client
}

type upstreamJSON struct {
	Name      string            `json:"name"`
	Provider  string            `json:"provider"`
	Protocol  string            `json:"protocol"`
	BaseURL   string            `json:"base_url"`
	Models    []string          `json:"models"`
	Priority  int               `json:"priority"`
	Weight    int               `json:"weight"`
	Available bool              `json:"available"`
	Snapshot  strategy.Snapshot `json:"state"`
}

func (s *Server) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/summary", s.auth(s.summary))
	mux.HandleFunc("/admin/upstreams", s.auth(s.upstreams))
	mux.HandleFunc("/admin/upstreams/", s.auth(s.upstreamAction))
	mux.HandleFunc("/admin/models", s.auth(s.models))
	mux.HandleFunc("/admin/events", s.auth(s.events))
	return mux
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !s.Tokens[key] {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) summary(w http.ResponseWriter, r *http.Request) {
	counts := map[string]int{}
	var totalBalance float64
	var balanceCount int
	for _, u := range s.Reg.All() {
		snap := u.Circuit.Snapshot()
		counts[snap.Status.String()]++
		if snap.LastBalance != nil {
			totalBalance += *snap.LastBalance
			balanceCount++
		}
	}
	writeJSON(w, map[string]any{
		"total":         len(s.Reg.All()),
		"by_status":     counts,
		"balance_usd":   totalBalance,
		"balance_known": balanceCount,
		"time":          time.Now(),
	})
}

func (s *Server) upstreams(w http.ResponseWriter, r *http.Request) {
	all := s.Reg.All()
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	out := make([]upstreamJSON, 0, len(all))
	for _, u := range all {
		out = append(out, upstreamJSON{
			Name:      u.Name,
			Provider:  u.Provider,
			Protocol:  u.Protocol,
			BaseURL:   u.BaseURL,
			Models:    u.Models,
			Priority:  u.Priority,
			Weight:    u.Weight,
			Available: u.Available(),
			Snapshot:  u.Circuit.Snapshot(),
		})
	}
	writeJSON(w, out)
}

// upstreamAction handles /admin/upstreams/{name}[/{action}]
func (s *Server) upstreamAction(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/upstreams/")
	parts := strings.SplitN(rest, "/", 2)
	name := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	var u *strategy.Upstream
	for _, cand := range s.Reg.All() {
		if cand.Name == name {
			u = cand
			break
		}
	}
	if u == nil {
		http.Error(w, `{"error":"upstream not found"}`, http.StatusNotFound)
		return
	}

	switch action {
	case "":
		writeJSON(w, map[string]any{
			"upstream": upstreamJSON{
				Name: u.Name, Provider: u.Provider, Protocol: u.Protocol,
				BaseURL: u.BaseURL, Models: u.Models, Priority: u.Priority,
				Weight: u.Weight, Available: u.Available(), Snapshot: u.Circuit.Snapshot(),
			},
			"events": s.Ring.ForUpstream(name, 50),
		})
	case "enable":
		d := u.Circuit.ManualEnable()
		s.Ring.Add(obs.Event{Upstream: name, From: d.From.String(), To: d.To.String(), Reason: "admin: manual enable"})
		writeJSON(w, map[string]string{"status": u.Circuit.Status().String()})
	case "disable":
		d := u.Circuit.ManualDisable("admin: manual disable")
		s.Ring.Add(obs.Event{Upstream: name, From: d.From.String(), To: d.To.String(), Reason: d.Reason})
		writeJSON(w, map[string]string{"status": u.Circuit.Status().String()})
	case "probe":
		if s.ProberOf == nil {
			http.Error(w, `{"error":"no probers configured"}`, http.StatusBadRequest)
			return
		}
		probers := s.ProberOf(name)
		if len(probers) == 0 {
			http.Error(w, `{"error":"no probers for upstream"}`, http.StatusBadRequest)
			return
		}
		client := s.Client
		if client == nil {
			client = http.DefaultClient
		}
		results := make([]map[string]any, 0, len(probers))
		for _, p := range probers {
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			sig := p.Probe(ctx, probe.Target{
				Name: u.Name, BaseURL: u.BaseURL, APIKey: u.APIKey,
				Models: u.Models, Native: u.NativeModel, Client: client,
			})
			cancel()
			u.Feed(sig)
			results = append(results, map[string]any{
				"probe":   p.Name(),
				"ok":      sig.OK,
				"fatal":   sig.Fatal,
				"balance": sig.Balance,
				"reason":  sig.Reason,
			})
		}
		writeJSON(w, results)
	default:
		http.Error(w, `{"error":"unknown action"}`, http.StatusNotFound)
	}
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	type modelEntry struct {
		Name      string `json:"name"`
		Upstreams int    `json:"upstreams"`
		Available int    `json:"available"`
	}
	seen := map[string]bool{}
	var out []modelEntry
	for _, u := range s.Reg.All() {
		for _, m := range u.Models {
			seen[m] = true
		}
	}
	for m := range seen {
		e := modelEntry{Name: m}
		for _, u := range s.Reg.Candidates(m) {
			e.Upstreams++
			if u.Available() {
				e.Available++
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, out)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	writeJSON(w, s.Ring.Since(since))
}
