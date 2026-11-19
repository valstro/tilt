package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/pkg/errors"

	"github.com/tilt-dev/tilt/internal/hud/webview"
	"github.com/tilt-dev/tilt/pkg/logger"
	"github.com/tilt-dev/tilt/pkg/model"
	"github.com/tilt-dev/tilt/pkg/model/logstore"
	proto_webview "github.com/tilt-dev/tilt/pkg/webview"
)

type FollowFlag bool

// SessionToken is the HUD session token (the X-Tilt-Token header value) that
// authorizes requests to requireToken-gated HUD server routes.
type SessionToken string

// Header and route names mirror internal/hud/server. They are duplicated here
// rather than imported so the client does not depend on the server package.
const (
	tiltTokenHeaderName   = "X-Tilt-Token"
	websocketTokenPath    = "/api/websocket_token"
	websocketViewPath     = "/ws/view"
	websocketCSRFQueryKey = "csrf"
)

type LogStreamer struct {
	follow     FollowFlag
	url        model.WebURL
	filter     LogFilter
	printer    LogPrinter
	token      SessionToken
	httpClient *http.Client
	dialer     *websocket.Dialer
}

func NewLogStreamer(follow FollowFlag, url model.WebURL, filter LogFilter, printer LogPrinter, token SessionToken) *LogStreamer {
	return &LogStreamer{
		follow:     follow,
		url:        url,
		filter:     filter,
		printer:    printer,
		token:      token,
		httpClient: http.DefaultClient,
		dialer:     websocket.DefaultDialer,
	}
}

func (ls *LogStreamer) Stream(ctx context.Context) error {
	csrf, err := ls.fetchWebsocketToken(ctx)
	if err != nil {
		return err
	}

	url := neturl.URL(ls.url)
	url.Scheme = "ws"
	url.Path = websocketViewPath
	logger.Get(ctx).Debugf("connecting to %s", url.String())

	// The server only upgrades /ws/view when ?csrf= carries the token served
	// by /api/websocket_token. Keep it out of the logged/error URL.
	dialURL := url
	q := dialURL.Query()
	q.Set(websocketCSRFQueryKey, csrf)
	dialURL.RawQuery = q.Encode()

	conn, _, err := ls.dialer.DialContext(ctx, dialURL.String(), nil)
	if err != nil {
		return errors.Wrapf(err, "dialing websocket %s", url.String())
	}
	defer conn.Close()

	wsr := newWebsocketReaderForLogs(conn, bool(ls.follow), ls.filter, ls.printer)
	return wsr.Listen(ctx)
}

// fetchWebsocketToken asks the HUD server for the websocket CSRF token,
// authenticating with the session token.
func (ls *LogStreamer) fetchWebsocketToken(ctx context.Context) (string, error) {
	url := neturl.URL(ls.url)
	url.Scheme = "http"
	url.Path = websocketTokenPath
	url.RawQuery = ""

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url.String(), nil)
	if err != nil {
		return "", errors.Wrapf(err, "building request for %s", url.String())
	}
	req.Header.Set(tiltTokenHeaderName, string(ls.token))

	res, err := ls.httpClient.Do(req)
	if err != nil {
		return "", errors.Wrapf(err, "fetching websocket token from %s", url.String())
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, 4096))
	if err != nil {
		return "", errors.Wrapf(err, "reading websocket token from %s", url.String())
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching websocket token from %s: %s: %s",
			url.String(), res.Status, strings.TrimSpace(string(body)))
	}

	csrf := strings.TrimSpace(string(body))
	if csrf == "" {
		return "", fmt.Errorf("fetching websocket token from %s: empty response", url.String())
	}
	return csrf, nil
}

type WebsocketConn interface {
	NextReader() (int, io.Reader, error)
	Close() error
	NextWriter(messageType int) (io.WriteCloser, error)
}

type WebsocketReader struct {
	conn       WebsocketConn
	persistent bool // whether to keep listening on websocket, or close after first message
	handler    ViewHandler
}

func newWebsocketReaderForLogs(conn WebsocketConn, persistent bool, filter LogFilter, printer LogPrinter) *WebsocketReader {
	ls := newLogViewHandler(filter, printer)
	return newWebsocketReader(conn, persistent, ls)
}

func newWebsocketReader(conn WebsocketConn, persistent bool, handler ViewHandler) *WebsocketReader {
	return &WebsocketReader{
		conn:       conn,
		persistent: persistent,
		handler:    handler,
	}
}

type ViewHandler interface {
	Handle(v *proto_webview.View) error
}

type logViewHandler struct {
	logstore *logstore.LogStore
	// checkpoint tracks the client's latest printed logs.
	//
	// WARNING: The server watermark values CANNOT be used for checkpointing within the client!
	checkpoint logstore.Checkpoint
	// serverWatermark ensures that we don't print any duplicate logs.
	//
	// This value should only be used to compare to other server values, NOT client checkpoints.
	serverWatermark int32
	filter          LogFilter
	printer         LogPrinter
	// isFirstBatch tracks whether we've received the first batch of logs.
	// Tail limit only applies to the first batch (initial history).
	isFirstBatch bool
}

func newLogViewHandler(filter LogFilter, p LogPrinter) *logViewHandler {
	return &logViewHandler{
		filter:       filter,
		logstore:     logstore.NewLogStore(),
		printer:      p,
		isFirstBatch: true,
	}
}

func (ls *logViewHandler) Handle(v *proto_webview.View) error {
	if v == nil || v.LogList == nil || v.LogList.FromCheckpoint == -1 {
		// Server has no new logs to send.
		// Mark first batch as processed so --tail doesn't apply to future logs.
		ls.isFirstBatch = false
		return nil
	}

	segments := v.LogList.Segments
	if v.LogList.FromCheckpoint < ls.serverWatermark {
		// The server is re-sending some logs we already have, so slice them off.
		deleteCount := ls.serverWatermark - v.LogList.FromCheckpoint
		segments = segments[deleteCount:]
	}

	for _, seg := range segments {
		// TODO(maia): secrets???
		ls.logstore.Append(webview.LogSegmentToEvent(seg, v.LogList.Spans), model.SecretSet{})
	}

	lines := ls.logstore.ContinuingLinesWithOptions(ls.checkpoint, logstore.LineOptions{
		SuppressPrefix: ls.filter.SuppressPrefix(),
	})

	// Apply tail limit only on the first batch (initial history).
	// Subsequent batches in follow mode should show all new logs.
	if ls.isFirstBatch {
		lines = ls.filter.Apply(lines)
		ls.isFirstBatch = false
	} else {
		lines = ls.filter.ApplyWithoutTail(lines)
	}

	ls.printer.Print(lines)

	ls.checkpoint = ls.logstore.Checkpoint()
	ls.serverWatermark = v.LogList.ToCheckpoint

	return nil
}

func (wsr *WebsocketReader) Listen(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			messageType, reader, err := wsr.conn.NextReader()
			if err != nil {
				return
			}

			if messageType == websocket.TextMessage {
				err = wsr.handleTextMessage(ctx, reader)
				if err != nil {
					logger.Get(ctx).Errorf("Error streaming logs: %v", err)
				}
				if !wsr.persistent {
					return
				}
			}
		}
	}()

	for {
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			err := ctx.Err()
			if err != context.Canceled {
				return err
			}

			return wsr.conn.Close()
		}
	}
}

func (wsr *WebsocketReader) handleTextMessage(_ context.Context, reader io.Reader) error {
	v := &proto_webview.View{}
	err := json.NewDecoder(reader).Decode(v)
	if err != nil {
		return errors.Wrap(err, "parsing")
	}

	err = wsr.handler.Handle(v)
	if err != nil {
		return errors.Wrap(err, "handling")
	}

	return nil
}
