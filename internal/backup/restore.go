package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// RestoreOptions control an import from the backup server.
type RestoreOptions struct {
	// Dest is the base data dir to restore into. Empty means the Service's own
	// data dir.
	Dest string
	// Overwrite replaces a local file whose content differs from the backup.
	// Default false: such a file is kept and reported as a conflict, so an
	// import never destroys newer local data unless the operator asks.
	Overwrite bool
}

// RestoreResult reports an import.
type RestoreResult struct {
	Restored  []FileEntry // written (new, or replaced with Overwrite)
	Unchanged int         // already identical locally
	Conflicts []string    // local file differs and Overwrite was not set
	Errors    []FileError
	Bytes     int64
}

// OK reports whether every file was restored or already identical.
func (r *RestoreResult) OK() bool { return len(r.Errors) == 0 }

// Restore imports the set from the backup server's constant path, using the
// stored credential. Each file is verified against the backup server's own
// SHA-256 of it before it is moved into place.
func (s *Service) Restore(ctx context.Context, opts RestoreOptions) (*RestoreResult, error) {
	srv, ok := s.Server()
	if !ok {
		return nil, ErrNoServer
	}
	if !s.runMu.TryLock() {
		return nil, ErrBackupRunning
	}
	defer s.runMu.Unlock()
	rem, err := s.connect(ctx, srv)
	if err != nil {
		return nil, err
	}
	defer rem.Close()
	return s.restoreFrom(ctx, rem, opts)
}

// RestoreWithPassword imports from a backup server given explicitly, for a
// rebuilt main server that has lost its own state (the stored server record and
// credential live in config/, which is not part of the backup set). The
// password is used for this call only and zeroed. After a successful import
// the caller should AddServer again to re-register the backup server.
func (s *Service) RestoreWithPassword(ctx context.Context, srv Server, password []byte, opts RestoreOptions) (*RestoreResult, error) {
	defer zero(password)
	if err := srv.validate(); err != nil {
		return nil, err
	}
	if !s.runMu.TryLock() {
		return nil, ErrBackupRunning
	}
	defer s.runMu.Unlock()
	rem, _, err := s.dial(ctx, srv, Credential{Password: password})
	if err != nil {
		return nil, fmt.Errorf("backup: connect to %s: %w", srv.addr(), err)
	}
	defer rem.Close()
	return s.restoreFrom(ctx, rem, opts)
}

func (s *Service) restoreFrom(ctx context.Context, rem Remote, opts RestoreOptions) (*RestoreResult, error) {
	dest := opts.Dest
	if dest == "" {
		dest = s.dataDir
	}
	list, err := runOut(ctx, rem, "cd "+shQuote(RemoteDataDir)+" && find . -type f", nil)
	if err != nil {
		return nil, fmt.Errorf("backup: list %s on the backup server: %w", RemoteDataDir, err)
	}
	var files []string
	for _, line := range strings.Split(string(list), "\n") {
		line = strings.TrimPrefix(strings.TrimSpace(line), "./")
		if line == "" || strings.HasSuffix(line, partSuffix) {
			continue
		}
		files = append(files, line)
	}
	sort.Strings(files)

	res := &RestoreResult{}
	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if !inSet(rel) {
			res.Errors = append(res.Errors, FileError{Path: rel, Err: "not part of the backup set; skipped"})
			continue
		}
		entry, status, err := s.restoreFile(ctx, rem, dest, rel, opts.Overwrite)
		switch {
		case err != nil:
			res.Errors = append(res.Errors, FileError{Path: rel, Err: err.Error()})
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return res, err
			}
		case status == restoreSame:
			res.Unchanged++
		case status == restoreConflict:
			res.Conflicts = append(res.Conflicts, rel)
		default:
			res.Restored = append(res.Restored, entry)
			res.Bytes += entry.Size
		}
	}
	s.log.Info("backup: import finished", "restored", len(res.Restored), "unchanged", res.Unchanged,
		"conflicts", len(res.Conflicts), "failed", len(res.Errors))
	if len(res.Errors) > 0 {
		return res, fmt.Errorf("backup: %d file(s) failed to restore, first: %s: %s", len(res.Errors), res.Errors[0].Path, res.Errors[0].Err)
	}
	return res, nil
}

type restoreStatus int

const (
	restoreWritten restoreStatus = iota
	restoreSame
	restoreConflict
)

// inSet reports whether rel is a safe relative path inside one of the set dirs.
func inSet(rel string) bool {
	if rel == "" || strings.HasPrefix(rel, "/") || strings.ContainsRune(rel, 0) {
		return false
	}
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	top := rel
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		top = rel[:i]
	} else {
		return false // a file directly in the base dir is not part of the set
	}
	for _, d := range SetDirs() {
		if d == top {
			return true
		}
	}
	return false
}

// restoreFile downloads one file, verifies it and moves it into place.
func (s *Service) restoreFile(ctx context.Context, rem Remote, dest, rel string, overwrite bool) (FileEntry, restoreStatus, error) {
	src := shQuote(path.Join(RemoteDataDir, rel))
	sumOut, err := runOut(ctx, rem, "sha256sum "+src+" | cut -d' ' -f1", nil)
	if err != nil {
		return FileEntry{}, 0, err
	}
	want := strings.TrimSpace(string(sumOut))
	if len(want) != 64 {
		return FileEntry{}, 0, errors.New("backup server returned no checksum")
	}
	target := filepath.Join(dest, filepath.FromSlash(rel))
	if have, err := hashFile(target); err == nil {
		if have == want {
			return FileEntry{}, restoreSame, nil
		}
		if !overwrite {
			return FileEntry{}, restoreConflict, nil
		}
	}

	dir := filepath.Dir(target)
	dirMode := os.FileMode(0o750)
	fileMode := os.FileMode(0o640)
	if strings.HasPrefix(rel, "ca/") { // CA keys: owner only
		dirMode, fileMode = 0o700, 0o600
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return FileEntry{}, 0, err
	}
	tmp, err := os.CreateTemp(dir, ".restore-*.tmp")
	if err != nil {
		return FileEntry{}, 0, err
	}
	name := tmp.Name()
	cleanup := func() { tmp.Close(); os.Remove(name) } // our own temp only
	h := sha256.New()
	if err := rem.Exec(ctx, "cat "+src, nil, io.MultiWriter(tmp, h)); err != nil {
		cleanup()
		return FileEntry{}, 0, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		cleanup()
		return FileEntry{}, 0, fmt.Errorf("%w: received %s, backup server has %s", ErrChecksum, got[:12], want[:12])
	}
	size, err := tmp.Seek(0, io.SeekCurrent)
	if err != nil {
		cleanup()
		return FileEntry{}, 0, err
	}
	if err := tmp.Chmod(fileMode); err != nil {
		cleanup()
		return FileEntry{}, 0, err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return FileEntry{}, 0, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return FileEntry{}, 0, err
	}
	if err := os.Rename(name, target); err != nil {
		os.Remove(name)
		return FileEntry{}, 0, err
	}
	return FileEntry{Path: rel, Size: size, SHA256: want}, restoreWritten, nil
}
