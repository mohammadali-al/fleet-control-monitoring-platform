package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestOSServiceControllerCommands(t *testing.T) {
	type call struct {
		name string
		args []string
	}
	var calls []call
	newCtrl := func(goos string, fn cmdRunner) *osServiceController {
		calls = nil
		return &osServiceController{
			goos:         goos,
			systemdUnit:  defaultSystemdUnit,
			launchdLabel: defaultLaunchdLabel,
			launchdPlist: defaultLaunchdPlist,
			run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				calls = append(calls, call{name, append([]string(nil), args...)})
				if fn != nil {
					return fn(ctx, name, args...)
				}
				return nil, nil
			},
		}
	}

	ctx := context.Background()
	lin := newCtrl("linux", func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "is-active" {
			return []byte("active\n"), nil
		}
		return nil, nil
	})
	if err := lin.Stop(ctx); err != nil {
		t.Fatalf("linux Stop: %v", err)
	}
	if err := lin.Start(ctx); err != nil {
		t.Fatalf("linux Start: %v", err)
	}
	st, err := lin.Status(ctx)
	if err != nil || st != "running" {
		t.Fatalf("linux Status = %q, %v; want running, nil", st, err)
	}
	wantLin := []string{
		"systemctl disable --now --no-block fleet-agent.service",
		"systemctl enable --now fleet-agent.service",
		"systemctl is-active fleet-agent.service",
	}
	if len(calls) != len(wantLin) {
		t.Fatalf("linux calls = %d, want %d: %+v", len(calls), len(wantLin), calls)
	}
	for i, w := range wantLin {
		got := calls[i].name + " " + strings.Join(calls[i].args, " ")
		if got != w {
			t.Errorf("linux call %d = %q, want %q", i, got, w)
		}
	}

	mac := newCtrl("darwin", nil)
	if err := mac.Stop(ctx); err != nil {
		t.Fatalf("darwin Stop: %v", err)
	}
	if err := mac.Start(ctx); err != nil {
		t.Fatalf("darwin Start: %v", err)
	}
	st, err = mac.Status(ctx)
	if err != nil || st != "running" {
		t.Fatalf("darwin Status = %q, %v; want running, nil", st, err)
	}
	wantMac := []string{
		"launchctl disable system/com.fleet.agent",
		"launchctl bootout system/com.fleet.agent",
		"launchctl enable system/com.fleet.agent",
		"launchctl bootstrap system /Library/LaunchDaemons/com.fleet.agent.plist",
		"launchctl print system/com.fleet.agent",
	}
	if len(calls) != len(wantMac) {
		t.Fatalf("darwin calls = %d, want %d: %+v", len(calls), len(wantMac), calls)
	}
	for i, w := range wantMac {
		got := calls[i].name + " " + strings.Join(calls[i].args, " ")
		if got != w {
			t.Errorf("darwin call %d = %q, want %q", i, got, w)
		}
	}

	failing := newCtrl("linux", func(context.Context, string, ...string) ([]byte, error) {
		return []byte("permission denied"), errors.New("exit status 1")
	})
	if err := failing.Stop(ctx); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("failing Stop err = %v, want output included", err)
	}

	linR := newCtrl("linux", nil)
	if err := linR.Restart(ctx); err != nil {
		t.Fatalf("linux Restart: %v", err)
	}
	if len(calls) != 1 || calls[0].name+" "+strings.Join(calls[0].args, " ") != "systemctl restart --no-block fleet-agent.service" {
		t.Errorf("linux Restart calls = %+v", calls)
	}

	macR := newCtrl("darwin", nil)
	if err := macR.Restart(ctx); err != nil {
		t.Fatalf("darwin Restart: %v", err)
	}
	if len(calls) != 1 || calls[0].name+" "+strings.Join(calls[0].args, " ") != "launchctl kickstart -k system/com.fleet.agent" {
		t.Errorf("darwin Restart calls = %+v", calls)
	}
}
