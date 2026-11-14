//go:build unix

// Package childproc records the long-lived child processes a test harness
// starts (cyoda-go nodes, compute test clients) and kills the ones that a test
// binary which died before its cleanup left running.
//
// The harness starts each child in its own process group, so the child does
// not die with the test binary: a panic, a timeout kill, SIGKILL or a killed
// `go test` leaves it running with PID 1 as its parent, holding CPU and ports.
// macOS has no parent-death signal, so the OS cannot end it. Instead every
// child is recorded in a per-user directory, and the next harness that starts
// a child first kills the orphans it finds there.
//
// # Identity: the record's lock
//
// Killing an unrelated process is the failure this package must make
// impossible, so a record is never trusted by its PID alone: a PID is reused.
// Each record is a file that carries a flock(2) lock, and the recorded process
// itself holds that lock. Start creates and locks the record file, passes it
// to the child as an extra file descriptor, and closes its own descriptor
// once the child runs. A flock lock belongs to the open file description,
// which the child shares through the inherited descriptor, so from then on
// the lock is held exactly as long as the child (or a descendant in its
// process group that inherited the descriptor) is alive, and the kernel drops
// it when the last of them exits. "The record's lock is held" therefore means
// "the recorded process group is alive", with no PID-reuse window: a PID
// cannot be reused while the process, or a process group of that ID, exists.
// The check is repeated before every signal.
//
// This replaces matching the PID's executable path and start time through
// ps(1): ps reports start times to the second, and it is a set-uid program
// that a sandboxed session cannot run at all, so a ps-based reaper would
// silently do nothing where most test runs happen.
//
// # Ownership: the owner's lock
//
// Several test binaries start children at the same time (parity packages run
// in parallel, other worktrees of the checkout share the directory), and a
// live fixture's children hold their record locks too. A reaper must tell a
// running fixture's child from an orphan, so each record names its owner: a
// file the owning test binary creates and locks once, and keeps open (not
// inherited by its children) until it exits. A record is an orphan only when
// its owner's lock is free — the owning test binary is gone, however it died —
// and its own lock is held. A record whose owner is alive is never touched,
// whatever state it is in. Reapers take a directory-wide lock, so one reaper
// probing a lock is never mistaken by another for a live holder.
//
// Failures — an unusable directory, a record that cannot be read — never fail
// the caller: they are logged, the child is started unrecorded, and the reaper
// moves on. When in doubt the reaper does not signal.
package childproc

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// recordSuffix names a child's record file; ownerSuffix an owning test
	// binary's lock file. pendingPrefix marks a file still being created: it
	// is renamed to its final name once it is locked, so a file under a final
	// name is locked from the moment it is visible.
	recordSuffix  = ".proc"
	ownerSuffix   = ".owner"
	pendingPrefix = ".pending-"
	reaperLock    = "reaper.lock"

	// termGrace is how long an orphan has to exit after SIGTERM before it is
	// sent SIGKILL; killGrace how long the SIGKILL is given to take effect.
	termGrace = 5 * time.Second
	killGrace = 5 * time.Second
	pollEvery = 50 * time.Millisecond
	// stalePending is the age after which an unlocked pending file is left
	// over from a creator that died before it could lock it.
	stalePending = time.Minute
)

// Registry records the children one test binary starts in one directory. Its
// zero value is not usable; use New or Default.
type Registry struct {
	dir  string
	logf func(format string, args ...any)

	ownerOnce sync.Once
	owner     *os.File // locked and kept open for the life of the process; nil when unusable
	ownerName string

	reapOnce sync.Once
}

// New returns a registry over dir that reports through logf.
func New(dir string, logf func(format string, args ...any)) *Registry {
	return &Registry{dir: dir, logf: logf}
}

var (
	defaultOnce     sync.Once
	defaultRegistry *Registry
)

// Default returns the process-wide registry over DefaultDir, logging through
// slog.
func Default() *Registry {
	defaultOnce.Do(func() {
		defaultRegistry = New(DefaultDir(), func(format string, args ...any) {
			slog.Info(fmt.Sprintf(format, args...), "pkg", "childproc")
		})
	})
	return defaultRegistry
}

// DefaultDir is the per-user directory every checkout and worktree records its
// children in. os.TempDir is $TMPDIR — a per-user directory on macOS that go
// test does not change — or /tmp, which other users share, hence the uid.
func DefaultDir() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("cyoda-test-procs-%d", os.Getuid()))
}

// Start reaps the orphans in the registry's directory (once per registry),
// then starts cmd and records it. The returned release removes the record; call
// it once the process has exited and been waited for. It is never nil and may
// be called more than once. The error is cmd.Start's: a failure to record is
// logged, and the process is started unrecorded.
func (r *Registry) Start(cmd *exec.Cmd) (release func(), err error) {
	r.reapOnce.Do(r.reapOrphans)
	noop := func() {}

	f, path := r.prepareRecord(cmd.Path)
	if f == nil {
		return noop, cmd.Start()
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, f)
	if err := cmd.Start(); err != nil {
		cmd.ExtraFiles = cmd.ExtraFiles[:len(cmd.ExtraFiles)-1]
		_ = f.Close()
		r.remove(path)
		return noop, err
	}
	// The new content is never shorter than the PID-less one it overwrites,
	// and one write either lands whole or not at all.
	rec := record{Owner: r.ownerName, PID: cmd.Process.Pid, Path: cmd.Path}
	if _, err := f.WriteAt(rec.encode(), 0); err != nil {
		r.logf("childproc: failed to record pid %d of %s: %v", rec.PID, cmd.Path, err)
	}
	// From here on the child alone holds the record's lock.
	_ = f.Close()

	var once sync.Once
	return func() { once.Do(func() { r.remove(path) }) }, nil
}

// prepareRecord creates a locked record file for a child about to be started
// from path. It returns nil when the directory cannot be used.
func (r *Registry) prepareRecord(exe string) (*os.File, string) {
	if !r.ensureOwner() {
		return nil, ""
	}
	f, final, err := r.createLocked(recordSuffix, record{Owner: r.ownerName, Path: exe}.encode())
	if err != nil {
		r.logf("childproc: cannot record %s, starting it unrecorded: %v", exe, err)
		return nil, ""
	}
	return f, final
}

// ensureOwner creates and locks this process's owner file once.
func (r *Registry) ensureOwner() bool {
	r.ownerOnce.Do(func() {
		if err := os.MkdirAll(r.dir, 0o700); err != nil {
			r.logf("childproc: cannot use %s, children are not recorded: %v", r.dir, err)
			return
		}
		f, final, err := r.createLocked(ownerSuffix, fmt.Appendf(nil, "%d\n", os.Getpid()))
		if err != nil {
			r.logf("childproc: cannot create an owner file in %s, children are not recorded: %v", r.dir, err)
			return
		}
		r.owner = f
		r.ownerName = filepath.Base(final)
	})
	return r.owner != nil
}

// createLocked creates a file with content in the directory, locks it, and
// only then gives it its final name, ending in suffix.
func (r *Registry) createLocked(suffix string, content []byte) (*os.File, string, error) {
	f, err := os.CreateTemp(r.dir, pendingPrefix+"*")
	if err != nil {
		return nil, "", fmt.Errorf("failed to create file: %w", err)
	}
	fail := func(err error) (*os.File, string, error) {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, "", err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail(fmt.Errorf("failed to lock %s: %w", f.Name(), err))
	}
	if _, err := f.Write(content); err != nil {
		return fail(fmt.Errorf("failed to write %s: %w", f.Name(), err))
	}
	final := filepath.Join(r.dir, strings.TrimPrefix(filepath.Base(f.Name()), pendingPrefix)+suffix)
	if err := os.Rename(f.Name(), final); err != nil {
		return fail(fmt.Errorf("failed to name %s: %w", final, err))
	}
	return f, final, nil
}

func (r *Registry) remove(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		r.logf("childproc: failed to remove %s: %v", path, err)
	}
}

// reapOrphans kills the orphans recorded in the directory and removes the
// records of processes that are gone. It never fails; problems are logged.
func (r *Registry) reapOrphans() {
	lock, err := os.OpenFile(filepath.Join(r.dir, reaperLock), os.O_CREATE|os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return // no directory, nothing recorded
	}
	if err != nil {
		r.logf("childproc: cannot reap orphans in %s: %v", r.dir, err)
		return
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		r.logf("childproc: cannot reap orphans in %s: failed to lock: %v", r.dir, err)
		return
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	entries, err := os.ReadDir(r.dir)
	if err != nil {
		r.logf("childproc: cannot reap orphans in %s: %v", r.dir, err)
		return
	}
	// Records first, owners after: a record's owner is read before the
	// owner's file can be cleared away.
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), recordSuffix) && !strings.HasPrefix(e.Name(), pendingPrefix) {
			r.reapRecord(filepath.Join(r.dir, e.Name()))
		}
	}
	for _, e := range entries {
		path := filepath.Join(r.dir, e.Name())
		switch {
		case strings.HasPrefix(e.Name(), pendingPrefix):
			info, err := e.Info()
			if err == nil && time.Since(info.ModTime()) > stalePending && r.lockFree(path) { // realtime: age of a leftover file
				r.remove(path)
			}
		case strings.HasSuffix(e.Name(), ownerSuffix) && e.Name() != r.ownerName:
			if r.lockFree(path) {
				r.remove(path)
			}
		}
	}
}

// reapRecord handles one record: untouched while its owner lives; removed
// when its process is gone; its process group killed when it is an orphan.
func (r *Registry) reapRecord(path string) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		r.logf("childproc: cannot read %s: %v", path, err)
		return
	}
	rec, decodeErr := decodeRecord(raw)
	if decodeErr == nil && r.ownerAlive(rec.Owner) {
		return
	}
	held, err := r.lockHeld(path)
	if err != nil {
		r.logf("childproc: cannot check %s: %v", path, err)
		return
	}
	if !held {
		// No process holds the record: the one it names has exited, whatever
		// now runs under its PID.
		r.remove(path)
		return
	}
	if decodeErr != nil {
		r.logf("childproc: %s is held but unreadable, not signalled: %v", path, decodeErr)
		return
	}
	if rec.PID <= 1 {
		r.logf("childproc: %s is held but names pid %d, not signalled", path, rec.PID)
		return
	}
	r.logf("childproc: killing orphan pid %d (%s) left by a test binary that has exited", rec.PID, rec.Path)
	for _, step := range []struct {
		sig   syscall.Signal
		grace time.Duration
	}{{syscall.SIGTERM, termGrace}, {syscall.SIGKILL, killGrace}} {
		if !r.signalIfHeld(path, rec, step.sig) {
			r.remove(path)
			return
		}
		if r.awaitRelease(path, step.grace) {
			r.remove(path)
			return
		}
	}
	r.logf("childproc: orphan pid %d (%s) survived SIGKILL; its record stays", rec.PID, rec.Path)
}

// signalIfHeld sends sig to the record's process group only while the record's
// lock is still held — its identity check, repeated before every signal. It
// reports whether the group was still alive.
func (r *Registry) signalIfHeld(path string, rec record, sig syscall.Signal) bool {
	held, err := r.lockHeld(path)
	if err != nil || !held {
		return false
	}
	// The child was started as the leader of its own group. A live process
	// with that PID in another group is not it; with no such process, the
	// group lives on in a descendant, and its ID cannot have been reused.
	if pgid, err := syscall.Getpgid(rec.PID); err == nil && pgid != rec.PID {
		r.logf("childproc: pid %d is not a process group leader, not signalled", rec.PID)
		return false
	}
	if err := syscall.Kill(-rec.PID, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		r.logf("childproc: failed to send %v to process group %d: %v", sig, rec.PID, err)
	}
	return true
}

// awaitRelease waits up to within for no process to hold the record's lock.
func (r *Registry) awaitRelease(path string, within time.Duration) bool {
	deadline := time.Now().Add(within) // realtime: grace period for a signalled process
	for {
		if r.lockFree(path) {
			return true
		}
		if time.Now().After(deadline) { // realtime: grace period for a signalled process
			return false
		}
		time.Sleep(pollEvery)
	}
}

// ownerAlive reports whether the owner file name is locked by a live test
// binary. A missing owner file is a dead owner; an owner that cannot be checked
// is treated as alive, so its children are not signalled.
func (r *Registry) ownerAlive(name string) bool {
	if name == "" || filepath.Base(name) != name || !strings.HasSuffix(name, ownerSuffix) {
		return false
	}
	held, err := r.lockHeld(filepath.Join(r.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		r.logf("childproc: cannot check owner %s: %v", name, err)
		return true
	}
	return held
}

// lockFree reports whether path exists and no process holds its lock.
func (r *Registry) lockFree(path string) bool {
	held, err := r.lockHeld(path)
	return err == nil && !held
}

// lockHeld reports whether another open file description holds path's lock.
func (r *Registry) lockHeld(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to probe the lock of %s: %w", path, err)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, nil
}

// record is the content of a record file. PID is zero until the child runs.
type record struct {
	Owner string `json:"owner"`
	Path  string `json:"path"`
	PID   int    `json:"pid"`
}

func (rec record) encode() []byte {
	b, err := json.Marshal(rec)
	if err != nil {
		panic(fmt.Sprintf("childproc: failed to encode a record: %v", err))
	}
	return b
}

func decodeRecord(raw []byte) (record, error) {
	var rec record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return record{}, fmt.Errorf("failed to decode record: %w", err)
	}
	return rec, nil
}

// Recorded reports whether the directory holds a record of pid, for a harness
// that checks it records what it starts.
func (r *Registry) Recorded(pid int) bool {
	paths, err := filepath.Glob(filepath.Join(r.dir, "*"+recordSuffix))
	if err != nil {
		return false
	}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if rec, err := decodeRecord(raw); err == nil && rec.PID == pid {
			return true
		}
	}
	return false
}
