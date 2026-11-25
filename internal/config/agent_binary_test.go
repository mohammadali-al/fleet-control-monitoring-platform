package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeAgentPlatform(t *testing.T) {
	for _, tc := range []struct {
		rawOS, rawArch   string
		wantOS, wantArch string
	}{
		{"Linux", "x86_64", "linux", "amd64"},
		{"linux", "amd64", "linux", "amd64"},
		{"Linux", "aarch64", "linux", "arm64"},
		{"linux", "arm64", "linux", "arm64"},
		{"Darwin", "x86_64", "darwin", "amd64"},
		{"darwin", "amd64", "darwin", "amd64"},
		{"Darwin", "arm64", "darwin", "arm64"},
	} {
		goos, arch, err := NormalizeAgentPlatform(tc.rawOS, tc.rawArch)
		if err != nil || goos != tc.wantOS || arch != tc.wantArch {
			t.Errorf("NormalizeAgentPlatform(%q, %q) = (%q, %q, %v), want (%q, %q, nil)",
				tc.rawOS, tc.rawArch, goos, arch, err, tc.wantOS, tc.wantArch)
		}
	}

	for _, bad := range [][2]string{
		{"windows", "amd64"},
		{"freebsd", "amd64"},
		{"linux", "386"},
		{"linux", "armv7l"},
		{"", "amd64"},
		{"linux", ""},
	} {
		if _, _, err := NormalizeAgentPlatform(bad[0], bad[1]); err == nil {
			t.Errorf("NormalizeAgentPlatform(%q, %q) succeeded, want error", bad[0], bad[1])
		}
	}
}

func TestSelectAgentBinary(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64"} {
		if err := os.WriteFile(filepath.Join(dir, "fleet-agent-"+p), []byte(p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fallback := filepath.Join(t.TempDir(), "fleet-agent-fallback")
	if err := os.WriteFile(fallback, []byte("fallback"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Prefers agent_binary_dir over agent_binary_path even for the host platform.
	for _, tc := range []struct{ goos, arch string }{
		{"linux", "amd64"},
		{"linux", "arm64"},
		{"darwin", "amd64"},
		{"darwin", "arm64"},
	} {
		got, err := SelectAgentBinary(dir, fallback, tc.goos, tc.arch, "linux", "amd64")
		want := filepath.Join(dir, "fleet-agent-"+tc.goos+"-"+tc.arch)
		if err != nil || got != want {
			t.Errorf("SelectAgentBinary(%s/%s) = (%q, %v), want (%q, nil)", tc.goos, tc.arch, got, err, want)
		}
	}

	// Falls back to agent_binary_path only for the host's own OS/arch.
	partialDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(partialDir, "fleet-agent-darwin-arm64"), []byte("mac"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := SelectAgentBinary(partialDir, fallback, "linux", "amd64", "linux", "amd64")
	if err != nil || got != fallback {
		t.Errorf("host fallback = (%q, %v), want (%q, nil)", got, err, fallback)
	}

	// Missing target binary must NOT fall back to a mismatched host binary.
	_, err = SelectAgentBinary(partialDir, fallback, "linux", "arm64", "linux", "amd64")
	if !errors.Is(err, ErrAgentBinaryUnavailable) || !strings.Contains(err.Error(), "linux/arm64") {
		t.Errorf("missing linux/arm64 err = %v", err)
	}

	// Empty file in agent_binary_dir is ignored (not treated as valid).
	if err := os.WriteFile(filepath.Join(partialDir, "fleet-agent-darwin-amd64"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := SelectAgentBinary(partialDir, "", "darwin", "amd64", "linux", "amd64"); !errors.Is(err, ErrAgentBinaryUnavailable) {
		t.Errorf("empty binary in dir err = %v, want ErrAgentBinaryUnavailable", err)
	}

	// Neither configured: clear error.
	if _, err := SelectAgentBinary("", "", "linux", "amd64", "linux", "amd64"); !errors.Is(err, ErrAgentBinaryUnavailable) {
		t.Errorf("unconfigured err = %v", err)
	}
}
