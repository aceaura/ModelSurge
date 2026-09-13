package server_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relayd/backend/account"
	"relayd/backend/config"
	"relayd/backend/relaystore"
	"relayd/backend/schedule"
	"relayd/backend/server"
	"relayd/backend/upstream"
	"relayd/backend/upstreamclient"
	"relayd/backend/upstreamstore"
)

func TestThreeProcessProductionChainNonStreamingAndStreaming(t *testing.T) {
	var providerBodies []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		providerBodies = append(providerBodies, string(body))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"x\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"native\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"through-ir\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"))
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer provider.Close()

	upStore, err := upstreamstore.Open(filepath.Join(t.TempDir(), "upstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer upStore.Close()
	if err := upStore.Accounts.InsertAccount(&account.Account{Name: "a", Type: account.TypeAPIKey, Enabled: true, Protocol: "anthropic", BaseURL: provider.URL, APIKey: "provider-key", Models: map[string]string{"public": "native"}}); err != nil {
		t.Fatal(err)
	}
	if err := upStore.MaterializeAccounts(); err != nil {
		t.Fatal(err)
	}
	manager, err := account.NewManager(upStore.Accounts, account.Cooldowns{}, account.ManagerDeps{})
	if err != nil {
		t.Fatal(err)
	}
	upHTTP := httptest.NewServer(upstream.NewHTTPServer(upstream.NewService(upStore, manager), "service-key").Handler())
	defer upHTTP.Close()
	control := upstreamclient.New(upHTTP.URL, "service-key", 0)
	if err := control.Health(context.Background()); err != nil {
		t.Fatal(err)
	}

	relayStore, err := relaystore.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer relayStore.Close()
	ctx := context.Background()
	if err := relayStore.PutUserModel(ctx, relaystore.UserModel{Name: "public", Protocol: "anthropic", APIKey: "client-key", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := relayStore.PutGroup(ctx, relaystore.Group{ID: "g", UserModel: "public", PolicyType: "sticky", PolicyConfig: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := relayStore.AddMembers(ctx, "g", []string{"a/public"}); err != nil {
		t.Fatal(err)
	}
	relayHTTP := httptest.NewServer(server.NewRelay(&config.Config{}, &schedule.Scheduler{Store: relayStore, Upstream: control}).Handler())
	defer relayHTTP.Close()

	call := func(stream bool) (int, string) {
		body := ` { "messages" : [ { "content" : "hi", "role" : "user" } ], "max_tokens" : 10, "model" : "public", "stream" : `
		if stream {
			body += "true }"
		} else {
			body += "false }"
		}
		r, _ := http.NewRequest(http.MethodPost, relayHTTP.URL+"/v1/messages", strings.NewReader(body))
		r.Header.Set("x-api-key", "client-key")
		r.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}
	status, body := call(false)
	if status != 200 || !strings.Contains(body, "through-ir") {
		t.Fatalf("non-stream status=%d body=%s", status, body)
	}
	status, body = call(true)
	if status != 200 || !strings.Contains(body, "through-ir") || !strings.Contains(body, "event:") {
		t.Fatalf("stream status=%d body=%s", status, body)
	}
	if len(providerBodies) != 2 {
		t.Fatalf("provider calls=%d", len(providerBodies))
	}
	for _, body := range providerBodies {
		if !strings.Contains(body, `"model":"native"`) || strings.HasPrefix(body, ` { "messages"`) {
			t.Fatalf("same-protocol request was not decoded and re-encoded through IR: %s", body)
		}
	}
	var reports int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := upStore.DB.QueryRow(`SELECT COUNT(*) FROM result_reports`).Scan(&reports); err != nil {
			t.Fatal(err)
		}
		if reports == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if reports != 2 {
		t.Fatalf("reports=%d", reports)
	}
}
