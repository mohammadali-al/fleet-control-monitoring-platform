package panel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pastebin"
)

// Pastebin API (internal/pastebin, T-0076): one shared scratch pad.
//
//	GET  /api/pastebin   the current pad {text, version}
//	POST /api/pastebin   replace it: body {text, client}; returns {version}
//
// Live updates are pushed on /ws/pastebin (see wsPastebin).
func (s *server) routesPastebin(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/pastebin", s.live(s.getPastebin))
	mux.HandleFunc("POST /api/pastebin", s.live(s.setPastebin))
}

var (
	pastebinStoreMu sync.Mutex
	pastebinStores  = map[*server]*pastebin.Store{}
)

var errPastebinUnconfigured = errors.New("the pastebin is not configured on this server")

// pastebinBodyLimit leaves room for JSON escaping and the envelope around a
// pad of pastebin.MaxBytes; the exact cap is enforced on the decoded text.
const pastebinBodyLimit = 2*pastebin.MaxBytes + 4<<10

// pastebinPing keeps an otherwise idle /ws/pastebin alive through proxies.
const pastebinPing = 25 * time.Second

func (s *server) pastebinStore() (*pastebin.Store, error) {
	pastebinStoreMu.Lock()
	defer pastebinStoreMu.Unlock()
	if st, ok := pastebinStores[s]; ok {
		return st, nil
	}
	dataDir := ""
	if dp, ok := s.be.(dataDirProvider); ok {
		dataDir = strings.TrimSpace(dp.DataDir())
	}
	if dataDir == "" {
		if ob, ok := s.be.(observer); ok {
			if o := ob.Observe(); o != nil {
				switch {
				case o.Jobs != nil && o.Jobs.Dir() != "":
					dataDir = filepath.Dir(o.Jobs.Dir())
				case o.Archive != nil && o.Archive.Root() != "":
					dataDir = filepath.Dir(o.Archive.Root())
				}
			}
		}
	}
	if dataDir == "" {
		return nil, errPastebinUnconfigured
	}
	st, err := pastebin.Open(dataDir)
	if err != nil {
		return nil, err
	}
	pastebinStores[s] = st
	return st, nil
}

func writePastebinErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errPastebinUnconfigured):
		writeJSON(w, http.StatusServiceUnavailable, errBody(err.Error()))
	case errors.Is(err, pastebin.ErrTooLarge):
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
	default:
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
	}
}

type pastebinResponse struct {
	Text    string `json:"text"`
	Version uint64 `json:"version"`
}

func (s *server) getPastebin(w http.ResponseWriter, _ *http.Request) {
	st, err := s.pastebinStore()
	if err != nil {
		writePastebinErr(w, err)
		return
	}
	text, version := st.Get()
	writeJSON(w, http.StatusOK, pastebinResponse{Text: text, Version: version})
}

func (s *server) setPastebin(w http.ResponseWriter, r *http.Request) {
	st, err := s.pastebinStore()
	if err != nil {
		writePastebinErr(w, err)
		return
	}
	var body struct {
		Text   *string `json:"text"`
		Client string  `json:"client"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, pastebinBodyLimit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad request body: "+err.Error()))
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, errBody("bad request body: trailing data"))
		return
	}
	if body.Text == nil {
		writeJSON(w, http.StatusBadRequest, errBody("text is required"))
		return
	}
	version, err := st.Set(*body.Text, clip(body.Client, 64))
	if err != nil {
		writePastebinErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]uint64{"version": version})
}

// wsPastebin streams the shared pad: the current state on connect, then every
// change, as {"text":..,"version":..,"client":..}. A client that falls behind
// is closed with "try again later" and reloads on reconnect.
func (s *server) wsPastebin(w http.ResponseWriter, r *http.Request) {
	if s.be == nil {
		writeJSON(w, http.StatusServiceUnavailable, errBody("the panel has no backend configured"))
		return
	}
	st, err := s.pastebinStore()
	if err != nil {
		writePastebinErr(w, err)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept has already written the reply
	}
	defer c.CloseNow()
	c.SetReadLimit(wsReadLimit)
	ctx := c.CloseRead(r.Context())

	feed, cancel := st.Subscribe()
	defer cancel()
	keepalive := time.NewTicker(pastebinPing)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-feed:
			if !ok {
				_ = c.Close(websocket.StatusTryAgainLater, "lagging: reconnect and reload")
				return
			}
			if err := writeJSONFrame(ctx, c, f); err != nil {
				return
			}
		case <-keepalive.C:
			pctx, pcancel := context.WithTimeout(ctx, wsWriteTimeout)
			err := c.Ping(pctx)
			pcancel()
			if err != nil {
				return
			}
		}
	}
}
