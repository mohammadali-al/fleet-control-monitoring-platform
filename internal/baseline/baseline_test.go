package baseline

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseAndValidateTemplate(t *testing.T) {
	t.Run("extracts unique placeholders in order", func(t *testing.T) {
		got, err := ValidateTemplate("hey -z {duration}s -q {rate} -H 'X-Rate: {rate}' {url}")
		if err != nil {
			t.Fatalf("ValidateTemplate unexpected error: %v", err)
		}
		want := []string{"duration", "rate", "url"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("placeholders = %v, want %v", got, want)
		}
	})

	t.Run("supports escaped braces and no placeholders", func(t *testing.T) {
		got, err := ValidateTemplate(`echo '{{ok}}' && ping -c 5 {ip}`)
		if err != nil {
			t.Fatalf("ValidateTemplate unexpected error: %v", err)
		}
		if !reflect.DeepEqual(got, []string{"ip"}) {
			t.Fatalf("placeholders = %v, want [ip]", got)
		}

		got2, err := ValidateTemplate(`echo '{{literal}}'`)
		if err != nil {
			t.Fatalf("ValidateTemplate unexpected error: %v", err)
		}
		if len(got2) != 0 {
			t.Fatalf("placeholders = %v, want empty", got2)
		}
	})

	t.Run("rejects malformed syntax", func(t *testing.T) {
		bad := []string{
			"",
			"   ",
			"hey {rate",
			"hey rate}",
			"hey {}",
			"hey {123}",
			"hey {rate!}",
			"hey { rate }",
			"hey {a{b}}",
			"echo \x00 {ip}",
		}
		for _, tmpl := range bad {
			if _, err := ValidateTemplate(tmpl); !errors.Is(err, ErrInvalidTemplate) {
				t.Errorf("ValidateTemplate(%q) err = %v, want ErrInvalidTemplate", tmpl, err)
			}
		}
	})
}

func TestRenderSubstitution(t *testing.T) {
	t.Run("merges defaults and input overrides", func(t *testing.T) {
		res, err := Render(
			"hey -z {duration}s -q {rate} {url}",
			map[string]string{"duration": "60", "rate": "200", "url": "https://default.test"},
			map[string]string{"rate": "500", "url": "https://override.test"},
		)
		if err != nil {
			t.Fatalf("Render unexpected error: %v", err)
		}
		wantCmd := "hey -z 60s -q 500 https://override.test"
		if res.Command != wantCmd {
			t.Fatalf("Command = %q, want %q", res.Command, wantCmd)
		}
		if !reflect.DeepEqual(res.Placeholders, []string{"duration", "rate", "url"}) {
			t.Fatalf("Placeholders = %v", res.Placeholders)
		}
	})

	t.Run("single pass does not re-expand substituted values", func(t *testing.T) {
		res, err := Render("echo {a} {{literal}}", map[string]string{"a": "{b}"}, nil)
		if err != nil {
			t.Fatalf("Render unexpected error: %v", err)
		}
		if res.Command != "echo {b} {literal}" {
			t.Fatalf("Command = %q, want %q", res.Command, "echo {b} {literal}")
		}
	})

	t.Run("rejects unresolved placeholders", func(t *testing.T) {
		_, err := Render(
			"hey -z {duration}s -q {rate} {url}",
			map[string]string{"duration": "60", "url": ""},
			map[string]string{},
		)
		if !errors.Is(err, ErrUnresolvedPlaceholder) {
			t.Fatalf("err = %v, want ErrUnresolvedPlaceholder", err)
		}
		var u *UnresolvedPlaceholdersError
		if !errors.As(err, &u) {
			t.Fatalf("expected *UnresolvedPlaceholdersError, got %T", err)
		}
		wantMissing := []string{"rate", "url"}
		if !reflect.DeepEqual(u.Missing, wantMissing) {
			t.Fatalf("Missing = %v, want %v", u.Missing, wantMissing)
		}
	})
}

func TestStoreCRUDPersistenceAndReload(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open empty: %v", err)
	}
	if len(st.List()) != 0 {
		t.Fatalf("expected empty list, got %d", len(st.List()))
	}

	now := time.Date(2026, 11, 28, 1, 0, 0, 0, time.UTC)
	st.now = func() time.Time { return now }

	created, err := st.Create(ItemSpec{
		Name:            "HTTP GET flood",
		Description:     "Sustained GET requests",
		CommandTemplate: "hey -z {duration}s -q {rate} {url}",
		Defaults:        map[string]string{"rate": "200", "duration": "60", "url": ""},
		WarmupSec:       3.5,
		Tags:            []string{"http", "http", "l7"},
		Metadata:        map[string]string{"tool": "hey"},
	}, "alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID != "http-get-flood" {
		t.Fatalf("ID = %q, want http-get-flood", created.ID)
	}
	if !reflect.DeepEqual(created.Placeholders, []string{"duration", "rate", "url"}) {
		t.Fatalf("Placeholders = %v", created.Placeholders)
	}
	if _, hasEmptyURL := created.Defaults["url"]; hasEmptyURL {
		t.Fatalf("empty default should be stripped, got %v", created.Defaults)
	}
	if created.CreatedBy != "alice" || created.UpdatedBy != "alice" {
		t.Fatalf("actors = (%q, %q)", created.CreatedBy, created.UpdatedBy)
	}

	// RenderItem fails when {url} is unresolved, succeeds when supplied.
	if _, err := RenderItem(created, nil); !errors.Is(err, ErrUnresolvedPlaceholder) {
		t.Fatalf("RenderItem without url err = %v, want ErrUnresolvedPlaceholder", err)
	}
	rendered, err := RenderItem(created, map[string]string{"url": "https://shop.example.test"})
	if err != nil {
		t.Fatalf("RenderItem: %v", err)
	}
	if rendered.Command != "hey -z 60s -q 200 https://shop.example.test" {
		t.Fatalf("rendered = %q", rendered.Command)
	}

	// Duplicate generates a distinct ID and copy name.
	now = now.Add(time.Minute)
	dup, err := st.Duplicate(created.ID, "", "bob")
	if err != nil {
		t.Fatalf("Duplicate: %v", err)
	}
	if dup.ID == created.ID || dup.Name != "HTTP GET flood (copy)" {
		t.Fatalf("dup = %+v", dup)
	}
	if dup.CreatedBy != "bob" {
		t.Fatalf("dup CreatedBy = %q, want bob", dup.CreatedBy)
	}

	// Update modifies fields and preserves CreatedAt/CreatedBy.
	now = now.Add(time.Minute)
	updated, err := st.Update(created.ID, ItemSpec{
		Name:            "HTTP GET flood v2",
		CommandTemplate: "hey -z {duration}s -q {rate} {url}",
		Defaults:        map[string]string{"rate": "400", "duration": "30"},
	}, "carol")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.CreatedBy != "alice" || updated.UpdatedBy != "carol" || !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("Update metadata mismatch: %+v", updated)
	}
	if updated.Defaults["rate"] != "400" {
		t.Fatalf("Update defaults = %v", updated.Defaults)
	}

	// Reload from disk into a brand new Store (simulating server restart).
	st2, err := Open(dir)
	if err != nil {
		t.Fatalf("Open reload: %v", err)
	}
	items := st2.List()
	if len(items) != 2 {
		t.Fatalf("reloaded len = %d, want 2", len(items))
	}
	got, ok := st2.Get(created.ID)
	if !ok || got.Name != "HTTP GET flood v2" || got.Defaults["rate"] != "400" {
		t.Fatalf("reloaded item mismatch: ok=%v item=%+v", ok, got)
	}

	// Delete removes item and persists across reload.
	if err := st2.Delete(dup.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	st3, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after delete: %v", err)
	}
	if len(st3.List()) != 1 {
		t.Fatalf("after delete len = %d, want 1", len(st3.List()))
	}
	if _, ok := st3.Get(dup.ID); ok {
		t.Fatal("deleted item still present after reload")
	}

	// File lives under <dataDir>/baseline/items.json.
	if _, err := os.Stat(filepath.Join(dir, "baseline", FileName)); err != nil {
		t.Fatalf("expected persisted file: %v", err)
	}
}

func TestStoreValidationAndErrors(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := st.Create(ItemSpec{Name: "", CommandTemplate: "ping {ip}"}, "op"); !errors.Is(err, ErrInvalidItem) {
		t.Errorf("empty name err = %v, want ErrInvalidItem", err)
	}
	if _, err := st.Create(ItemSpec{Name: "Bad tpl", CommandTemplate: "ping {ip"}, "op"); !errors.Is(err, ErrInvalidTemplate) {
		t.Errorf("bad template err = %v, want ErrInvalidTemplate", err)
	}
	if _, err := st.Create(ItemSpec{Name: "Bad def", CommandTemplate: "ping {ip}", Defaults: map[string]string{"bad key": "1"}}, "op"); !errors.Is(err, ErrInvalidItem) {
		t.Errorf("bad default key err = %v, want ErrInvalidItem", err)
	}
	if _, err := st.Create(ItemSpec{ID: "custom_1", Name: "Custom", CommandTemplate: "ping {ip}"}, "op"); err != nil {
		t.Fatalf("Create with custom ID: %v", err)
	}
	if _, err := st.Create(ItemSpec{ID: "custom_1", Name: "Custom Dup", CommandTemplate: "ping {ip}"}, "op"); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("duplicate ID err = %v, want ErrAlreadyExists", err)
	}
	if err := st.Delete("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete missing err = %v, want ErrNotFound", err)
	}

	// Corrupt JSON fails loudly on Open.
	if err := os.WriteFile(st.Path(), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("expected parse error on corrupt file, got %v", err)
	}
}
