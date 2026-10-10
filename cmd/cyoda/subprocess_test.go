//go:build !windows

package main

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// childOutput collects the child's combined stdout and stderr. exec writes to
// it from its own goroutines while the test reads it, hence the lock.
type childOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *childOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

func (o *childOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

// cyodaChild is the cyoda binary running as a subprocess. The child gets one
// cmd.Wait; exited is closed once it returns and waitErr holds its result, so
// the test and the cleanup can both ask, any number of times.
type cyodaChild struct {
	cmd     *exec.Cmd
	out     *childOutput
	exited  chan struct{}
	waitErr error // written once, before exited is closed
}

// startCyoda builds the cyoda binary and starts it in its own process group,
// so a signal sent to the child never reaches the test process. Every port is
// 0: the OS assigns them and the child logs what it bound, so no port is ever
// picked here, released, and claimed again by the child — a window in which a
// neighbouring test can take it. env entries override the defaults.
func startCyoda(t *testing.T, env ...string) *cyodaChild {
	t.Helper()
	return startCyodaWithArgs(t, nil, env...)
}

// startCyodaWithArgs is startCyoda with a command line: args are passed to
// the child after the program name.
func startCyodaWithArgs(t *testing.T, args []string, env ...string) *cyodaChild {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "cyoda-test")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("go build cyoda: %v", err)
	}

	c := &cyodaChild{cmd: exec.Command(bin, args...), out: &childOutput{}, exited: make(chan struct{})}
	c.cmd.Env = append(append(os.Environ(),
		"CYODA_HTTP_PORT=0",
		"CYODA_GRPC_PORT=0",
		"CYODA_ADMIN_PORT=0",
		"CYODA_HTTP_BIND_ADDRESS=127.0.0.1",
		"CYODA_GRPC_BIND_ADDRESS=127.0.0.1",
		"CYODA_ADMIN_BIND_ADDRESS=127.0.0.1",
		"CYODA_SUPPRESS_BANNER=true",
		"CYODA_LOG_LEVEL=info",
		"CYODA_OTEL_ENABLED=false",
		"CYODA_IAM_MODE=mock",
		// Keep the child off the developer's own configuration, data store
		// and cluster: the user config is looked up under XDG_CONFIG_HOME,
		// and a shell variable wins over every env file.
		"XDG_CONFIG_HOME="+t.TempDir(),
		"CYODA_PROFILES=",
		"CYODA_STORAGE_BACKEND=memory",
		"CYODA_CLUSTER_ENABLED=false",
	), env...)
	c.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Logging goes to stdout (see internal/logging.Init); capture both
	// streams so a test can assert on any line the child wrote.
	c.cmd.Stdout = c.out
	c.cmd.Stderr = c.out

	if err := c.cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	go func() {
		c.waitErr = c.cmd.Wait()
		close(c.exited)
	}()
	t.Cleanup(func() {
		// Kill, then reap: a test that failed before the child exited must
		// not leave it behind. Both are no-ops once it has exited.
		_ = c.cmd.Process.Signal(syscall.SIGKILL)
		<-c.exited
		if t.Failed() {
			t.Logf("child output:\n%s", c.out.String())
		}
	})
	return c
}

// loopbackAddr waits for the child to log that the named server ("gRPC",
// "HTTP" or "admin") is starting and returns a loopback address for the port
// it bound. The sockets are bound before that line is written, so a dial to
// the returned address connects.
func (c *cyodaChild) loopbackAddr(t *testing.T, server string) string {
	t.Helper()
	line := regexp.MustCompile(`msg="` + regexp.QuoteMeta(server) + ` server starting" addr=(\S+)`)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if m := line.FindStringSubmatch(c.out.String()); m != nil {
			_, port, err := net.SplitHostPort(m[1])
			if err != nil {
				t.Fatalf("child logged an unparseable %s address %q: %v", server, m[1], err)
			}
			return net.JoinHostPort("127.0.0.1", port)
		}
		if time.Now().After(deadline) {
			t.Fatalf("child did not log its %s address within 15s", server)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// wait returns the child's exit code, failing the test if it has not exited
// within the timeout.
func (c *cyodaChild) wait(t *testing.T, timeout time.Duration) int {
	t.Helper()
	select {
	case <-c.exited:
		var exitErr *exec.ExitError
		if errors.As(c.waitErr, &exitErr) {
			return exitErr.ExitCode()
		}
		if c.waitErr != nil {
			t.Fatalf("wait for child: %v", c.waitErr)
		}
		return 0
	case <-time.After(timeout):
		_ = c.cmd.Process.Signal(syscall.SIGKILL)
		t.Fatalf("child did not exit within %v", timeout)
		return -1
	}
}

// TestStartup_PortConflict_TearsDownAndExits1 pins the fail-fast contract end
// to end. A port that cannot be bound stops the process with exit status 1
// before any server starts, and the App built by then is closed on the way
// out. The evidence of teardown is App.Close's own log line: Shutdown, which
// runs immediately before it and is what deregisters a clustered node from
// its peers, logs nothing on a standalone node.
func TestStartup_PortConflict_TearsDownAndExits1(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess startup test in -short mode")
	}

	// The very address the child's HTTP surface binds: a holder on a
	// different host of the same port does not collide on every platform.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	_, port, _ := net.SplitHostPort(taken.Addr().String())

	child := startCyoda(t, "CYODA_HTTP_PORT="+port)

	if code := child.wait(t, 30*time.Second); code != 1 {
		t.Errorf("exit code = %d; want 1", code)
	}
	out := child.out.String()
	for _, want := range []string{
		`msg="listen failed"`,
		"http listener",
		`msg="shutting down"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("child output is missing %q", want)
		}
	}
	if strings.Contains(out, "server starting") {
		t.Error("a server started although a port could not be bound")
	}
}

// TestStartup_MockIAMOffLoopbackWarns pins that the mock-IAM exposure check
// runs on the real startup path, before any server starts: a mock-IAM child
// whose HTTP listener is on every interface names it at WARN, and one with
// both API listeners on loopback does not. The compiled-in default itself is
// pinned in package app.
func TestStartup_MockIAMOffLoopbackWarns(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess startup test in -short mode")
	}
	const warning = `msg="mock IAM is active on a listener that is not on loopback`

	t.Run("http on every interface", func(t *testing.T) {
		child := startCyoda(t, "CYODA_HTTP_BIND_ADDRESS=0.0.0.0")
		child.loopbackAddr(t, "gRPC")
		child.loopbackAddr(t, "HTTP")
		out := child.out.String()
		if !strings.Contains(out, warning) || !strings.Contains(out, "listener=http") {
			t.Errorf("child output has no WARN naming the HTTP listener")
		}
		if strings.Contains(out, "listener=grpc") {
			t.Errorf("child output names the gRPC listener, which is on loopback")
		}
	})
	t.Run("loopback", func(t *testing.T) {
		child := startCyoda(t)
		child.loopbackAddr(t, "gRPC")
		child.loopbackAddr(t, "HTTP")
		if strings.Contains(child.out.String(), warning) {
			t.Errorf("child output has the exposure WARN although every API listener is on loopback")
		}
	})
}

// TestStartup_UnknownArgumentExits2 pins end to end that a command line cyoda
// does not understand stops the process with exit status 2 and the usage
// summary on stderr, before any configuration is loaded or any server starts.
func TestStartup_UnknownArgumentExits2(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess startup test in -short mode")
	}

	for _, args := range [][]string{
		{"version"},
		{"--http-port", "8081"},
		{"serve", "extra"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			child := startCyodaWithArgs(t, args)

			if code := child.wait(t, 30*time.Second); code != 2 {
				t.Errorf("exit code = %d; want 2", code)
			}
			out := child.out.String()
			for _, want := range []string{"cyoda: ", "USAGE", "cyoda help cli"} {
				if !strings.Contains(out, want) {
					t.Errorf("child output is missing %q", want)
				}
			}
			if strings.Contains(out, "server starting") {
				t.Error("a server started on an unknown argument")
			}
		})
	}
}

// TestStartup_UnknownArgumentLoadsNoConfig pins that a refused command line
// reads no env file: the child has a user config, and the log line that
// app.LoadEnvFiles writes for a loaded file never appears. migrate, which
// loads the env files, is the positive control for that log line.
func TestStartup_UnknownArgumentLoadsNoConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess startup test in -short mode")
	}
	const loaded = "loaded env files"
	xdg := t.TempDir()
	if err := os.MkdirAll(filepath.Join(xdg, "cyoda"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "cyoda", "cyoda.env"), []byte("CYODA_LOG_LEVEL=info\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	control := startCyodaWithArgs(t, []string{"migrate"}, "XDG_CONFIG_HOME="+xdg)
	if code := control.wait(t, 30*time.Second); code != 0 {
		t.Fatalf("control: migrate exit code = %d; want 0", code)
	}
	if !strings.Contains(control.out.String(), loaded) {
		t.Fatalf("control: migrate did not log %q; the assertion below would prove nothing", loaded)
	}

	refused := startCyodaWithArgs(t, []string{"version"}, "XDG_CONFIG_HOME="+xdg)
	if code := refused.wait(t, 30*time.Second); code != 2 {
		t.Errorf("exit code = %d; want 2", code)
	}
	if strings.Contains(refused.out.String(), loaded) {
		t.Error("a refused command line loaded the env files")
	}
}

// TestStartup_ServeCommandStartsServer pins that 'cyoda serve' starts the
// server, as a bare 'cyoda' does.
func TestStartup_ServeCommandStartsServer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess startup test in -short mode")
	}

	child := startCyodaWithArgs(t, []string{"serve"})
	child.loopbackAddr(t, "HTTP")
}

// TestStartup_HelpFlagAfterCommandShowsTopic pins end to end that -h after a
// command that parses no flags prints that command's help topic and exits 0,
// without starting anything.
func TestStartup_HelpFlagAfterCommandShowsTopic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess startup test in -short mode")
	}

	for _, tc := range []struct {
		args  []string
		topic string
	}{
		{[]string{"serve", "--help"}, "cli.serve"},
		{[]string{"health", "-h"}, "cli.health"},
		{[]string{"help", "-h"}, "cli.help"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			child := startCyodaWithArgs(t, tc.args)

			if code := child.wait(t, 30*time.Second); code != 0 {
				t.Errorf("exit code = %d; want 0", code)
			}
			out := child.out.String()
			if !strings.Contains(out, tc.topic) {
				t.Errorf("child output does not show help topic %q", tc.topic)
			}
			if strings.Contains(out, "server starting") {
				t.Error("a server started on a help request")
			}
		})
	}
}
