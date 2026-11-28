package agent

import (
	"context"
	"log/slog"
	"runtime"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/version"
)

const versionSendTimeout = 10 * time.Second

func effectiveAgentVersion(override string) string {
	if override != "" {
		return override
	}
	return version.Version
}

func reportAgentVersion(ctx context.Context, send exec.Sender, log *slog.Logger, verOverride string) {
	if send == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p := channel.AgentVersionPayload{
		Version: effectiveAgentVersion(verOverride),
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
		At:      time.Now().UTC(),
	}
	env, err := channel.NewEnvelope(channel.TypeAgentVersion, "", p)
	if err != nil {
		if log != nil {
			log.Warn("build agent-version envelope", "component", "fleet-agent", "err", err)
		}
		return
	}
	sctx, cancel := context.WithTimeout(ctx, versionSendTimeout)
	defer cancel()
	if err := send.Send(sctx, env); err != nil && log != nil {
		log.Debug("send agent-version", "component", "fleet-agent", "err", err)
	}
}
