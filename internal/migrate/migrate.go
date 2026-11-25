// Package migrate manages versioned, idempotent on-disk migrations for the
// main server's persistent state (config.json, <FLEET_DATA_DIR>/SCHEMA_VERSION,
// and data-dir contents).
//
// Startup policy: fleet-server auto-migrates older on-disk state in place after
// backing up the data directory, initializes brand-new empty data directories at
// CurrentVersion without backup, and refuses to start if on-disk state is newer
// than the running binary. Migrations never touch or regenerate <data>/ca/.
package migrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

const (
	// SeedVersion is the baseline schema version representing the initial
	// pre-migration on-disk layout.
	SeedVersion = 1
	// CurrentVersion is the schema version expected by this binary.
	CurrentVersion = config.CurrentSchemaVersion
)

var (
	// ErrNewerSchema indicates on-disk state has a schema version newer than
	// the running binary supports.
	ErrNewerSchema = config.ErrNewerSchema
	// ErrInvalidSchemaVersion indicates a malformed or non-positive
	// SCHEMA_VERSION marker file.
	ErrInvalidSchemaVersion = errors.New("invalid schema version")
)

// Context carries the paths and version bounds for a single migration step.
type Context struct {
	DataDir     string
	ConfigPath  string
	FromVersion int
	ToVersion   int
}

// Step is one ordered, idempotent migration from FromVersion to ToVersion
// (FromVersion + 1). Steps must never modify or regenerate <DataDir>/ca/.
type Step struct {
	FromVersion int
	ToVersion   int
	Description string
	Apply       func(Context) error
}

// Options configures Run and EnsureCurrent.
type Options struct {
	DataDir       string
	ConfigPath    string
	TargetVersion int
	Steps         []Step
	Now           func() time.Time
}

// Result describes what Run or EnsureCurrent observed and performed.
type Result struct {
	FromVersion int
	ToVersion   int
	BackupDir   string
	Applied     []Step
	Migrated    bool
	Initialized bool
}

// DefaultSteps returns the ordered built-in migration steps from SeedVersion
// to CurrentVersion.
func DefaultSteps() []Step {
	return []Step{
		{
			FromVersion: 1,
			ToVersion:   2,
			Description: "establish v2 baseline directories and verify registry",
			Apply: func(ctx Context) error {
				for _, sub := range []string{config.ConfigDirName, config.RegistryDirName} {
					if err := os.MkdirAll(filepath.Join(ctx.DataDir, sub), 0o750); err != nil {
						return fmt.Errorf("create %s directory: %w", sub, err)
					}
				}
				if _, err := registry.NewStore(ctx.DataDir).Load(); err != nil {
					return fmt.Errorf("verify registry during v1->v2 migration: %w", err)
				}
				return nil
			},
		},
	}
}

// Run explicitly migrates the data directory (and config file, if present) up
// to TargetVersion (default CurrentVersion). Running when already at
// TargetVersion is a no-op with no backup created.
func Run(opts Options) (Result, error) {
	return run(opts, true)
}

// EnsureCurrent is the server startup guard:
//   - On-disk schema newer than TargetVersion: refuses with ErrNewerSchema.
//   - Brand-new empty data dir with unversioned config: stamps SCHEMA_VERSION
//     without creating a backup.
//   - On-disk schema older than TargetVersion: backs up the data dir and
//     auto-migrates to TargetVersion.
func EnsureCurrent(opts Options) (Result, error) {
	return run(opts, false)
}

func run(opts Options, explicit bool) (Result, error) {
	if opts.DataDir == "" {
		return Result{}, errors.New("migrate: data directory is required")
	}
	dataDir := filepath.Clean(opts.DataDir)
	target := opts.TargetVersion
	if target == 0 {
		target = CurrentVersion
	}
	if target < SeedVersion {
		return Result{}, fmt.Errorf("migrate: invalid target version %d", target)
	}
	steps := opts.Steps
	if steps == nil {
		steps = DefaultSteps()
	}
	orderedSteps, err := normalizeSteps(steps)
	if err != nil {
		return Result{}, err
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	state, err := inspectState(dataDir, opts.ConfigPath, target)
	if err != nil {
		return Result{}, err
	}

	// Brand-new empty data directory and no explicit older schema_version on config.
	if state.dirVer == 0 && state.cfgVer == 0 {
		if err := os.MkdirAll(dataDir, 0o750); err != nil {
			return Result{}, fmt.Errorf("migrate: create data dir %s: %w", dataDir, err)
		}
		if err := writeSchemaVersionFile(dataDir, target); err != nil {
			return Result{}, err
		}
		if explicit && state.cfgExists {
			if err := setConfigSchemaVersion(opts.ConfigPath, target); err != nil {
				return Result{}, err
			}
		}
		return Result{FromVersion: target, ToVersion: target, Initialized: true}, nil
	}

	fromVer := effectiveFromVersion(state)
	if fromVer == target {
		if explicit && state.cfgExists && state.cfgVer == 0 {
			if err := setConfigSchemaVersion(opts.ConfigPath, target); err != nil {
				return Result{}, err
			}
		}
		return Result{FromVersion: target, ToVersion: target}, nil
	}

	plan, err := buildPlan(orderedSteps, fromVer, target)
	if err != nil {
		return Result{}, err
	}

	var backupDir string
	if state.dirExists {
		backupDir, err = backupDataDir(dataDir, fromVer, nowFn().UTC())
		if err != nil {
			return Result{}, err
		}
	} else if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return Result{}, fmt.Errorf("migrate: create data dir %s: %w", dataDir, err)
	}

	applied := make([]Step, 0, len(plan))
	for _, s := range plan {
		ctx := Context{
			DataDir:     dataDir,
			ConfigPath:  opts.ConfigPath,
			FromVersion: s.FromVersion,
			ToVersion:   s.ToVersion,
		}
		if err := s.Apply(ctx); err != nil {
			if backupDir != "" {
				return Result{FromVersion: fromVer, ToVersion: s.FromVersion, BackupDir: backupDir, Applied: applied},
					fmt.Errorf("migrate step %d->%d (%s) failed (backup at %s): %w", s.FromVersion, s.ToVersion, s.Description, backupDir, err)
			}
			return Result{FromVersion: fromVer, ToVersion: s.FromVersion, Applied: applied},
				fmt.Errorf("migrate step %d->%d (%s) failed: %w", s.FromVersion, s.ToVersion, s.Description, err)
		}
		if err := writeSchemaVersionFile(dataDir, s.ToVersion); err != nil {
			return Result{FromVersion: fromVer, ToVersion: s.FromVersion, BackupDir: backupDir, Applied: applied}, err
		}
		applied = append(applied, s)
	}

	if err := writeSchemaVersionFile(dataDir, target); err != nil {
		return Result{FromVersion: fromVer, ToVersion: target, BackupDir: backupDir, Applied: applied}, err
	}

	if state.cfgExists && state.cfgVer != target {
		if err := setConfigSchemaVersion(opts.ConfigPath, target); err != nil {
			if !explicit && errors.Is(err, fs.ErrPermission) {
				return Result{}, fmt.Errorf("migrate: cannot update %s (%w); run 'fleet-server migrate --config %s'", opts.ConfigPath, err, opts.ConfigPath)
			}
			return Result{}, err
		}
	}

	return Result{
		FromVersion: fromVer,
		ToVersion:   target,
		BackupDir:   backupDir,
		Applied:     applied,
		Migrated:    true,
	}, nil
}

type diskState struct {
	dirExists bool
	dirVer    int
	cfgExists bool
	cfgVer    int
}

func inspectState(dataDir, cfgPath string, target int) (diskState, error) {
	var st diskState
	info, err := os.Stat(dataDir)
	switch {
	case err == nil:
		if !info.IsDir() {
			return diskState{}, fmt.Errorf("migrate: %s is not a directory", dataDir)
		}
		st.dirExists = true
	case errors.Is(err, fs.ErrNotExist):
		st.dirExists = false
	default:
		return diskState{}, fmt.Errorf("migrate: stat %s: %w", dataDir, err)
	}

	if st.dirExists {
		ver, hasMarker, err := readSchemaVersionFile(dataDir)
		if err != nil {
			return diskState{}, err
		}
		if hasMarker {
			st.dirVer = ver
		} else {
			entries, err := os.ReadDir(dataDir)
			if err != nil {
				return diskState{}, fmt.Errorf("migrate: read %s: %w", dataDir, err)
			}
			if len(entries) > 0 {
				// Pre-versioning populated data directory corresponds to SeedVersion (v1).
				st.dirVer = SeedVersion
			}
		}
	}

	if st.dirVer > target {
		return diskState{}, fmt.Errorf("%w: data directory %s has schema version %d (binary supports %d); upgrade fleet-server instead of downgrading",
			ErrNewerSchema, dataDir, st.dirVer, target)
	}

	cfgVer, cfgExists, err := config.ReadSchemaVersion(cfgPath)
	if err != nil {
		return diskState{}, err
	}
	st.cfgExists = cfgExists
	st.cfgVer = cfgVer
	if st.cfgVer > target {
		return diskState{}, fmt.Errorf("%w: config %s has schema_version %d (binary supports %d); upgrade fleet-server instead of downgrading",
			ErrNewerSchema, cfgPath, st.cfgVer, target)
	}

	if st.dirExists {
		regVer, regExists, err := registry.InspectFileVersion(dataDir)
		if err != nil {
			return diskState{}, err
		}
		if regExists && regVer > registry.CurrentFormatVersion {
			return diskState{}, fmt.Errorf("%w: registry in %s has version %d (binary supports %d); upgrade fleet-server instead of downgrading",
				ErrNewerSchema, dataDir, regVer, registry.CurrentFormatVersion)
		}
	}

	return st, nil
}

func effectiveFromVersion(st diskState) int {
	from := st.dirVer
	if from == 0 {
		from = st.cfgVer
	}
	if st.cfgVer > 0 && st.cfgVer < from {
		from = st.cfgVer
	}
	return from
}

func readSchemaVersionFile(dataDir string) (int, bool, error) {
	path := filepath.Join(dataDir, config.SchemaVersionFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, true, fmt.Errorf("migrate: read %s: %w", path, err)
	}
	raw := strings.TrimSpace(string(data))
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return 0, true, fmt.Errorf("%w: %s contains %q", ErrInvalidSchemaVersion, path, raw)
	}
	return v, true, nil
}

func writeSchemaVersionFile(dataDir string, ver int) error {
	path := filepath.Join(dataDir, config.SchemaVersionFile)
	return writeFileAtomic(path, []byte(fmt.Sprintf("%d\n", ver)), 0o640)
}

func setConfigSchemaVersion(cfgPath string, ver int) error {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("migrate: read config %s: %w", cfgPath, err)
	}
	info, err := os.Stat(cfgPath)
	if err != nil {
		return fmt.Errorf("migrate: stat config %s: %w", cfgPath, err)
	}
	var doc map[string]any
	if len(strings.TrimSpace(string(data))) == 0 {
		doc = map[string]any{}
	} else if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("migrate: parse config %s: %w", cfgPath, err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	doc["schema_version"] = ver
	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("migrate: encode config %s: %w", cfgPath, err)
	}
	encoded = append(encoded, '\n')
	if err := writeFileAtomic(cfgPath, encoded, info.Mode().Perm()); err != nil {
		return fmt.Errorf("migrate: update config %s: %w", cfgPath, err)
	}
	return nil
}

func normalizeSteps(steps []Step) (map[int]Step, error) {
	byFrom := make(map[int]Step, len(steps))
	for _, s := range steps {
		if s.FromVersion < SeedVersion {
			return nil, fmt.Errorf("migrate: invalid step from version %d", s.FromVersion)
		}
		if s.ToVersion != s.FromVersion+1 {
			return nil, fmt.Errorf("migrate: step %d->%d must increment by 1", s.FromVersion, s.ToVersion)
		}
		if s.Apply == nil {
			return nil, fmt.Errorf("migrate: step %d->%d has nil Apply", s.FromVersion, s.ToVersion)
		}
		if _, dup := byFrom[s.FromVersion]; dup {
			return nil, fmt.Errorf("migrate: duplicate step for version %d", s.FromVersion)
		}
		byFrom[s.FromVersion] = s
	}
	return byFrom, nil
}

func buildPlan(byFrom map[int]Step, fromVer, target int) ([]Step, error) {
	if fromVer > target {
		return nil, fmt.Errorf("migrate: from version %d exceeds target %d", fromVer, target)
	}
	plan := make([]Step, 0, target-fromVer)
	for v := fromVer; v < target; v++ {
		s, ok := byFrom[v]
		if !ok {
			return nil, fmt.Errorf("migrate: missing migration step %d->%d", v, v+1)
		}
		plan = append(plan, s)
	}
	sort.Slice(plan, func(i, j int) bool { return plan[i].FromVersion < plan[j].FromVersion })
	return plan, nil
}

func backupDataDir(dataDir string, fromVer int, now time.Time) (string, error) {
	ts := now.Format("20060102T150405Z")
	base := fmt.Sprintf("%s.bak-%d-%s", dataDir, fromVer, ts)
	dest := base
	for i := 1; ; i++ {
		_, err := os.Lstat(dest)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("migrate: check backup path %s: %w", dest, err)
		}
		dest = fmt.Sprintf("%s-%d", base, i)
	}
	if err := copyDir(dataDir, dest); err != nil {
		_ = os.RemoveAll(dest)
		return "", fmt.Errorf("migrate: backup %s to %s: %w", dataDir, dest, err)
	}
	return dest, nil
}

func copyDir(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, srcInfo.Mode().Perm()); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch mode := info.Mode(); {
		case mode.IsDir():
			return os.MkdirAll(target, mode.Perm())
		case mode&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case mode.IsRegular():
			return copyFile(path, target, mode.Perm())
		default:
			return nil
		}
	})
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
