package registry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
)

func TestInspectFileVersion(t *testing.T) {
	dir := t.TempDir()
	ver, exists, err := InspectFileVersion(dir)
	if err != nil || exists || ver != 0 {
		t.Fatalf("missing: (%d, %v, %v)", ver, exists, err)
	}

	st := NewStore(dir)
	if err := st.Save(New()); err != nil {
		t.Fatal(err)
	}
	ver, exists, err = InspectFileVersion(dir)
	if err != nil || !exists || ver != CurrentFormatVersion {
		t.Fatalf("saved: (%d, %v, %v)", ver, exists, err)
	}

	regPath := filepath.Join(dir, config.RegistryDirName, FileName)
	if err := os.WriteFile(regPath, []byte(`{"version": 9, "machines": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ver, exists, err = InspectFileVersion(dir)
	if err != nil || !exists || ver != 9 {
		t.Fatalf("v9: (%d, %v, %v)", ver, exists, err)
	}
}
