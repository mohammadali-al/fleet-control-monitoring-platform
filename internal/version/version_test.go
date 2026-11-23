package version

import (
	"strings"
	"testing"
)

func TestStringNonEmptyAndIncludesVersion(t *testing.T) {
	old := Version
	defer func() { Version = old }()
	Version = "1.2.3"

	s := String()
	if s == "" {
		t.Fatal("String() is empty")
	}
	if !strings.Contains(s, "1.2.3") {
		t.Fatalf("String() = %q, want it to contain the version", s)
	}
}
