// Package maneuver stores reusable Maneuver definitions (a named timeline of
// placed baseline items targeting exactly one (ip, origin) across selected
// worker agents) and materializes them via internal/baseline substitution.
package maneuver

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/baseline"
)

// Pattern types. Only PatternAllInOne is executable while the Load Controller
// is deferred (D-0012); Linear and Spikes are kept in the schema and rejected.
const (
	PatternAllInOne = "all"
	PatternLinear   = "linear"
	PatternSpikes   = "spikes"
)

// Limits for validated Maneuvers.
const (
	MaxTotalSec = 7 * 24 * 3600
	MaxReplicas = 64
)

// Sentinel errors returned by validation, materialization, and store ops.
var (
	ErrNotFound        = errors.New("maneuver: not found")
	ErrAlreadyExists   = errors.New("maneuver: already exists")
	ErrInvalidManeuver = errors.New("maneuver: invalid maneuver")
	ErrPatternDeferred = errors.New("maneuver: pattern requires the Load Controller (deferred)")
)

// Target identifies the single (ip, origin) target of a Maneuver.
type Target struct {
	IP     string `json:"ip"`
	Origin string `json:"origin,omitempty"`
}

// PatternConfig describes the load pattern for a Maneuver.
type PatternConfig struct {
	Type             string  `json:"type"`
	PatternLengthSec int     `json:"pattern_length_sec,omitempty"`
	SpikeDurationSec int     `json:"spike_duration_sec,omitempty"`
	WindowLengthSec  int     `json:"window_length_sec,omitempty"`
	MaxCPUCores      float64 `json:"max_cpu_cores,omitempty"`
	MaxRAMGB         float64 `json:"max_ram_gb,omitempty"`
}

// TimelineItem is one placed baseline-item instance on a Maneuver timeline.
type TimelineItem struct {
	ID          string            `json:"id"`
	BaselineID  string            `json:"baseline_id"`
	Name        string            `json:"name,omitempty"`
	StartSec    int               `json:"start_sec"`
	DurationSec int               `json:"duration_sec"`
	Inputs      map[string]string `json:"inputs,omitempty"`
	Replicas    int               `json:"replicas,omitempty"`
}

// Maneuver is a persisted Maneuver plan.
type Maneuver struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Target      Target         `json:"target"`
	TotalSec    int            `json:"total_sec"`
	Pattern     PatternConfig  `json:"pattern"`
	Timeline    []TimelineItem `json:"timeline"`
	Agents      []string       `json:"agents"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	CreatedBy   string         `json:"created_by,omitempty"`
	UpdatedBy   string         `json:"updated_by,omitempty"`
}

// Spec holds the mutable fields used to create, update, or preview a Maneuver.
type Spec struct {
	ID          string         `json:"id,omitempty"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Target      Target         `json:"target"`
	TotalSec    int            `json:"total_sec"`
	Pattern     PatternConfig  `json:"pattern"`
	Timeline    []TimelineItem `json:"timeline"`
	Agents      []string       `json:"agents"`
}

// BaselineLookup looks up baseline items by ID. *baseline.Store satisfies it.
type BaselineLookup interface {
	Get(id string) (baseline.Item, bool)
}

// MaterializedItem is one timeline item rendered with target and placeholder inputs.
type MaterializedItem struct {
	ItemID       string            `json:"item_id"`
	BaselineID   string            `json:"baseline_id"`
	Name         string            `json:"name"`
	Command      string            `json:"command"`
	StartSec     int               `json:"start_sec"`
	DurationSec  int               `json:"duration_sec"`
	Replicas     int               `json:"replicas"`
	Placeholders []string          `json:"placeholders"`
	Resolved     map[string]string `json:"resolved"`
}

// MaterializedPlan is a fully resolved Maneuver ready for preview or push.
type MaterializedPlan struct {
	ManeuverID string             `json:"maneuver_id,omitempty"`
	Name       string             `json:"name"`
	Target     Target             `json:"target"`
	TotalSec   int                `json:"total_sec"`
	Pattern    PatternConfig      `json:"pattern"`
	Items      []MaterializedItem `json:"items"`
	Agents     []string           `json:"agents"`
}

// ItemResolutionError wraps a per-timeline-item materialization error.
type ItemResolutionError struct {
	Index      int
	ItemID     string
	BaselineID string
	Err        error
}

func (e *ItemResolutionError) Error() string {
	return fmt.Sprintf("timeline item %d (%s / %s): %v", e.Index+1, e.ItemID, e.BaselineID, e.Err)
}

func (e *ItemResolutionError) Unwrap() error { return e.Err }

// ValidateTarget enforces that a Maneuver has exactly one valid target IP and
// at most one valid HTTP(S) origin (no paths, queries, or multiple targets).
func ValidateTarget(t Target) (Target, error) {
	rawIP := strings.TrimSpace(t.IP)
	if rawIP == "" {
		return Target{}, fmt.Errorf("%w: target ip is required", ErrInvalidManeuver)
	}
	if strings.ContainsAny(rawIP, " ,\t\r\n") {
		return Target{}, fmt.Errorf("%w: target must be a single IP address", ErrInvalidManeuver)
	}
	ip := net.ParseIP(rawIP)
	if ip == nil {
		return Target{}, fmt.Errorf("%w: invalid target ip %q", ErrInvalidManeuver, rawIP)
	}

	rawOrigin := strings.TrimSpace(t.Origin)
	if rawOrigin == "" {
		return Target{IP: ip.String()}, nil
	}
	if strings.ContainsAny(rawOrigin, " ,\t\r\n") {
		return Target{}, fmt.Errorf("%w: target must have a single origin", ErrInvalidManeuver)
	}
	u, err := url.Parse(rawOrigin)
	if err != nil {
		return Target{}, fmt.Errorf("%w: invalid target origin %q: %v", ErrInvalidManeuver, rawOrigin, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return Target{}, fmt.Errorf("%w: target origin %q must use http or https", ErrInvalidManeuver, rawOrigin)
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return Target{}, fmt.Errorf("%w: target origin %q must be scheme://host[:port]", ErrInvalidManeuver, rawOrigin)
	}
	if u.Path != "" && u.Path != "/" {
		return Target{}, fmt.Errorf("%w: target origin %q must not include a path (set paths per timeline item)", ErrInvalidManeuver, rawOrigin)
	}
	return Target{IP: ip.String(), Origin: scheme + "://" + u.Host}, nil
}

// ValidatePattern validates p, defaulting empty type to PatternAllInOne and
// rejecting Linear/Spikes because the Load Controller is deferred (D-0012).
func ValidatePattern(p PatternConfig) (PatternConfig, error) {
	typ := strings.ToLower(strings.TrimSpace(p.Type))
	if typ == "" {
		typ = PatternAllInOne
	}
	switch typ {
	case PatternAllInOne:
	case PatternLinear, PatternSpikes:
		return PatternConfig{}, fmt.Errorf("%w: %q needs the Load Controller", ErrPatternDeferred, typ)
	default:
		return PatternConfig{}, fmt.Errorf("%w: unknown load pattern %q", ErrInvalidManeuver, p.Type)
	}
	if p.PatternLengthSec < 0 || p.SpikeDurationSec < 0 || p.WindowLengthSec < 0 {
		return PatternConfig{}, fmt.Errorf("%w: pattern durations must be >= 0", ErrInvalidManeuver)
	}
	for name, v := range map[string]float64{"max_cpu_cores": p.MaxCPUCores, "max_ram_gb": p.MaxRAMGB} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return PatternConfig{}, fmt.Errorf("%w: %s must be >= 0", ErrInvalidManeuver, name)
		}
	}
	out := p
	out.Type = typ
	return out, nil
}

// ValidateSpec validates and normalizes a Maneuver Spec.
func ValidateSpec(spec Spec, allowID bool) (Spec, error) {
	id := strings.TrimSpace(spec.ID)
	if allowID && id != "" && !isValidID(id) {
		return Spec{}, fmt.Errorf("%w: invalid id %q", ErrInvalidManeuver, id)
	}
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return Spec{}, fmt.Errorf("%w: name is required", ErrInvalidManeuver)
	}
	if len(name) > 120 {
		return Spec{}, fmt.Errorf("%w: name exceeds 120 characters", ErrInvalidManeuver)
	}
	if spec.TotalSec <= 0 || spec.TotalSec > MaxTotalSec {
		return Spec{}, fmt.Errorf("%w: total_sec must be between 1 and %d", ErrInvalidManeuver, MaxTotalSec)
	}
	target, err := ValidateTarget(spec.Target)
	if err != nil {
		return Spec{}, err
	}
	pattern, err := ValidatePattern(spec.Pattern)
	if err != nil {
		return Spec{}, err
	}

	seenItemIDs := make(map[string]bool, len(spec.Timeline))
	timeline := make([]TimelineItem, 0, len(spec.Timeline))
	for i, raw := range spec.Timeline {
		it, err := validateTimelineItem(raw, i, spec.TotalSec, seenItemIDs)
		if err != nil {
			return Spec{}, err
		}
		seenItemIDs[it.ID] = true
		timeline = append(timeline, it)
	}

	seenAgents := make(map[string]bool, len(spec.Agents))
	agents := make([]string, 0, len(spec.Agents))
	for _, raw := range spec.Agents {
		a := strings.TrimSpace(raw)
		if a == "" {
			return Spec{}, fmt.Errorf("%w: empty agent id in selection", ErrInvalidManeuver)
		}
		if seenAgents[a] {
			continue
		}
		seenAgents[a] = true
		agents = append(agents, a)
	}

	return Spec{
		ID:          id,
		Name:        name,
		Description: strings.TrimSpace(spec.Description),
		Target:      target,
		TotalSec:    spec.TotalSec,
		Pattern:     pattern,
		Timeline:    timeline,
		Agents:      agents,
	}, nil
}

func validateTimelineItem(raw TimelineItem, idx, totalSec int, seen map[string]bool) (TimelineItem, error) {
	baseID := strings.TrimSpace(raw.BaselineID)
	if baseID == "" {
		return TimelineItem{}, fmt.Errorf("%w: timeline item %d missing baseline_id", ErrInvalidManeuver, idx+1)
	}
	itemID := strings.TrimSpace(raw.ID)
	if itemID == "" {
		for n := idx + 1; ; n++ {
			cand := fmt.Sprintf("item-%d", n)
			if !seen[cand] {
				itemID = cand
				break
			}
		}
	} else {
		if !isValidID(itemID) {
			return TimelineItem{}, fmt.Errorf("%w: timeline item %d has invalid id %q", ErrInvalidManeuver, idx+1, itemID)
		}
		if seen[itemID] {
			return TimelineItem{}, fmt.Errorf("%w: duplicate timeline item id %q", ErrInvalidManeuver, itemID)
		}
	}
	if raw.StartSec < 0 {
		return TimelineItem{}, fmt.Errorf("%w: timeline item %q start_sec must be >= 0", ErrInvalidManeuver, itemID)
	}
	if raw.DurationSec <= 0 {
		return TimelineItem{}, fmt.Errorf("%w: timeline item %q duration_sec must be > 0", ErrInvalidManeuver, itemID)
	}
	if raw.StartSec+raw.DurationSec > totalSec {
		return TimelineItem{}, fmt.Errorf("%w: timeline item %q (%d+%ds) exceeds total_sec (%ds)",
			ErrInvalidManeuver, itemID, raw.StartSec, raw.DurationSec, totalSec)
	}
	replicas := raw.Replicas
	if replicas == 0 {
		replicas = 1
	}
	if replicas < 1 || replicas > MaxReplicas {
		return TimelineItem{}, fmt.Errorf("%w: timeline item %q replicas must be between 1 and %d", ErrInvalidManeuver, itemID, MaxReplicas)
	}

	var inputs map[string]string
	if len(raw.Inputs) > 0 {
		inputs = make(map[string]string, len(raw.Inputs))
		for k, v := range raw.Inputs {
			key := strings.TrimSpace(k)
			if !baseline.IsPlaceholderName(key) {
				return TimelineItem{}, fmt.Errorf("%w: timeline item %q has invalid input key %q", ErrInvalidManeuver, itemID, k)
			}
			if strings.IndexByte(v, 0) >= 0 {
				return TimelineItem{}, fmt.Errorf("%w: timeline item %q input %q contains NUL byte", ErrInvalidManeuver, itemID, key)
			}
			if v != "" {
				inputs[key] = v
			}
		}
		if len(inputs) == 0 {
			inputs = nil
		}
	}

	return TimelineItem{
		ID:          itemID,
		BaselineID:  baseID,
		Name:        strings.TrimSpace(raw.Name),
		StartSec:    raw.StartSec,
		DurationSec: raw.DurationSec,
		Inputs:      inputs,
		Replicas:    replicas,
	}, nil
}

// Materialize validates spec and resolves every placed timeline item against
// lookup using internal/baseline's substitution engine. The Maneuver's single
// (ip, origin) target is bound into {ip}, {origin}, and fallback {url}.
func Materialize(spec Spec, lookup BaselineLookup) (MaterializedPlan, error) {
	norm, err := ValidateSpec(spec, true)
	if err != nil {
		return MaterializedPlan{}, err
	}
	if lookup == nil {
		return MaterializedPlan{}, fmt.Errorf("%w: baseline library lookup is required", ErrInvalidManeuver)
	}

	items := make([]MaterializedItem, 0, len(norm.Timeline))
	for i, ti := range norm.Timeline {
		bItem, ok := lookup.Get(ti.BaselineID)
		if !ok {
			return MaterializedPlan{}, &ItemResolutionError{
				Index: i, ItemID: ti.ID, BaselineID: ti.BaselineID,
				Err: fmt.Errorf("%w: %s", baseline.ErrNotFound, ti.BaselineID),
			}
		}
		inputs, err := mergedInputs(norm.Target, ti, bItem)
		if err != nil {
			return MaterializedPlan{}, &ItemResolutionError{
				Index: i, ItemID: ti.ID, BaselineID: ti.BaselineID, Err: err,
			}
		}
		rendered, err := baseline.RenderItem(bItem, inputs)
		if err != nil {
			return MaterializedPlan{}, &ItemResolutionError{
				Index: i, ItemID: ti.ID, BaselineID: ti.BaselineID, Err: err,
			}
		}
		name := ti.Name
		if name == "" {
			name = bItem.Name
		}
		items = append(items, MaterializedItem{
			ItemID:       ti.ID,
			BaselineID:   ti.BaselineID,
			Name:         name,
			Command:      rendered.Command,
			StartSec:     ti.StartSec,
			DurationSec:  ti.DurationSec,
			Replicas:     ti.Replicas,
			Placeholders: rendered.Placeholders,
			Resolved:     rendered.Resolved,
		})
	}

	return MaterializedPlan{
		ManeuverID: norm.ID,
		Name:       norm.Name,
		Target:     norm.Target,
		TotalSec:   norm.TotalSec,
		Pattern:    norm.Pattern,
		Items:      items,
		Agents:     norm.Agents,
	}, nil
}

func mergedInputs(target Target, ti TimelineItem, bItem baseline.Item) (map[string]string, error) {
	inputs := make(map[string]string, len(ti.Inputs)+4)
	for k, v := range ti.Inputs {
		inputs[k] = v
	}
	if v, ok := inputs["ip"]; ok && v != target.IP {
		return nil, fmt.Errorf("%w: timeline item %q cannot override target ip (%q != %q)", ErrInvalidManeuver, ti.ID, v, target.IP)
	}
	inputs["ip"] = target.IP

	if target.Origin != "" {
		if v, ok := inputs["origin"]; ok && v != target.Origin {
			return nil, fmt.Errorf("%w: timeline item %q cannot override target origin (%q != %q)", ErrInvalidManeuver, ti.ID, v, target.Origin)
		}
		inputs["origin"] = target.Origin

		if inputs["url"] == "" && bItem.Defaults["url"] == "" {
			if p := strings.TrimSpace(inputs["path"]); p != "" {
				if !strings.HasPrefix(p, "/") {
					p = "/" + p
				}
				inputs["url"] = target.Origin + p
			} else {
				inputs["url"] = target.Origin
			}
		}
	}

	if inputs["duration"] == "" && bItem.Defaults["duration"] == "" {
		inputs["duration"] = strconv.Itoa(ti.DurationSec)
	}
	return inputs, nil
}
