package upstream

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceaura/ModelSurge/upstream/account"
	"github.com/aceaura/ModelSurge/upstream/contract/upstreamv1"
	"github.com/aceaura/ModelSurge/upstream/ir"
	"github.com/aceaura/ModelSurge/upstream/upstreamstore"
)

func TestKiroExecuteUsesUpstreamRuntimeAndEmitsNDJSON(t *testing.T) {
	const profileARN = "arn:aws:codewhisperer:us-east-1:1:profile/test"
	var logs bytes.Buffer
	oldWriter := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	})
	var gotAuthorization, gotTarget string
	var gotPayload map[string]any
	kiroEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		gotTarget = r.Header.Get("x-amz-target")
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(awsEventFrame([]byte(`{"content":"hello"}`)))
		_, _ = w.Write(awsEventFrame([]byte(`{"usage":{"cacheReadInputTokens":3}}`)))
	}))
	defer kiroEndpoint.Close()

	store, err := upstreamstore.Open(filepath.Join(t.TempDir(), "upstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	acc := &account.Account{Name: "kiro", Type: account.TypeKiro, Enabled: true, Models: map[string]string{"public": "native-model"}, Kiro: &account.KiroAccount{Source: account.SourceRefreshToken, RefreshToken: "refresh", APIRegion: "us-east-1", Token: &account.TokenState{AccessToken: "access-token", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour), ProfileArn: profileARN}}}
	if err := store.Accounts.InsertAccount(acc); err != nil {
		t.Fatal(err)
	}
	if err := store.Accounts.SaveTokenState("kiro", acc.Kiro.Token); err != nil {
		t.Fatal(err)
	}
	if err := store.MaterializeAccounts(); err != nil {
		t.Fatal(err)
	}
	mgr, err := account.NewManager(store.Accounts, account.Cooldowns{}, account.ManagerDeps{})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(store, mgr)
	svc.KiroHTTPClient = &http.Client{Transport: rewriteHostTransport{base: kiroEndpoint.Client().Transport, host: kiroEndpoint.URL}}
	svc.KiroFirstTokenTimeout = time.Second
	h := NewHTTPServer(svc, "service-key")

	canonical, _ := json.Marshal(ir.Request{Model: "public", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "private-body-text"}}}}})
	envelope, _ := json.Marshal(upstreamv1.KiroExecuteRequest{RequestID: "req-kiro", TargetID: "kiro/public", Request: canonical})
	req := httptest.NewRequest(http.MethodPost, upstreamv1.BasePath+"/kiro/execute", bytes.NewReader(envelope))
	req.Header.Set("Authorization", "Bearer service-key")
	req.Header.Set("X-Request-ID", "req-kiro")
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("status=%d content-type=%q body=%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if gotAuthorization != "Bearer access-token" || gotTarget != account.TargetGenerateAssistantResponse {
		t.Fatalf("authorization=%q target=%q", gotAuthorization, gotTarget)
	}
	if gotPayload["profileArn"] != profileARN {
		t.Fatalf("profileArn=%v payload=%v", gotPayload["profileArn"], gotPayload)
	}
	state := gotPayload["conversationState"].(map[string]any)
	current := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
	if current["modelId"] != "native-model" {
		t.Fatalf("modelId=%v", current["modelId"])
	}
	var events []ir.Event
	decoder := json.NewDecoder(strings.NewReader(rec.Body.String()))
	for {
		var event ir.Event
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if len(events) < 4 || events[0].Type != ir.EvMessageStart || events[2].Type != ir.EvMessageDelta && events[len(events)-2].Type != ir.EvMessageDelta {
		t.Fatalf("events=%+v", events)
	}
	logged := logs.String()
	for _, phase := range []string{"phase=provider_out request_id=req-kiro", "phase=provider_in request_id=req-kiro", "phase=stream_first request_id=req-kiro", "phase=stream_done request_id=req-kiro"} {
		if !strings.Contains(logged, phase) {
			t.Errorf("missing %s: %s", phase, logged)
		}
	}
	if !strings.Contains(logged, "phase=stream_done request_id=req-kiro raw_events=2") || strings.Contains(logged, "phase=stream_done request_id=req-kiro raw_events=2 events=4 bytes=0") {
		t.Fatalf("stream totals omitted first-event data: %s", logged)
	}
	for _, forbidden := range []string{"access-token", "Bearer access-token", "Authorization", "private-body-text", `"content":"hello"`} {
		if strings.Contains(logged, forbidden) {
			t.Errorf("Upstream logs leaked %q: %s", forbidden, logged)
		}
	}
}

type rewriteHostTransport struct {
	base http.RoundTripper
	host string
}

func (t rewriteHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL, _ = clone.URL.Parse(t.host + req.URL.Path)
	return t.base.RoundTrip(clone)
}

func awsEventFrame(payload []byte) []byte {
	total := 16 + len(payload)
	frame := make([]byte, total)
	binary.BigEndian.PutUint32(frame[0:4], uint32(total))
	binary.BigEndian.PutUint32(frame[4:8], 0)
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[:8]))
	copy(frame[12:], payload)
	binary.BigEndian.PutUint32(frame[total-4:], crc32.ChecksumIEEE(frame[:total-4]))
	return frame
}
