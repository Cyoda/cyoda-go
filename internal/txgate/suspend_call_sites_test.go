package txgate

// suspend_call_sites_test.go — every txgate.Suspend site must defer its resume.
//
// Suspend hands back the only thing that can put the gate back. If the blocking
// callout it spans panics and resume never runs, the caller's own deferred gate
// release unlocks a mutex nobody holds: `sync: unlock of unlocked mutex` is a
// runtime fatal, uncatchable by recover, so the door's recovery interceptors and
// the health latch are all bypassed and the node dies. A site may ALSO call
// resume explicitly (to re-acquire before its next buffer write) — resume is
// sync.Once-guarded, so having both costs nothing. Only the defer is mandatory.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var suspendAssignPattern = regexp.MustCompile(`^\s*(\w+)\s*:?=\s*(?:txgate\.)?Suspend\(`)

func TestSuspend_EveryCallSiteDefersItsResume(t *testing.T) {
	root := moduleRoot(t)

	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Separate modules cannot import cyoda-go/internal, and vendored or
			// hidden trees are not ours to police.
			if name := info.Name(); name != "." && (strings.HasPrefix(name, ".") || name == "plugins" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			m := suspendAssignPattern.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			want := "defer " + m[1] + "()"
			if i+1 >= len(lines) || strings.TrimSpace(lines[i+1]) != want {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, rel+":"+strconv.Itoa(i+1)+": next line is not "+strconv.Quote(want))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module tree: %v", err)
	}

	if len(offenders) > 0 {
		t.Fatalf("a Suspend whose resume is not deferred leaves the gate released when the callout panics, "+
			"and the caller's deferred release then unlocks an unlocked mutex (runtime fatal):\n%s",
			strings.Join(offenders, "\n"))
	}
}

// moduleRoot walks up from the working directory to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := wd
	for {
		if _, statErr := os.Stat(filepath.Join(root, "go.mod")); statErr == nil {
			return root
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Skip("cannot locate the module root; test skipped")
			return ""
		}
		root = parent
	}
}
