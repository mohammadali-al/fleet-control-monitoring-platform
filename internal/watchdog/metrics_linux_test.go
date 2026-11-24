//go:build linux

package watchdog

import "testing"

// Reading link statistics over rtnetlink needs no root. A sandbox without netlink
// or without any non-loopback interface skips rather than fails.
func TestLinkTxBytesLive(t *testing.T) {
	tx, err := linkTxBytes()
	if err != nil {
		t.Skipf("no usable netlink link statistics here: %v", err)
	}
	tx2, err := linkTxBytes()
	if err != nil {
		t.Fatal(err)
	}
	if tx2 < tx {
		t.Fatalf("tx byte counter went backwards: %d -> %d", tx, tx2)
	}
	t.Logf("non-loopback tx bytes: %d", tx2)
}

func TestDiskPercentLive(t *testing.T) {
	pct, err := diskPercent(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if pct < 0 || pct > 100 {
		t.Fatalf("disk percent out of range: %v", pct)
	}
	if _, err := diskPercent("/definitely/not/here"); err == nil {
		t.Fatal("missing path must fail")
	}
}
