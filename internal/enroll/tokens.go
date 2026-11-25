package enroll

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"time"
)

// DefaultTokenTTL is how long a minted token stays redeemable.
const DefaultTokenTTL = 10 * time.Minute

// codeAlphabet has no look-alike characters (no 0/O, 1/I/L), so a code read
// aloud or typed from a screen survives. 8 characters of 31 symbols is about
// 40 bits; with the 10-minute lifetime and the failed-attempt limiter on the
// endpoint, guessing a live code is not practical.
const (
	codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	codeLen      = 8
)

// Token errors. The HTTP endpoint answers all of them with the same 401 so an
// unauthenticated caller learns nothing about which tokens exist.
var (
	ErrTokenInvalid = errors.New("enroll: unknown token")
	ErrTokenExpired = errors.New("enroll: token expired")
	ErrTokenUsed    = errors.New("enroll: token already used")
)

// Token is one enrollment token. It is a credential: it is never logged.
type Token struct {
	// Token is the long secret the script presents; Code is the short
	// human-typable form that identifies the same token (it fetches the
	// script, which carries the token).
	Token string
	Code  string
	// Role is what the machine enrolls as (worker|monitor).
	Role      string
	CreatedBy string
	CreatedAt time.Time
	ExpiresAt time.Time
	// RedeemedBy is the machine id the token bound to; empty until redeemed.
	RedeemedBy string
	RedeemedAt time.Time
}

type tokenStore struct {
	mu     sync.Mutex
	now    func() time.Time
	ttl    time.Duration
	tokens map[string]*Token // by Token
	codes  map[string]*Token // by Code
}

func newTokenStore(now func() time.Time, ttl time.Duration) *tokenStore {
	if now == nil {
		now = time.Now
	}
	if ttl <= 0 {
		ttl = DefaultTokenTTL
	}
	return &tokenStore{now: now, ttl: ttl, tokens: map[string]*Token{}, codes: map[string]*Token{}}
}

func (s *tokenStore) mint(role, by string) (Token, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Token{}, err
	}
	code, err := randomCode()
	if err != nil {
		return Token{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked()
	for s.codes[code] != nil { // vanishingly rare; codes only need to be unique among live tokens
		if code, err = randomCode(); err != nil {
			return Token{}, err
		}
	}
	now := s.now()
	t := &Token{
		Token: base64.RawURLEncoding.EncodeToString(raw[:]), Code: code, Role: role, CreatedBy: by,
		CreatedAt: now, ExpiresAt: now.Add(s.ttl),
	}
	s.tokens[t.Token] = t
	s.codes[t.Code] = t
	return *t, nil
}

func randomCode() (string, error) {
	var raw [codeLen]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, x := range raw {
		b.WriteByte(codeAlphabet[int(x)%len(codeAlphabet)])
	}
	return b.String(), nil
}

// purgeLocked forgets tokens that expired a while ago (kept briefly so a late
// redemption reports "expired" rather than "unknown").
func (s *tokenStore) purgeLocked() {
	cut := s.now().Add(-time.Hour)
	for k, t := range s.tokens {
		if t.ExpiresAt.Before(cut) {
			delete(s.tokens, k)
			delete(s.codes, t.Code)
		}
	}
}

// check validates a token without consuming it (downloads before the
// redemption). viaCode lets the short code stand for the token (the script
// fetch only).
func (s *tokenStore) check(cred string, viaCode bool) (Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tokens[cred]
	if t == nil && viaCode {
		t = s.codes[strings.ToUpper(strings.TrimSpace(cred))]
	}
	return s.liveLocked(t)
}

func (s *tokenStore) liveLocked(t *Token) (Token, error) {
	switch {
	case t == nil:
		return Token{}, ErrTokenInvalid
	case t.RedeemedBy != "":
		return Token{}, ErrTokenUsed
	case !s.now().Before(t.ExpiresAt):
		return Token{}, ErrTokenExpired
	}
	return *t, nil
}

// redeem runs fn for a live token and, only if fn succeeds, binds the token to
// the machine id fn returns. The store is locked throughout, so two machines
// racing for one token cannot both win; a failure in fn (bad request, refused
// role) leaves the token usable until it expires.
func (s *tokenStore) redeem(secret string, fn func(Token) (string, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.liveLocked(s.tokens[secret]) // the secret only: the short code cannot redeem
	if err != nil {
		return err
	}
	id, err := fn(t)
	if err != nil {
		return err
	}
	live := s.tokens[secret]
	live.RedeemedBy = id
	live.RedeemedAt = s.now()
	return nil
}
