package platformclock

// time_reads_test.go — every time read in non-test code is either the injected
// clock or explicitly marked real time.
//
// Business time (entity timestamps, transition times, scheduled-fire due times,
// anything a test clock must be able to move) comes from the injected spi.Clock.
// A read of the wall clock or the database clock that measures real time
// (timeout, lease, heartbeat, auth validity, metrics) says so with a
// `realtime: <reason>` comment. Comments and strings do not count as reads: the
// scan is AST-based for Go and lexical for SQL.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const timeReadAdvice = "business time must come from the injected clock; if this call measures real time (timeout, lease, heartbeat, auth, metrics), mark it `// realtime: <reason>`"

var (
	goMarker  = regexp.MustCompile(`^//\s*realtime:\s*\S`)
	sqlMarker = regexp.MustCompile(`(--|//)\s*realtime:\s*\S`)
	sqlRead   = regexp.MustCompile(`(?i)(?:^|[^.\w])(now\s*\(\s*\)|current_timestamp|clock_timestamp\s*\(\s*\))`)
)

// markers records where `// realtime: <reason>` comments sit. A whole-line
// comment (nothing else on its line) covers a read on the next line; a trailing
// comment on a code line covers only reads on that same line.
type markers struct {
	whole    map[int]bool
	trailing map[int]bool
}

func (m markers) onLine(l int) bool { return m.whole[l] || m.trailing[l] }

// covers reports whether a read on line l is marked: a marker on l, or a
// whole-line marker directly above.
func (m markers) covers(l int) bool { return m.onLine(l) || m.whole[l-1] }

func findMarkers(fset *token.FileSet, file *ast.File, src []byte) markers {
	m := markers{whole: map[int]bool{}, trailing: map[int]bool{}}
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			if !goMarker.MatchString(c.Text) {
				continue
			}
			p := fset.Position(c.Pos())
			if strings.TrimSpace(string(lineStart(src, c.Pos(), fset))) == "" {
				m.whole[p.Line] = true
			} else {
				m.trailing[p.Line] = true
			}
		}
	}
	return m
}

// lineStart returns the source text on pos's line before pos.
func lineStart(src []byte, pos token.Pos, fset *token.FileSet) []byte {
	off := fset.Position(pos).Offset
	i := off
	for i > 0 && src[i-1] != '\n' {
		i--
	}
	return src[i:off]
}

// scanGoTimeReads reports every unmarked time.Now/Since/Until reference and,
// when sql is true, every unmarked now()/CURRENT_TIMESTAMP/clock_timestamp()
// in a string literal. src may be nil to read filename from disk.
func scanGoTimeReads(filename string, src any, sql bool) ([]string, error) {
	var raw []byte
	switch v := src.(type) {
	case nil:
		b, err := os.ReadFile(filename)
		if err != nil {
			return nil, err
		}
		raw = b
	case string:
		raw = []byte(v)
	case []byte:
		raw = v
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, raw, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	marked := findMarkers(fset, file, raw)
	line := func(p token.Pos) int { return fset.Position(p).Line }
	at := func(l int) string { return filename + ":" + strconv.Itoa(l) }

	var out []string
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		switch x := n.(type) {
		case *ast.SelectorExpr:
			id, ok := x.X.(*ast.Ident)
			if !ok || id.Name != "time" {
				break
			}
			switch x.Sel.Name {
			case "Now", "Since", "Until":
				l := line(x.Pos())
				if !marked.covers(l) {
					out = append(out, at(l)+": time."+x.Sel.Name+": "+timeReadAdvice)
				}
			}
		case *ast.BasicLit:
			if !sql || x.Kind != token.STRING {
				break
			}
			val := x.Value
			if !sqlRead.MatchString(val) {
				break
			}
			start := line(x.Pos())
			// Innermost enclosing statement: a marker on or above it covers
			// a literal that begins on a later line of the same statement.
			stmtStart := start
			for i := len(stack) - 2; i >= 0; i-- {
				if _, ok := stack[i].(ast.Stmt); ok {
					stmtStart = line(stack[i].Pos())
					break
				}
			}
			goLevel := marked.covers(start) || marked.covers(stmtStart)
			for i, l := range strings.Split(val, "\n") {
				if !sqlRead.MatchString(l) {
					continue
				}
				if goLevel || sqlMarker.MatchString(l) {
					continue
				}
				out = append(out, at(start+i)+": "+strings.Trim(sqlRead.FindString(l), " \t")+": "+timeReadAdvice)
			}
		}
		return true
	})
	return out, nil
}

var (
	funcStart = regexp.MustCompile(`(?i)^\s*CREATE\s+(OR\s+REPLACE\s+)?FUNCTION\s+(cyoda_stamp|cyoda_consistency_time)\b`)
	sqlLineRe = regexp.MustCompile(`--\s*realtime:\s*\S`)
)

// scanSQLTimeReads reports every unmarked SQL time read in a .sql file, outside
// the bodies of cyoda_stamp and cyoda_consistency_time (the documented
// production stamp source). Whole-line `--` comments carry no read.
func scanSQLTimeReads(filename, src string) []string {
	var out []string
	inStamp := false
	for i, l := range strings.Split(src, "\n") {
		if funcStart.MatchString(l) {
			inStamp = true
		}
		if inStamp {
			if strings.Contains(l, "END $$;") {
				inStamp = false
			}
			continue
		}
		code := l
		if idx := strings.Index(code, "--"); idx >= 0 {
			code = code[:idx]
		}
		if !sqlRead.MatchString(code) || sqlLineRe.MatchString(l) {
			continue
		}
		if i > 0 && sqlLineRe.MatchString(strings.Split(src, "\n")[i-1]) {
			continue
		}
		out = append(out, filename+":"+strconv.Itoa(i+1)+": "+strings.Trim(sqlRead.FindString(code), " \t")+": "+timeReadAdvice)
	}
	return out
}

func TestTimeReadGuard_FlagsUnmarked(t *testing.T) {
	src := `package p
import "time"
func f() {
	_ = time.Now()
	_ = time.Since(time.Time{})
	// realtime:
	_ = time.Until(time.Time{})
	q := "SELECT now()"
	_ = q
}
`
	got, err := scanGoTimeReads("p.go", src, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("want 4 findings (Now, Since, Until with empty reason, SQL now()), got %d: %v", len(got), got)
	}
	if !strings.HasPrefix(got[0], "p.go:4:") || !strings.Contains(got[0], "injected clock") {
		t.Errorf("finding must name file:line and the advice, got %q", got[0])
	}

	sqlGot := scanSQLTimeReads("m.sql", "SELECT 1;\nUPDATE t SET a = now();\n")
	if len(sqlGot) != 1 || !strings.HasPrefix(sqlGot[0], "m.sql:2:") {
		t.Errorf("want one finding at m.sql:2, got %v", sqlGot)
	}
}

func TestTimeReadGuard_TrailingMarkerCoversOnlyItsLine(t *testing.T) {
	src := `package p
import "time"
func f() {
	a := time.Now() // realtime: first
	b := time.Now()
	_, _ = a, b
}
`
	got, err := scanGoTimeReads("p.go", src, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.HasPrefix(got[0], "p.go:5:") {
		t.Fatalf("a trailing marker must not cover the next line; want one finding at p.go:5, got %v", got)
	}
}

func TestTimeReadGuard_AcceptsMarked(t *testing.T) {
	src := `package p
import "time"
func f() {
	_ = time.Now() // realtime: callout deadline
	// realtime: lease expiry
	_ = time.Since(time.Time{})
	// time.Now() in a comment is not a read
	_ = "time.Now() in a string is not a read"
	// realtime: heartbeat
	q := "UPDATE j SET h = now()"
	_ = q
	r := ` + "`" + `SELECT a,
	  clock_timestamp() -- realtime: lease
	` + "`" + `
	_ = r
	// realtime: statement-level marker
	_, _ = db.Exec(ctx,
		"SELECT now()")
}
`
	got, err := scanGoTimeReads("p.go", src, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("marked sites must pass, got %v", got)
	}

	sqlSrc := "UPDATE t SET a = now(); -- realtime: liveness\n-- realtime: liveness\nUPDATE t SET b = now();\n-- a comment about now()\n" +
		"CREATE FUNCTION cyoda_stamp(k int4) RETURNS timestamptz AS $$\nBEGIN\n  x := clock_timestamp();\nEND $$;\n"
	if got := scanSQLTimeReads("m.sql", sqlSrc); len(got) != 0 {
		t.Fatalf("marked SQL and stamp bodies must pass, got %v", got)
	}
}

// repoRoot walks up from the working directory to the nearest directory holding go.work,
// or failing that the nearest go.mod that declares the cyoda-go module.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.work")); err == nil {
			return d
		}
		if b, err := os.ReadFile(filepath.Join(d, "go.mod")); err == nil &&
			strings.HasPrefix(strings.TrimSpace(string(b)), "module github.com/cyoda-platform/cyoda-go\n") {
			return d
		}
		if filepath.Dir(d) == d {
			t.Fatalf("repo root not found above %s", dir)
		}
	}
}

func TestTimeReads_AllClassified(t *testing.T) {
	root := repoRoot(t)
	skipDirs := map[string]bool{
		".git":         true,
		".worktrees":   true,
		".superpowers": true,
		"node_modules": true,
	}
	// The real-clock implementations themselves.
	skipFiles := map[string]bool{
		"internal/platformclock/clock.go": true,
		"plugins/memory/clock.go":         true,
		"plugins/sqlite/clock.go":         true,
	}
	migNum := regexp.MustCompile(`^(\d+)_`)

	var findings []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if skipDirs[rel] {
				return filepath.SkipDir
			}
			return nil
		}
		if skipFiles[rel] {
			return nil
		}
		switch {
		case strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, "_test.go"):
			got, err := scanGoTimeReads(rel, mustRead(t, path), strings.HasPrefix(rel, "plugins/postgres/"))
			if err != nil {
				t.Errorf("parse %s: %v", rel, err)
			}
			findings = append(findings, got...)
		case strings.HasPrefix(rel, "plugins/postgres/migrations/") && strings.HasSuffix(rel, ".up.sql"):
			m := migNum.FindStringSubmatch(filepath.Base(rel))
			if m == nil {
				return nil
			}
			if n, _ := strconv.Atoi(m[1]); n < 17 {
				return nil
			}
			findings = append(findings, scanSQLTimeReads(rel, string(mustRead(t, path)))...)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) > 0 {
		sort.Strings(findings)
		t.Fatalf("%d unclassified time read(s):\n%s", len(findings), strings.Join(findings, "\n"))
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
