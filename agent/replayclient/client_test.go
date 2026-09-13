package replayclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

func TestDispatchUsesServiceAuthAndContract(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"request_id":"r","group_id":"g","target_id":"t","protocol":"openai-chat","native_model":"n","base_url":"http://provider","credential":"secret"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "svc", time.Second)
	lease, err := c.Dispatch(context.Background(), replayv1.DispatchRequest{Model: "m", InboundProtocol: "openai-chat", ClientKey: "client", RequestID: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer svc" || lease.TargetID != "t" {
		t.Fatalf("auth=%q lease=%+v", auth, lease)
	}
}
