package panel

import (
	"strings"
	"testing"
)

// The Machines SPA route serves the React shell and bundle.
func TestMachinesPageHasTokenEnrollmentUI(t *testing.T) {
	srv := noBackend(t)
	status, _, page := body(t, srv.URL+"/machines")
	if status != 200 {
		t.Fatalf("GET /machines = %d", status)
	}
	for _, want := range []string{`<div id="root"></div>`, `/assets/dist/app.css`, `/assets/dist/app.js`} {
		if !strings.Contains(page, want) {
			t.Errorf("machines SPA shell lacks %q", want)
		}
	}

	status, _, js := body(t, srv.URL+"/assets/dist/app.js")
	if status != 200 || len(js) == 0 {
		t.Fatalf("GET /assets/dist/app.js = %d (len=%d)", status, len(js))
	}
}
