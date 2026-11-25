package enroll

// The operator side of token enrollment: what the Machines page calls. It is
// mounted next to the panel (behind the auth proxy) and trusts the proxy's
// identity headers the same way the panel does.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

// APIPrefix is where APIHandler expects to be mounted.
const APIPrefix = "/api/enroll/"

// APIHandler serves:
//
//	POST /api/enroll/tokens                 {role}  -> code, expiry, Linux and macOS one-liners
//	GET  /api/enroll/script?role=worker     the bootstrap script, read-only ("show script")
//	GET  /api/enroll/machines               the token-enrolled machines, with specs
//	POST /api/enroll/machines/{id}/stop     disable the service (stays down across reboot)
//	POST /api/enroll/machines/{id}/detach   revoke the certificate and forget the machine
func (s *Service) APIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+APIPrefix+"tokens", s.apiMint)
	mux.HandleFunc("GET "+APIPrefix+"script", s.apiScript)
	mux.HandleFunc("GET "+APIPrefix+"machines", s.apiMachines)
	mux.HandleFunc("POST "+APIPrefix+"machines/{id}/stop", s.apiStop)
	mux.HandleFunc("POST "+APIPrefix+"machines/{id}/detach", s.apiDetach)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		// A state-changing request must be JSON: a cross-site HTML form cannot
		// send that without a preflight.
		if r.Method == http.MethodPost && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			writeJSON(w, http.StatusUnsupportedMediaType, errBody("Content-Type must be application/json"))
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func operator(r *http.Request) string {
	for _, h := range []string{"Remote-User", "X-Forwarded-User", "X-Auth-Request-User", "X-Pomerium-Claim-Email"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			if len(v) > 64 {
				v = v[:64]
			}
			return v
		}
	}
	return "operator"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errBody(msg string) map[string]string { return map[string]string{"error": msg} }

func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrBadRequest), errors.Is(err, ErrMacWorker):
		return http.StatusBadRequest
	case errors.Is(err, registry.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrNotEnrolled):
		return http.StatusConflict
	case errors.Is(err, ErrStopUnsupported):
		return http.StatusNotImplemented
	default:
		return http.StatusBadGateway
	}
}

type mintRequest struct {
	Role string `json:"role"`
}

type mintResponse struct {
	Code       string    `json:"code"`
	Role       string    `json:"role"`
	ExpiresAt  time.Time `json:"expires_at"`
	TTLSeconds int       `json:"ttl_seconds"`
	Linux      string    `json:"linux"`
	MacOS      string    `json:"macos,omitempty"` // empty for a worker: macOS is monitor-only
	URL        string    `json:"url"`
}

func (s *Service) apiMint(w http.ResponseWriter, r *http.Request) {
	var body mintRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad request body: "+err.Error()))
		return
	}
	m, err := s.Mint(body.Role, operator(r))
	if err != nil {
		writeJSON(w, statusFor(err), errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, mintResponse{
		Code: m.Token.Code, Role: m.Token.Role, ExpiresAt: m.Token.ExpiresAt,
		TTLSeconds: int(m.Token.ExpiresAt.Sub(m.Token.CreatedAt).Seconds()),
		Linux:      m.Linux, MacOS: m.MacOS, URL: s.BaseURL(),
	})
}

func (s *Service) apiScript(w http.ResponseWriter, r *http.Request) {
	script, err := s.Script(r.URL.Query().Get("role"), "")
	if err != nil {
		writeJSON(w, statusFor(err), errBody(err.Error()))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(script))
}

func (s *Service) apiMachines(w http.ResponseWriter, _ *http.Request) {
	ms := s.Machines()
	if ms == nil {
		ms = []Machine{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"machines": ms})
}

func (s *Service) apiStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Stop(r.Context(), id); err != nil {
		msg := err.Error()
		if errors.Is(err, ErrStopUnsupported) {
			msg = "This agent cannot be stopped from here (monitor agents have no command channel). " +
				"Run `sudo fleet-agent stop` on the machine, or Detach it."
		}
		writeJSON(w, statusFor(err), errBody(msg))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stopped": id})
}

func (s *Service) apiDetach(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Detach(r.Context(), id); err != nil {
		writeJSON(w, statusFor(err), errBody(err.Error()))
		return
	}
	s.log.Info("detach requested", "component", "enroll", "machine", id, "by", operator(r))
	writeJSON(w, http.StatusOK, map[string]any{"detached": id})
}
