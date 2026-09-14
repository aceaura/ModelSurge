package upstreamclient

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
	"github.com/aceaura/ModelSurge/upstream/contract/upstreamv1"
)

func TestClientUsesAuthenticatedPublicContract(t *testing.T) {
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer service-key" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(upstreamv1.ErrorEnvelope{Error: upstreamv1.Error{Code: upstreamv1.CodeUnauthorized, Message: "bad key"}})
			return false
		}
		return true
	}
	mux.HandleFunc("GET /internal/v1/health", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) {
			writeTestJSON(w, upstreamv1.HealthResponse{Status: "ok"})
		}
	})
	mux.HandleFunc("GET /internal/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) {
			writeTestJSON(w, upstreamv1.ModelsResponse{Models: []upstreamv1.ModelSummary{{ID: "a/m"}}})
		}
	})
	mux.HandleFunc("GET /internal/v1/models/", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) {
			writeTestJSON(w, upstreamv1.ResolvedTarget{ID: "a/m", Protocol: "openai-chat", NativeModel: "native", BaseURL: "https://example.test", APIKey: "secret"})
		}
	})
	mux.HandleFunc("POST /internal/v1/candidates/evaluate", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) {
			writeTestJSON(w, upstreamv1.EvaluateResponse{Candidates: []upstreamv1.CandidateEvaluation{{ID: "a/m", Available: true}}})
		}
	})
	mux.HandleFunc("POST /internal/v1/results", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) {
			writeTestJSON(w, upstreamv1.ResultResponse{Applied: true})
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	if err := New(server.URL, "wrong", 0).Health(context.Background()); err == nil {
		t.Fatal("wrong service key unexpectedly authenticated")
	}
	client := New(server.URL, "service-key", 0)
	models, err := client.Models(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "a/m" {
		t.Fatalf("models=%+v err=%v", models, err)
	}
	target, err := client.Resolve(context.Background(), "a/m")
	if err != nil || target.APIKey != "secret" {
		t.Fatalf("target=%+v err=%v", target, err)
	}
	evals, err := client.Evaluate(context.Background(), []string{"a/m"})
	if err != nil || len(evals) != 1 || !evals[0].Available {
		t.Fatalf("evals=%+v err=%v", evals, err)
	}
	if _, err := client.Report(context.Background(), upstreamv1.ResultReport{ReportID: "r", TargetID: "a/m", Outcome: "normal"}); err != nil {
		t.Fatal(err)
	}
}

func TestControlLogsAndPropagatesRequestIDWithoutServiceKey(t *testing.T) {
	var headerRequestID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headerRequestID = r.Header.Get("X-Request-ID")
		writeTestJSON(w, upstreamv1.EvaluateResponse{Candidates: []upstreamv1.CandidateEvaluation{{ID: "a/m", Available: true}}})
	}))
	defer server.Close()

	var logs bytes.Buffer
	oldWriter := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	})
	client := New(server.URL, "service-secret", 0)
	ctx := replayv1.WithRequestID(context.Background(), "req-control")
	if _, err := client.Evaluate(ctx, []string{"a/m"}); err != nil {
		t.Fatal(err)
	}
	got := logs.String()
	for _, phase := range []string{"phase=control_out request_id=req-control", "phase=control_in request_id=req-control"} {
		if !strings.Contains(got, phase) {
			t.Errorf("missing %s: %s", phase, got)
		}
	}
	if headerRequestID != "req-control" {
		t.Fatalf("X-Request-ID=%q", headerRequestID)
	}
	if strings.Contains(got, "service-secret") || strings.Contains(got, "Authorization") {
		t.Fatalf("upstream client logs leaked credential: %s", got)
	}
}

func writeTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
