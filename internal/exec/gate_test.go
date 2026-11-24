package exec

import (
	"errors"
	"testing"
	"time"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

func newTestGate(t *testing.T, patterns []string) *Gate {
	t.Helper()
	g, err := NewGate(testKey, patterns)
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	return g
}

func TestGateNeedsKey(t *testing.T) {
	if _, err := NewGate(nil, nil); err == nil {
		t.Fatal("NewGate without a key succeeded")
	}
	if _, err := NewGate(testKey, []string{"("}); err == nil {
		t.Fatal("NewGate with a bad pattern succeeded")
	}
}

func TestDefaultPatternsMatch(t *testing.T) {
	g := newTestGate(t, nil)
	for _, c := range []string{
		"rm -rf /", "rm   -rf   /", "sudo rm -fr / ", "rm -rf /*", "mkfs.ext4 /dev/sda1",
		"dd if=/dev/zero of=/dev/sda", "shutdown -h now", "reboot", ":(){ :|:& };:",
		"echo x > /dev/sda",
	} {
		if _, hit := g.Match(c); !hit {
			t.Errorf("default patterns do not match %q", c)
		}
	}
	for _, c := range []string{
		"ls -l /", "echo hello", "rm -rf /tmp/build", "rm file", "cat /etc/hostname", "echo rebooting soon is fine",
	} {
		if p, hit := g.Match(c); hit {
			t.Errorf("default pattern %q wrongly matches %q", p, c)
		}
	}
}

func TestGateAuthorize(t *testing.T) {
	g := newTestGate(t, []string{`DANGER`})
	const m = "agent-01"

	// Not dangerous: always allowed, token irrelevant.
	if err := g.Authorize(m, "echo safe", ""); err != nil {
		t.Fatalf("safe command refused: %v", err)
	}

	cmd := "echo  DANGER now"
	if err := g.Authorize(m, cmd, ""); !errors.Is(err, ErrConfirmationRequired) {
		t.Fatalf("no token: err = %v, want ErrConfirmationRequired", err)
	}
	// A browser-side "ok" (or any guessable value) is not a token.
	for _, bad := range []string{"ok", "true", "confirmed", "v1.0.deadbeef", "v1.99999999999.deadbeef", "v1..", "x.y.z"} {
		if err := g.Authorize(m, cmd, bad); !errors.Is(err, ErrConfirmationInvalid) {
			t.Errorf("token %q: err = %v, want ErrConfirmationInvalid", bad, err)
		}
	}
	// A token minted with another key (a forging browser) is refused.
	other, _ := NewGate([]byte("another-secret-another-secret-00"), []string{`DANGER`})
	if err := g.Authorize(m, cmd, other.Mint(m, cmd, time.Minute)); !errors.Is(err, ErrConfirmationInvalid) {
		t.Errorf("forged token: err = %v", err)
	}
	// Bound to the machine and to the command.
	tok := g.Mint(m, cmd, time.Minute)
	if err := g.Authorize("agent-02", cmd, tok); !errors.Is(err, ErrConfirmationInvalid) {
		t.Errorf("token for another machine accepted: %v", err)
	}
	if err := g.Authorize(m, "echo DANGER later", tok); !errors.Is(err, ErrConfirmationInvalid) {
		t.Errorf("token for another command accepted: %v", err)
	}
	// Valid token works once (whitespace-normalised), then is spent.
	if err := g.Authorize(m, "echo DANGER now", tok); err != nil {
		t.Fatalf("valid token refused: %v", err)
	}
	if err := g.Authorize(m, cmd, tok); !errors.Is(err, ErrConfirmationInvalid) {
		t.Errorf("replayed token accepted: %v", err)
	}
}

func TestGateTokenExpiry(t *testing.T) {
	g := newTestGate(t, []string{`DANGER`})
	now := time.Now()
	g.now = func() time.Time { return now }
	tok := g.Mint("a", "echo DANGER", time.Minute)
	g.now = func() time.Time { return now.Add(2 * time.Minute) }
	if err := g.Authorize("a", "echo DANGER", tok); !errors.Is(err, ErrConfirmationInvalid) {
		t.Fatalf("expired token accepted: %v", err)
	}
}

func TestGateSetPatterns(t *testing.T) {
	g := newTestGate(t, []string{`AAA`})
	if err := g.SetPatterns([]string{"("}); err == nil {
		t.Fatal("bad pattern accepted")
	}
	if _, hit := g.Match("AAA"); !hit {
		t.Fatal("failed SetPatterns dropped the old list")
	}
	if err := g.SetPatterns([]string{`BBB`}); err != nil {
		t.Fatal(err)
	}
	if _, hit := g.Match("AAA"); hit {
		t.Error("old pattern still active")
	}
	if _, hit := g.Match("xBBBx"); !hit {
		t.Error("new pattern not active")
	}
}

func newFilter(t *testing.T) (*lineFilter, *Gate) {
	g := newTestGate(t, []string{`DANGER`})
	return &lineFilter{gate: g, machineID: "a"}, g
}

func TestLineFilterPassesSafeLines(t *testing.T) {
	f, _ := newFilter(t)
	in := []byte("echo hi\r")
	fwd, ref := f.feed(in, "")
	if len(ref) != 0 || string(fwd) != string(in) {
		t.Fatalf("fwd=%q refusals=%v, want pass-through", fwd, ref)
	}
}

func TestLineFilterRefusesDangerousLine(t *testing.T) {
	f, _ := newFilter(t)
	fwd, ref := f.feed([]byte("echo DANGER\r"), "")
	if len(ref) != 1 || ref[0].Line != "echo DANGER" {
		t.Fatalf("refusals = %+v", ref)
	}
	// The Enter is withheld and replaced by Ctrl-U (discard the line).
	if string(fwd) != "echo DANGER\x15" {
		t.Errorf("forwarded %q, want the line then Ctrl-U and no Enter", fwd)
	}
	// "ok" is not a token.
	_, ref = f.feed([]byte("echo DANGER\r"), "ok")
	if len(ref) != 1 {
		t.Errorf("browser ok bypassed the filter: %+v", ref)
	}
}

func TestLineFilterSplitAcrossMessages(t *testing.T) {
	f, _ := newFilter(t)
	var refused int
	for _, part := range []string{"echo ", "DAN", "GER", "\r"} {
		_, ref := f.feed([]byte(part), "")
		refused += len(ref)
	}
	if refused != 1 {
		t.Errorf("refusals = %d, want 1 (line typed in pieces)", refused)
	}
}

func TestLineFilterBackspaceAndKillKeys(t *testing.T) {
	f, _ := newFilter(t)
	// Typing it then erasing the dangerous part is fine.
	_, ref := f.feed([]byte("echo DANGER\x7f\x7f\x7f\x7f\x7f\x7fok\r"), "")
	if len(ref) != 0 {
		t.Errorf("backspaced line refused: %+v", ref)
	}
	// Ctrl-U clears the tracked line.
	_, ref = f.feed([]byte("echo DANGER\x15echo fine\r"), "")
	if len(ref) != 0 {
		t.Errorf("Ctrl-U line refused: %+v", ref)
	}
	// Ctrl-W drops the last word only.
	_, ref = f.feed([]byte("echo x DANGER\x17\r"), "")
	if len(ref) != 0 {
		t.Errorf("Ctrl-W line refused: %+v", ref)
	}
}

func TestLineFilterRunsWithToken(t *testing.T) {
	f, g := newFilter(t)
	tok := g.Mint("a", "echo DANGER", time.Minute)
	fwd, ref := f.feed([]byte("echo DANGER\r"), tok)
	if len(ref) != 0 || string(fwd) != "echo DANGER\r" {
		t.Fatalf("fwd=%q refusals=%+v, want it to run", fwd, ref)
	}
	// Single use: the same token does not authorise a second line.
	_, ref = f.feed([]byte("echo DANGER\r"), tok)
	if len(ref) != 1 {
		t.Errorf("replayed token let a line through")
	}
}

func TestLineFilterGrants(t *testing.T) {
	f, g := newFilter(t)
	f.grant(g.Mint("a", "echo DANGER", time.Minute))
	if _, ref := f.feed([]byte("echo DANGER\r"), ""); len(ref) != 0 {
		t.Fatalf("granted line refused: %+v", ref)
	}
	if _, ref := f.feed([]byte("echo DANGER\r"), ""); len(ref) != 1 {
		t.Fatal("grant was not single-use")
	}
	f.grant(g.Mint("a", "echo DANGER other", time.Minute)) // wrong line
	if _, ref := f.feed([]byte("echo DANGER\r"), ""); len(ref) != 1 {
		t.Fatal("grant for another line authorised this one")
	}
}

func TestLineFilterOpaqueLines(t *testing.T) {
	// History/cursor keys make the line untrackable: refused when strict.
	f, _ := newFilter(t)
	_, ref := f.feed([]byte("\x1b[A\r"), "") // Up, Enter: would run a recalled command
	if len(ref) != 1 {
		t.Fatalf("history recall not refused in strict mode: %+v", ref)
	}
	// The next clean line is fine again.
	if _, ref := f.feed([]byte("echo ok\r"), ""); len(ref) != 0 {
		t.Errorf("clean line after a refusal was refused: %+v", ref)
	}
	// Tab completion does not taint.
	if _, ref := f.feed([]byte("ls /et\t\r"), ""); len(ref) != 0 {
		t.Errorf("tab-completed line refused: %+v", ref)
	}
	// Relaxed mode tracks only what it saw.
	f2, _ := newFilter(t)
	f2.allowOpaque = true
	if _, ref := f2.feed([]byte("\x1b[A\r"), ""); len(ref) != 0 {
		t.Errorf("relaxed mode refused history line: %+v", ref)
	}
	if _, ref := f2.feed([]byte("\x1b[Aecho DANGER\r"), ""); len(ref) != 1 {
		t.Errorf("relaxed mode missed a visible dangerous line: %+v", ref)
	}
}

func TestLineFilterBracketedPaste(t *testing.T) {
	f, _ := newFilter(t)
	fwd, ref := f.feed([]byte("\x1b[200~echo DANGER\x1b[201~\r"), "")
	if len(ref) != 1 {
		t.Fatalf("pasted dangerous line not refused: fwd=%q ref=%+v", fwd, ref)
	}
	if _, ref := f.feed([]byte("\x1b[200~echo fine\x1b[201~\r"), ""); len(ref) != 0 {
		t.Errorf("pasted safe line refused: %+v", ref)
	}
}
