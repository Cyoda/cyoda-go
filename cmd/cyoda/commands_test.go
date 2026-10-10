package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/cmd/cyoda/help"
)

// TestResolveCommand_SelectsCommand pins the command each accepted command
// line selects, and the arguments that command is given.
func TestResolveCommand_SelectsCommand(t *testing.T) {
	cases := []struct {
		args     []string
		wantName string
		wantArgs []string
	}{
		{nil, "serve", nil},
		{[]string{"serve"}, "serve", nil},
		{[]string{"init", "--force"}, "init", []string{"--force"}},
		{[]string{"health"}, "health", nil},
		{[]string{"migrate", "--timeout", "1m"}, "migrate", []string{"--timeout", "1m"}},
		{[]string{"token", "--tenant", "t1"}, "token", []string{"--tenant", "t1"}},
		{[]string{"help", "cli", "serve"}, "help", []string{"cli", "serve"}},
		{[]string{"--help"}, "--help", nil},
		{[]string{"-h"}, "--help", nil},
		{[]string{"--version"}, "--version", nil},
		{[]string{"-v"}, "--version", nil},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			c, args, err := resolveCommand(tc.args)
			if err != nil {
				t.Fatalf("resolveCommand(%q): %v", tc.args, err)
			}
			if c.names[0] != tc.wantName {
				t.Errorf("command = %q; want %q", c.names[0], tc.wantName)
			}
			if !slices.Equal(args, tc.wantArgs) {
				t.Errorf("args = %q; want %q", args, tc.wantArgs)
			}
		})
	}
}

// TestResolveCommand_RejectsUnknownArguments pins that a command line cyoda
// does not understand is an error, never a server start: an unknown command,
// a flag, and an argument given to a command that takes none.
func TestResolveCommand_RejectsUnknownArguments(t *testing.T) {
	cases := []struct {
		args    []string
		wantErr string
	}{
		{[]string{"version"}, `unknown command "version"`},
		{[]string{"helth"}, `unknown command "helth"`},
		{[]string{"start"}, `unknown command "start"`},
		{[]string{"--http-port", "8081"}, `unknown flag "--http-port"`},
		{[]string{"serve", "--http-port", "8081"}, `"serve" takes no arguments, got "--http-port"`},
		{[]string{"health", "--port", "9999"}, `"health" takes no arguments, got "--port"`},
		{[]string{"--version", "extra"}, `"--version" takes no arguments, got "extra"`},
		{[]string{"-h", "cli"}, `"-h" takes no arguments, got "cli"`},
		{[]string{"unknown\x1b[2J"}, `unknown command "unknown\x1b[2J"`},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			_, _, err := resolveCommand(tc.args)
			if err == nil {
				t.Fatalf("resolveCommand(%q) succeeded; want an error", tc.args)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q; want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestResolveCommand_FlagErrorPointsAtEnvironment pins the hint a flag gets:
// the server has no configuration flags, so the error says where its
// configuration comes from.
func TestResolveCommand_FlagErrorPointsAtEnvironment(t *testing.T) {
	_, _, err := resolveCommand([]string{"--http-port", "8081"})
	if err == nil {
		t.Fatal("resolveCommand succeeded; want an error")
	}
	for _, want := range []string{"CYODA_", "cyoda help config"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q; want it to mention %q", err, want)
		}
	}
}

// TestWriteUsage_ListsEveryCommand pins that the usage summary printed on a
// usage error names every command cyoda accepts.
func TestWriteUsage_ListsEveryCommand(t *testing.T) {
	var buf bytes.Buffer
	writeUsage(&buf)
	out := buf.String()
	for _, c := range commands {
		if !strings.Contains(out, c.synopsis) {
			t.Errorf("usage is missing %q:\n%s", c.synopsis, out)
		}
	}
	if !strings.Contains(out, "cyoda help cli") {
		t.Errorf("usage does not point at 'cyoda help cli':\n%s", out)
	}
}

// TestCommands_MatchCLIHelpTopics pins that the command table and the cli
// help subtopics name the same commands, in both directions: a subtopic for a
// command that does not exist invites a guess that is then refused.
func TestCommands_MatchCLIHelpTopics(t *testing.T) {
	tree := help.BuildTree()
	cli := tree.Find([]string{"cli"})
	if cli == nil {
		t.Fatal("help tree has no cli topic")
	}
	var topics []string
	for _, c := range commands {
		if c.topic == "" {
			continue
		}
		topics = append(topics, c.topic)
		if tree.Find(strings.Split(c.topic, ".")) == nil {
			t.Errorf("command %q names help topic %q, which does not exist", c.names[0], c.topic)
		}
	}
	for _, child := range cli.Children {
		if !slices.Contains(topics, child.DottedPath()) {
			t.Errorf("help topic %q names no command", child.DottedPath())
		}
	}
}

// TestRunInit_RejectsPositionalArgument pins that init refuses an argument
// it does not take, and writes nothing.
func TestRunInit_RejectsPositionalArgument(t *testing.T) {
	tmp := setupIsolatedConfig(t)

	if code := runInit([]string{"--force", "extra"}); code != 2 {
		t.Errorf("runInit exit code = %d; want 2", code)
	}
	if _, err := os.Stat(filepath.Join(tmp, "cyoda", "cyoda.env")); !os.IsNotExist(err) {
		t.Errorf("runInit wrote a config although it refused its arguments (stat: %v)", err)
	}
}

// TestRunMigrate_RejectsPositionalArgument pins that migrate refuses an
// argument it does not take before it reads any configuration.
func TestRunMigrate_RejectsPositionalArgument(t *testing.T) {
	t.Setenv("CYODA_STORAGE_BACKEND", "no-such-backend")
	if code := runMigrate([]string{"--timeout", "1m", "extra"}); code != 2 {
		t.Errorf("runMigrate exit code = %d; want 2", code)
	}
}

// TestResolveCommand_HelpFlagOnCommandWithoutFlags pins that -h or --help
// after a command that parses no flags of its own is a request for that
// command's help topic, not a usage error.
func TestResolveCommand_HelpFlagOnCommandWithoutFlags(t *testing.T) {
	for _, args := range [][]string{
		{"serve", "-h"},
		{"serve", "--help"},
		{"health", "-h"},
		{"help", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			c, rest, err := resolveCommand(args)
			if err != nil {
				t.Fatalf("resolveCommand(%q): %v", args, err)
			}
			if c.names[0] != args[0] {
				t.Errorf("command = %q; want %q", c.names[0], args[0])
			}
			if len(rest) != 0 {
				t.Errorf("args = %q; want none", rest)
			}
		})
	}
	// Only alone: -h does not license other arguments.
	if _, _, err := resolveCommand([]string{"serve", "-h", "extra"}); err == nil {
		t.Error(`resolveCommand("serve -h extra") succeeded; want an error`)
	}
}

// TestSubcommandHelpFlag_Exits0 pins that -h on a subcommand with flags is a
// request for its usage, not a usage error: exit 0, as 'cyoda token -h' and
// Go's flag package convention do.
func TestSubcommandHelpFlag_Exits0(t *testing.T) {
	setupIsolatedConfig(t)
	if code := runInit([]string{"-h"}); code != 0 {
		t.Errorf("runInit(-h) exit code = %d; want 0", code)
	}
	if code := runMigrate([]string{"-h"}); code != 0 {
		t.Errorf("runMigrate(-h) exit code = %d; want 0", code)
	}
}
