package panel

import (
	"strings"
	"testing"
)

// The Machines page carries the token-enrollment UI (T-0026): the "Add agent
// (token)" action, the code + one-liner area with a "show script" link, and the
// table of enrolled agents with Stop and Detach, driven by live.js.
func TestMachinesPageHasTokenEnrollmentUI(t *testing.T) {
	srv := noBackend(t)
	status, _, page := body(t, srv.URL+"/machines.html")
	if status != 200 {
		t.Fatalf("GET /machines.html = %d", status)
	}
	for _, want := range []string{
		"Add agent (token)", `id="enroll"`, "data-enroll-form", "data-enroll-code",
		`data-enroll-cmd="linux"`, `data-enroll-cmd="macos"`, "data-enroll-script", "Show script",
		`id="enrolled"`, "Stop</strong> disables the service", "Detach</strong> revokes", `id="dlg-enroll-act"`,
		"macOS machines can only be monitor agents",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("machines page lacks %q", want)
		}
	}
	// The SSH onboarding form is still there.
	if !strings.Contains(page, `id="onboard-form"`) {
		t.Error("the SSH onboarding form is gone")
	}

	_, _, js := body(t, srv.URL+"/assets/js/live.js")
	for _, want := range []string{"initEnroll", "/api/enroll/tokens", "/api/enroll/machines", "data-enroll-act"} {
		if !strings.Contains(js, want) {
			t.Errorf("live.js lacks %q", want)
		}
	}
}
