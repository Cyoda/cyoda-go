//go:build unix

package childproc

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// sleeper is a throwaway long-lived child: sleep 300 in its own process group,
// as the fixtures start cyoda-go. Its one Wait runs on a goroutine; done is
// closed when it returns.
type sleeper struct {
	cmd     *exec.Cmd
	done    chan struct{}
	waitErr error // written once, before done is closed
}

func newSleepCmd() *exec.Cmd {
	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// startSleeper starts sleep 300 through r (recorded) or, with r nil, directly
// (unrecorded). The test's cleanup kills whatever is still running.
func startSleeper(t *testing.T, r *Registry) (*sleeper, func()) {
	t.Helper()
	s := &sleeper{cmd: newSleepCmd(), done: make(chan struct{})}
	release := func() {}
	if r != nil {
		rel, err := r.Start(s.cmd)
		if err != nil {
			t.Fatalf("start sleep: %v", err)
		}
		release = rel
	} else if err := s.cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	go func() {
		s.waitErr = s.cmd.Wait()
		close(s.done)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		<-s.done
	})
	return s, release
}

func (s *sleeper) running() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// awaitSignalled waits for the sleeper to exit and returns the signal that
// ended it, failing the test if it is still running after within.
func (s *sleeper) awaitSignalled(t *testing.T, within time.Duration) syscall.Signal {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(within):
		t.Fatalf("child %d still running %s after the reap", s.cmd.Process.Pid, within)
	}
	var exitErr *exec.ExitError
	if !errors.As(s.waitErr, &exitErr) {
		t.Fatalf("child exited without an exit error: %v", s.waitErr)
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		t.Fatalf("child was not ended by a signal: %v", s.waitErr)
	}
	return ws.Signal()
}

// privateDir returns a new directory only the user can use, as a registry
// requires; t.TempDir's is open to group and others.
func privateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// recordFiles lists the process records in dir.
func recordFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "*"+recordSuffix))
	if err != nil {
		t.Fatalf("glob records: %v", err)
	}
	return m
}

// abandon makes r's owner look dead to every other reader of the directory,
// as the death of its test binary would: the kernel drops the owner lock when
// its file closes.
func abandon(t *testing.T, r *Registry) {
	t.Helper()
	if r.owner == nil {
		t.Fatal("registry has no owner file")
	}
	if err := r.owner.Close(); err != nil {
		t.Fatalf("close owner file: %v", err)
	}
}

// writeRecord writes a record file into dir as a registry would, without any
// process holding its lock.
func writeRecord(t *testing.T, dir string, rec record) string {
	t.Helper()
	path := filepath.Join(dir, "manual"+recordSuffix)
	if err := os.WriteFile(path, rec.encode(), 0o600); err != nil {
		t.Fatalf("write record: %v", err)
	}
	return path
}

// An orphan — its owner is gone, the process it records still holds the
// record's lock — has its process group killed and its record removed.
func TestReap_OrphanOfDeadOwnerIsKilled(t *testing.T) {
	dir := privateDir(t)
	owner := New(dir, t.Logf)
	child, _ := startSleeper(t, owner)
	abandon(t, owner)
	if n := len(recordFiles(t, dir)); n != 1 {
		t.Fatalf("records before the reap = %d, want 1", n)
	}

	New(dir, t.Logf).reapOrphans()

	if sig := child.awaitSignalled(t, 10*time.Second); sig != syscall.SIGTERM && sig != syscall.SIGKILL {
		t.Fatalf("orphan ended by %v, want SIGTERM or SIGKILL", sig)
	}
	if recs := recordFiles(t, dir); len(recs) != 0 {
		t.Fatalf("records after the reap = %v, want none", recs)
	}
}

// The first Start of a registry reaps before it starts its own process.
func TestStart_ReapsOrphansFirst(t *testing.T) {
	dir := privateDir(t)
	owner := New(dir, t.Logf)
	orphan, _ := startSleeper(t, owner)
	abandon(t, owner)

	next := New(dir, t.Logf)
	fresh, _ := startSleeper(t, next)

	orphan.awaitSignalled(t, 10*time.Second)
	if !fresh.running() {
		t.Fatal("the new registry's own child is not running")
	}
	if n := len(recordFiles(t, dir)); n != 1 {
		t.Fatalf("records after the reap = %d, want 1 (the new child's)", n)
	}
}

// A record whose PID is alive but is not the recorded process — it does not
// hold the record's lock, as a reused PID would not — is never signalled; the
// record is removed.
func TestReap_LivePIDThatIsNotTheRecordedProcessIsNotSignalled(t *testing.T) {
	dir := privateDir(t)
	unrelated, _ := startSleeper(t, nil)
	writeRecord(t, dir, record{Owner: "owner-gone" + ownerSuffix, PID: unrelated.cmd.Process.Pid, Path: unrelated.cmd.Path})

	New(dir, t.Logf).reapOrphans()

	time.Sleep(200 * time.Millisecond)
	if !unrelated.running() {
		t.Fatalf("unrelated process was signalled: %v", unrelated.waitErr)
	}
	if recs := recordFiles(t, dir); len(recs) != 0 {
		t.Fatalf("records after the reap = %v, want none", recs)
	}
}

// A record whose process has exited is removed.
func TestReap_DeadProcessRecordIsRemoved(t *testing.T) {
	dir := privateDir(t)
	done := exec.Command("true")
	if err := done.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	writeRecord(t, dir, record{Owner: "owner-gone" + ownerSuffix, PID: done.Process.Pid, Path: done.Path})

	New(dir, t.Logf).reapOrphans()

	if recs := recordFiles(t, dir); len(recs) != 0 {
		t.Fatalf("records after the reap = %v, want none", recs)
	}
}

// A record whose owner is alive belongs to a running test binary: its process
// is not signalled and its record stays.
func TestReap_LiveOwnersChildIsUntouched(t *testing.T) {
	dir := privateDir(t)
	owner := New(dir, t.Logf)
	child, _ := startSleeper(t, owner)

	New(dir, t.Logf).reapOrphans()

	time.Sleep(200 * time.Millisecond)
	if !child.running() {
		t.Fatalf("live owner's child was signalled: %v", child.waitErr)
	}
	if n := len(recordFiles(t, dir)); n != 1 {
		t.Fatalf("records after the reap = %d, want 1", n)
	}
}

// A record naming no usable process group (a PID of 0 or 1 would signal every
// process the user owns) is never signalled, even when its lock is held.
func TestReap_InvalidPIDIsNeverSignalled(t *testing.T) {
	dir := privateDir(t)
	owner := New(dir, t.Logf)
	child, _ := startSleeper(t, owner)
	recs := recordFiles(t, dir)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	raw, err := os.ReadFile(recs[0])
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	rec, err := decodeRecord(raw)
	if err != nil {
		t.Fatalf("decode record: %v", err)
	}
	abandon(t, owner)
	for _, pid := range []int{0, 1, -1} {
		rec.PID = pid
		f, err := os.OpenFile(recs[0], os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			t.Fatalf("open record: %v", err)
		}
		if _, err := f.Write(rec.encode()); err != nil {
			t.Fatalf("rewrite record: %v", err)
		}
		f.Close()

		New(dir, t.Logf).reapOrphans()

		time.Sleep(100 * time.Millisecond)
		if !child.running() {
			t.Fatalf("pid %d: child was signalled: %v", pid, child.waitErr)
		}
	}
}

// A clean shutdown — the process has been reaped and release called — leaves
// no record behind.
func TestRelease_RemovesTheRecord(t *testing.T) {
	dir := privateDir(t)
	r := New(dir, t.Logf)
	child, release := startSleeper(t, r)
	if n := len(recordFiles(t, dir)); n != 1 {
		t.Fatalf("records while running = %d, want 1", n)
	}
	_ = syscall.Kill(-child.cmd.Process.Pid, syscall.SIGKILL)
	<-child.done
	release()
	release() // a second call is harmless

	if recs := recordFiles(t, dir); len(recs) != 0 {
		t.Fatalf("records after release = %v, want none", recs)
	}
}

// A registry whose directory cannot be used still starts the process; it only
// cannot record it.
func TestStart_UnusableDirectoryStillStarts(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var logged []string
	r := New(filepath.Join(file, "procs"), func(format string, args ...any) {
		logged = append(logged, format)
	})
	child, release := startSleeper(t, r)
	if !child.running() {
		t.Fatal("child not running")
	}
	release()
	if len(logged) == 0 {
		t.Fatal("the unusable directory was not logged")
	}
}

// startTermIgnorer starts, through r, a child that ignores SIGTERM: an
// ignored signal stays ignored across exec, so sleep ignores it.
func startTermIgnorer(t *testing.T, r *Registry) *sleeper {
	t.Helper()
	cmd := exec.Command("sh", "-c", `trap "" TERM; exec sleep 300`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	s := &sleeper{cmd: cmd, done: make(chan struct{})}
	if _, err := r.Start(cmd); err != nil {
		t.Fatalf("start: %v", err)
	}
	go func() {
		s.waitErr = cmd.Wait()
		close(s.done)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-s.done
	})
	return s
}

// Orphans that ignore SIGTERM are sent SIGKILL once the grace period ends.
// They share one grace period: a reap takes about one grace period however
// many orphans it finds.
func TestReap_OrphansIgnoringSIGTERMAreKilled(t *testing.T) {
	dir := privateDir(t)
	owner := New(dir, t.Logf)
	orphans := []*sleeper{startTermIgnorer(t, owner), startTermIgnorer(t, owner), startTermIgnorer(t, owner)}
	time.Sleep(200 * time.Millisecond) // let sh install the trap and exec sleep
	abandon(t, owner)

	began := time.Now()
	New(dir, t.Logf).reapOrphans()
	took := time.Since(began)

	for _, s := range orphans {
		if sig := s.awaitSignalled(t, 2*time.Second); sig != syscall.SIGKILL {
			t.Fatalf("orphan ended by %v, want SIGKILL", sig)
		}
	}
	if recs := recordFiles(t, dir); len(recs) != 0 {
		t.Fatalf("records after the reap = %v, want none", recs)
	}
	if limit := termGrace + 2*time.Second; took > limit {
		t.Fatalf("the reap took %s, want under %s: orphans were waited for one after another", took, limit)
	}
}

// Recorded names the processes that have a record, until it is released.
func TestRecorded(t *testing.T) {
	dir := privateDir(t)
	r := New(dir, t.Logf)
	child, release := startSleeper(t, r)
	pid := child.cmd.Process.Pid
	if !r.Recorded(pid) {
		t.Fatalf("pid %d not recorded while running", pid)
	}
	if r.Recorded(pid + 1) {
		t.Fatalf("pid %d recorded, never started", pid+1)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	<-child.done
	release()
	if r.Recorded(pid) {
		t.Fatalf("pid %d still recorded after release", pid)
	}
}

// plantOrphan leaves an orphan in dir — a recorded sleeper whose owner is gone
// — and returns it with its record's path.
func plantOrphan(t *testing.T, dir string) (*sleeper, string) {
	t.Helper()
	owner := New(dir, t.Logf)
	child, _ := startSleeper(t, owner)
	recs := recordFiles(t, dir)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	abandon(t, owner)
	return child, recs[0]
}

// assertUntouched fails unless the orphan was neither signalled nor had its
// record removed. A reap that signals an orphan returns only once its record's
// lock is free, so a held lock right after the reap shows it was not signalled.
func assertUntouched(t *testing.T, child *sleeper, record string) {
	t.Helper()
	held, err := New(filepath.Dir(record), t.Logf).lockHeld(record)
	if err != nil {
		t.Fatalf("record after the reap: %v", err)
	}
	if !held || !child.running() {
		t.Fatalf("the orphan was signalled: %v", child.waitErr)
	}
}

// A directory that other users can write to is not used: a record in it may
// have been planted, so nothing is reaped, and nothing is recorded there.
func TestStart_DirectoryOpenToOthersIsNotUsed(t *testing.T) {
	dir := privateDir(t)
	child, record := plantOrphan(t, dir)
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	var logged []string
	next := New(dir, func(format string, args ...any) { logged = append(logged, format) })

	fresh, _ := startSleeper(t, next)

	assertUntouched(t, child, record)
	if !fresh.running() {
		t.Fatal("the new registry's child is not running")
	}
	if next.Recorded(fresh.cmd.Process.Pid) {
		t.Fatal("the new child was recorded in a directory open to others")
	}
	if len(logged) == 0 {
		t.Fatal("the unusable directory was not logged")
	}
}

// A directory reached through a symbolic link is not used.
func TestReap_SymlinkedDirectoryIsNotUsed(t *testing.T) {
	real := privateDir(t)
	child, record := plantOrphan(t, real)
	link := filepath.Join(t.TempDir(), "procs")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	New(link, t.Logf).reapOrphans()

	assertUntouched(t, child, record)
}

// A directory owned by another user is not used.
func TestReap_DirectoryOfAnotherUserIsNotUsed(t *testing.T) {
	dir := privateDir(t)
	child, record := plantOrphan(t, dir)
	asAnotherUser(t)

	New(dir, t.Logf).reapOrphans()

	assertUntouched(t, child, record)
}

// asAnotherUser makes the files the test creates read as another user's, for
// the rest of the test.
func asAnotherUser(t *testing.T) {
	t.Helper()
	prev := expectedUID
	expectedUID = func() int { return prev() + 1 }
	t.Cleanup(func() { expectedUID = prev })
}

// A record file owned by another user is neither signalled nor removed.
func TestReap_RecordOfAnotherUserIsUntouched(t *testing.T) {
	dir := privateDir(t)
	child, record := plantOrphan(t, dir)
	asAnotherUser(t)

	if _, ok := New(dir, t.Logf).checkRecord(record); ok {
		t.Fatal("a record of another user was taken for an orphan")
	}

	assertUntouched(t, child, record)
}

// A record that is a symbolic link is neither followed nor removed.
func TestReap_SymlinkedRecordIsUntouched(t *testing.T) {
	elsewhere := privateDir(t)
	child, record := plantOrphan(t, elsewhere)
	dir := privateDir(t)
	link := filepath.Join(dir, "planted"+recordSuffix)
	if err := os.Symlink(record, link); err != nil {
		t.Fatal(err)
	}

	New(dir, t.Logf).reapOrphans()

	assertUntouched(t, child, record)
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("the symbolic link was removed: %v", err)
	}
}
