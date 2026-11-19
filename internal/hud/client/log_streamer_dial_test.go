package client

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tilt-dev/tilt/pkg/logger"
	"github.com/tilt-dev/tilt/pkg/model"
	proto_webview "github.com/tilt-dev/tilt/pkg/webview"
)

const (
	dialTestSessionToken = "session-token-xyz"
	dialTestCSRFToken    = "csrf-token-123"
)

// fakeHUD mimics the HUD server's auth: /api/websocket_token requires the
// session token, and /ws/view only upgrades when ?csrf= matches.
type fakeHUD struct {
	t   *testing.T
	srv *httptest.Server

	mu           sync.Mutex
	tokenHeaders []string
	wsQueries    []url.Values
}

func newFakeHUD(t *testing.T) *fakeHUD {
	f := &fakeHUD{t: t}
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return r.URL.Query().Get("csrf") == dialTestCSRFToken
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/websocket_token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.tokenHeaders = append(f.tokenHeaders, r.Header.Get("X-Tilt-Token"))
		f.mu.Unlock()
		if r.Header.Get("X-Tilt-Token") != dialTestSessionToken {
			http.Error(w, "invalid session token", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(dialTestCSRFToken))
	})
	mux.HandleFunc("/ws/view", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.wsQueries = append(f.wsQueries, r.URL.Query())
		f.mu.Unlock()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		view := &proto_webview.View{
			LogList: &proto_webview.LogList{
				Spans: map[string]*proto_webview.LogSpan{"foo": {ManifestName: "foo"}},
				Segments: []*proto_webview.LogSegment{
					{SpanId: "foo", Text: "hello from foo\n"},
				},
				FromCheckpoint: 0,
				ToCheckpoint:   1,
			},
		}
		b, err := json.Marshal(view)
		require.NoError(t, err)
		_ = conn.WriteMessage(websocket.TextMessage, b)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHUD) webURL() model.WebURL {
	u, err := url.Parse(f.srv.URL)
	require.NoError(f.t, err)
	return model.WebURL(*u)
}

func newDialTestStreamer(t *testing.T, u model.WebURL, token SessionToken) (*LogStreamer, *bytes.Buffer) {
	out := &bytes.Buffer{}
	filter := NewLogFilter(FilterSourceAll, FilterResources{}, FilterLevel(logger.InfoLvl),
		FilterSince{}, FilterTail(-1), FilterJSON(false))
	return NewLogStreamer(FollowFlag(false), u, filter, NewIncrementalPrinter(Stdout(out)), token), out
}

func TestLogStreamerFetchesCSRFTokenAndDialsWithIt(t *testing.T) {
	hud := newFakeHUD(t)
	ls, out := newDialTestStreamer(t, hud.webURL(), dialTestSessionToken)

	ctx, cancel := context.WithTimeout(logger.WithLogger(context.Background(), logger.NewTestLogger(&bytes.Buffer{})), 5*time.Second)
	defer cancel()
	require.NoError(t, ls.Stream(ctx))

	hud.mu.Lock()
	defer hud.mu.Unlock()
	assert.Equal(t, []string{dialTestSessionToken}, hud.tokenHeaders,
		"websocket token should be fetched once with the session token")
	require.Len(t, hud.wsQueries, 1)
	assert.Equal(t, dialTestCSRFToken, hud.wsQueries[0].Get("csrf"))
	assert.Contains(t, out.String(), "hello from foo")
}

func TestLogStreamerReportsRejectedSessionToken(t *testing.T) {
	hud := newFakeHUD(t)
	ls, _ := newDialTestStreamer(t, hud.webURL(), "wrong-session-token")

	ctx, cancel := context.WithTimeout(logger.WithLogger(context.Background(), logger.NewTestLogger(&bytes.Buffer{})), 5*time.Second)
	defer cancel()
	err := ls.Stream(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/api/websocket_token")
	assert.Contains(t, err.Error(), "403")
	assert.Contains(t, err.Error(), "invalid session token")

	hud.mu.Lock()
	defer hud.mu.Unlock()
	assert.Empty(t, hud.wsQueries, "should not dial the websocket without a csrf token")
}
