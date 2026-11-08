package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestResponsesWebsocketReaderCountsModelOperations(t *testing.T) {
	rate := &usage.RequestRate{}
	received := make(chan struct{}, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Error(errUpgrade)
			return
		}
		defer func() {
			if errClose := conn.Close(); errClose != nil {
				t.Error(errClose)
			}
		}()
		ctx, cancel := context.WithCancelCause(usage.WithRequestRate(r.Context(), rate))
		defer cancel(nil)
		input, done := readResponsesWebsocketInput(ctx, cancel, conn, func([]byte) error { return nil }, newResponsesLocalInterrupt(), newResponsesWebsocketWriter(conn), nil, &atomic.Uint64{})
		for range input {
			received <- struct{}{}
		}
		<-done
	}))
	defer server.Close()
	conn, _, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if errDial != nil {
		t.Fatal(errDial)
	}
	defer func() { _ = conn.Close() }()
	for _, frame := range []string{`{"type":"response.create"}`, `{"type":"response.interrupt"}`, `{"type":"response.append"}`, `{"type":"unknown"}`} {
		if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(frame)); errWrite != nil {
			t.Fatal(errWrite)
		}
	}
	for range 3 {
		select {
		case <-received:
		case <-time.After(5 * time.Second):
			t.Fatal("reader did not deliver operations")
		}
	}
	if got := rate.Count(time.Now()); got != 2 {
		t.Fatalf("RPM counted control frames or missed model operations: %d", got)
	}
}
