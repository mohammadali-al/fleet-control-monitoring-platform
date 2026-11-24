package exec

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Dangerous-command gate.
//
// The main server owns a pattern list (regular expressions, matched against
// the whitespace-normalised command text). A command that matches may run only
// if it carries a confirmation token. The token is an HMAC minted by the main
// server after the operator confirms; the agent holds the same secret and
// verifies it itself, so the check is enforced agent-side and a browser-side
// "ok" (which cannot produce a valid HMAC) never suffices.
//
// Token format (opaque to callers):
//
//	v1.<expiry unix seconds>.<hex HMAC-SHA256(key, machineID NUL command NUL expiry)>
//
// A token is bound to one machine and one exact (normalised) command, expires,
// and is single-use on the verifying side.

// Errors reported by the gate.
var (
	ErrConfirmationRequired = errors.New("exec: dangerous command requires a confirmation token")
	ErrConfirmationInvalid  = errors.New("exec: confirmation token is invalid, expired or already used")
)

// DefaultTokenTTL is how long a minted token stays valid.
const DefaultTokenTTL = 2 * time.Minute

// DefaultPatterns is the built-in dangerous-command list; the server's config
// replaces it via SetPatterns.
func DefaultPatterns() []string {
	return []string{
		`\brm\s+(-[a-zA-Z]*[rRf][a-zA-Z]*\s+)+(--no-preserve-root\s+)?/(\s|\*|$)`, // rm -rf /
		`\brm\s+.*--no-preserve-root`,
		`\bmkfs(\.\w+)?\b`,
		`\bdd\s+.*\bof=/dev/`,
		`>\s*/dev/(sd|nvme|vd|xvd|mmcblk)\w*`,
		`:\(\)\s*\{.*\};\s*:`, // fork bomb
		`\b(shutdown|poweroff|halt)\b`,
		`\breboot\b`,
		`\binit\s+[06]\b`,
		`\bchmod\s+(-\w+\s+)*[0-7]*777\s+/(\s|$)`,
	}
}

// Gate holds the pattern list and the confirmation secret.
type Gate struct {
	mu       sync.Mutex
	patterns []*regexp.Regexp
	key      []byte
	used     map[string]int64 // token -> expiry, for single-use
	now      func() time.Time
}

// NewGate builds a gate. key is the shared confirmation secret (required);
// patterns nil means DefaultPatterns.
func NewGate(key []byte, patterns []string) (*Gate, error) {
	if len(key) == 0 {
		return nil, errors.New("exec: gate needs a confirmation key")
	}
	g := &Gate{key: append([]byte(nil), key...), used: map[string]int64{}, now: time.Now}
	if patterns == nil {
		patterns = DefaultPatterns()
	}
	if err := g.SetPatterns(patterns); err != nil {
		return nil, err
	}
	return g, nil
}

// SetPatterns replaces the pattern list atomically; on a bad pattern the old
// list is kept.
func (g *Gate) SetPatterns(patterns []string) error {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return fmt.Errorf("exec: bad gate pattern %q: %w", p, err)
		}
		compiled = append(compiled, re)
	}
	g.mu.Lock()
	g.patterns = compiled
	g.mu.Unlock()
	return nil
}

// normalize collapses runs of whitespace so that "rm   -rf /" and
// "rm -rf /" are the same command for matching and for token binding.
func normalize(cmd string) string { return strings.Join(strings.Fields(cmd), " ") }

// Match reports whether cmd matches a dangerous pattern, and which one.
func (g *Gate) Match(cmd string) (pattern string, matched bool) {
	n := normalize(cmd)
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, re := range g.patterns {
		if re.MatchString(n) {
			return re.String(), true
		}
	}
	return "", false
}

func (g *Gate) mac(machineID, cmd string, expiry int64) string {
	h := hmac.New(sha256.New, g.key)
	fmt.Fprintf(h, "%s\x00%s\x00%d", machineID, normalize(cmd), expiry)
	return hex.EncodeToString(h.Sum(nil))
}

// Mint issues a confirmation token for running cmd on machineID. It is called
// by the main server once the operator has confirmed.
func (g *Gate) Mint(machineID, cmd string, ttl time.Duration) string {
	if ttl <= 0 {
		ttl = DefaultTokenTTL
	}
	exp := g.now().Add(ttl).Unix()
	return fmt.Sprintf("v1.%d.%s", exp, g.mac(machineID, cmd, exp))
}

// Authorize decides whether cmd may run on machineID with the given token. A
// command that matches no pattern is always allowed (a token is ignored and
// not consumed). A matching command needs a valid, unexpired, unused token for
// exactly this machine and command; a valid token is consumed.
func (g *Gate) Authorize(machineID, cmd, token string) error {
	if _, hit := g.Match(cmd); !hit {
		return nil
	}
	if token == "" {
		return ErrConfirmationRequired
	}
	return g.consume(machineID, cmd, token)
}

// Valid reports whether token is currently valid for (machineID, cmd) without
// consuming it.
func (g *Gate) Valid(machineID, cmd, token string) bool {
	_, ok := g.check(machineID, cmd, token)
	return ok
}

func (g *Gate) check(machineID, cmd, token string) (exp int64, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return 0, false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || g.now().Unix() > exp {
		return 0, false
	}
	want := g.mac(machineID, cmd, exp)
	if !hmac.Equal([]byte(want), []byte(parts[2])) {
		return 0, false
	}
	g.mu.Lock()
	_, replay := g.used[token]
	g.mu.Unlock()
	return exp, !replay
}

func (g *Gate) consume(machineID, cmd, token string) error {
	exp, ok := g.check(machineID, cmd, token)
	if !ok {
		return ErrConfirmationInvalid
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, replay := g.used[token]; replay { // lost a race with another consumer
		return ErrConfirmationInvalid
	}
	now := g.now().Unix()
	for t, e := range g.used { // prune expired entries
		if e < now {
			delete(g.used, t)
		}
	}
	g.used[token] = exp
	return nil
}

// Refusal describes one refused command or PTY input line.
type Refusal struct {
	Line   string // the command or line text
	Reason string
}

// lineFilter gates PTY input. Input bytes are forwarded to the shell as they
// arrive (so editing and echo work) while the filter tracks the line being
// typed. When Enter arrives for a line that matches the gate and has no valid
// token, the Enter is withheld and replaced with Ctrl-U, which makes the shell
// discard the line, and a Refusal is reported.
//
// Limitation (documented, deliberate): bytes the filter cannot model --
// cursor/history escape sequences and most control keys -- make the
// line "opaque". Unless allowOpaque is set, Enter on an opaque line is
// refused, because the agent cannot know what the shell would run. Setting
// allowOpaque trades that for usable TUIs and history, matching only the
// text the filter saw. Aliases, scripts and subshells are out of the gate's
// reach in either mode: it is a guard against accidents, not a sandbox.
type lineFilter struct {
	gate        *Gate
	machineID   string
	allowOpaque bool

	buf    []byte
	opaque bool

	esc    int // 0 none, 1 after ESC, 2 in CSI, 3 after ESC O (one more byte)
	csi    []byte
	grants []string // tokens pre-authorised by a confirm message
}

const (
	ctrlC = 0x03
	ctrlU = 0x15
	ctrlW = 0x17
	escB  = 0x1b
)

// grant stores a token delivered by a confirm message; it is tried against the
// next matching line.
func (f *lineFilter) grant(token string) {
	if token != "" {
		f.grants = append(f.grants, token)
	}
}

func (f *lineFilter) reset() { f.buf, f.opaque = f.buf[:0], false }

// feed processes data and returns the bytes to forward to the PTY plus any
// refusals. token (may be empty) authorises matching lines completed in data.
func (f *lineFilter) feed(data []byte, token string) (forward []byte, refusals []Refusal) {
	out := make([]byte, 0, len(data))
	for _, b := range data {
		// Escape sequences are forwarded untouched but taint the line.
		switch f.esc {
		case 1:
			out = append(out, b)
			switch b {
			case '[': // CSI: tainted once we know it is not a paste marker
				f.esc, f.csi = 2, f.csi[:0]
				continue
			case 'O':
				f.esc = 3
			default:
				f.esc = 0
			}
			f.opaque = true
			continue
		case 2:
			out = append(out, b)
			f.csi = append(f.csi, b)
			if b >= 0x40 && b <= 0x7e {
				f.esc = 0
				// Bracketed-paste markers do not edit the line.
				if s := string(f.csi); s != "200~" && s != "201~" {
					f.opaque = true
				}
			}
			continue
		case 3:
			out = append(out, b)
			f.esc = 0
			f.opaque = true
			continue
		}

		switch {
		case b == escB:
			f.esc = 1
			out = append(out, b)
		case b == '\r' || b == '\n':
			if r, refused := f.decide(string(f.buf), token); refused {
				out = append(out, ctrlU) // make the shell discard the typed line
				refusals = append(refusals, r)
			} else {
				out = append(out, b)
			}
			f.reset()
		case b == 0x7f || b == 0x08:
			if n := len(f.buf); n > 0 {
				_, size := utf8.DecodeLastRune(f.buf)
				f.buf = f.buf[:n-size]
			}
			out = append(out, b)
		case b == ctrlU || b == ctrlC:
			f.reset()
			out = append(out, b)
		case b == ctrlW:
			s := strings.TrimRight(string(f.buf), " \t")
			if i := strings.LastIndexAny(s, " \t"); i >= 0 {
				s = s[:i+1]
			} else {
				s = ""
			}
			f.buf = append(f.buf[:0], s...)
			out = append(out, b)
		case b == '\t':
			out = append(out, b) // completion only appends; not tracked, does not taint
		case b < 0x20:
			f.opaque = true
			out = append(out, b)
		default:
			f.buf = append(f.buf, b)
			out = append(out, b)
		}
	}
	return out, refusals
}

// decide applies the gate to a completed line.
func (f *lineFilter) decide(line, token string) (Refusal, bool) {
	if f.opaque && !f.allowOpaque {
		return Refusal{Line: line, Reason: "line was edited with keys the agent cannot track (history, cursor); retype it"}, true
	}
	if _, hit := f.gate.Match(line); !hit {
		return Refusal{}, false
	}
	// Try the inline token first, then pre-authorised grants.
	if token != "" && f.gate.consume(f.machineID, line, token) == nil {
		return Refusal{}, false
	}
	for i, g := range f.grants {
		if f.gate.consume(f.machineID, line, g) == nil {
			f.grants = append(f.grants[:i], f.grants[i+1:]...)
			return Refusal{}, false
		}
	}
	reason := ErrConfirmationRequired.Error()
	if token != "" {
		reason = ErrConfirmationInvalid.Error()
	}
	return Refusal{Line: line, Reason: reason}, true
}
