package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

// command is one first argument cyoda accepts. The commands table is the
// whole dispatch: a command line it does not cover is a usage error, never a
// server start.
type command struct {
	// names are the first arguments that select the command; names[0] is
	// its canonical name.
	names []string
	// synopsis is the command's line in the usage summary.
	synopsis string
	// topic is the dotted help topic that documents the command, or "" for
	// a global flag.
	topic string
	// takesArgs is false for a command that takes no argument at all;
	// resolveCommand refuses one given to it. A command that takes arguments
	// refuses the ones it does not understand itself.
	takesArgs bool
	// parsesFlags is true for a command with a flag set of its own, which
	// answers -h and --help with its flag usage. For any other command with
	// a topic, resolveCommand answers a lone -h or --help with that topic.
	parsesFlags bool
	run         func(args []string) int
}

// serveCommand is what a bare 'cyoda' runs.
var serveCommand = command{
	names:    []string{"serve"},
	synopsis: "cyoda [serve]",
	topic:    "cli.serve",
	run:      func([]string) int { return runServeCmd() },
}

var commands = []command{
	serveCommand,
	{
		names:       []string{"init"},
		synopsis:    "cyoda init [--force]",
		topic:       "cli.init",
		takesArgs:   true,
		parsesFlags: true,
		run:         runInit,
	},
	{
		names:    []string{"health"},
		synopsis: "cyoda health",
		topic:    "cli.health",
		run:      func([]string) int { return runHealth() },
	},
	{
		names:       []string{"migrate"},
		synopsis:    "cyoda migrate [--timeout <duration>]",
		topic:       "cli.migrate",
		takesArgs:   true,
		parsesFlags: true,
		run:         runMigrate,
	},
	{
		names:       []string{"token"},
		synopsis:    "cyoda token --tenant <tenantId> [--user <userId>] [--roles <r1,r2>] [--ttl <duration>]",
		topic:       "cli.token",
		takesArgs:   true,
		parsesFlags: true,
		run:         func(args []string) int { return runToken(args, os.Stdout, os.Stderr) },
	},
	{
		names:     []string{"help"},
		synopsis:  "cyoda help [<topic>...] [--format=<fmt>]",
		topic:     "cli.help",
		takesArgs: true,
		run:       runHelpCmd,
	},
	{
		// Delegates to the help subsystem so there is a single source of
		// truth: no positional args renders the USAGE + FLAGS + TOPICS
		// summary.
		names:    []string{"--help", "-h"},
		synopsis: "cyoda --help | -h",
		run:      func([]string) int { return runHelpCmd(nil) },
	},
	{
		names:    []string{"--version", "-v"},
		synopsis: "cyoda --version | -v",
		run: func([]string) int {
			printVersion(os.Stdout)
			return 0
		},
	},
}

// resolveCommand returns the command a command line selects and the
// arguments that command is given. No argument selects the server. A lone
// -h or --help after a command without a flag set of its own selects that
// command's help topic. An argument that names no command, and an argument
// given to a command that takes none, are errors: a typo, a guessed command
// or a flag must not start a server against the user's configuration and
// data store.
func resolveCommand(args []string) (command, []string, error) {
	if len(args) == 0 {
		return serveCommand, nil, nil
	}
	name, rest := args[0], args[1:]
	for _, c := range commands {
		if !slices.Contains(c.names, name) {
			continue
		}
		answersHelp := c.topic != "" && !c.parsesFlags && len(rest) > 0 && (rest[0] == "-h" || rest[0] == "--help")
		if answersHelp && len(rest) == 1 {
			return c.helpTopic(), nil, nil
		}
		if answersHelp {
			return command{}, nil, fmt.Errorf("%q: %s must be the only argument", name, rest[0])
		}
		if len(rest) > 0 && !c.takesArgs {
			if c.names[0] == serveCommand.names[0] && strings.HasPrefix(rest[0], "-") {
				return command{}, nil, fmt.Errorf("%q takes no arguments, got %q; %s", name, rest[0], serverFlagHint)
			}
			return command{}, nil, fmt.Errorf("%q takes no arguments, got %q", name, rest[0])
		}
		return c, rest, nil
	}
	if strings.HasPrefix(name, "-") {
		return command{}, nil, fmt.Errorf("unknown flag %q; %s", name, serverFlagHint)
	}
	return command{}, nil, fmt.Errorf("unknown command %q", name)
}

// serverFlagHint follows an error for a flag given to the server, which has
// none: it says where the server's configuration comes from.
const serverFlagHint = "the server takes no flags, it is configured by CYODA_* environment variables (see 'cyoda help config')"

// helpTopic returns a command that shows c's help topic in place of running c.
func (c command) helpTopic() command {
	c.run = func([]string) int { return runHelpCmd(strings.Split(c.topic, ".")) }
	return c
}

// writeUsage writes the usage summary printed on a usage error.
func writeUsage(w io.Writer) {
	fmt.Fprintln(w, "USAGE")
	for _, c := range commands {
		fmt.Fprintf(w, "  %s\n", c.synopsis)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run 'cyoda help cli' for details.")
}
