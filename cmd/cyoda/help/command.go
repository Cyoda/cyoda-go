// Package help — CLI dispatch for the `cyoda help` subcommand.
//
// CLI output uses fmt.Fprint to injected writers — this is user-facing
// output, not operational logging. The log/slog rule applies to
// slog-ingested diagnostic events, not stdout.
package help

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/cyoda-platform/cyoda-go/cmd/cyoda/help/renderer"
)

// RunHelp dispatches `cyoda help [args...]`. Returns the intended exit
// code: 0 on success, 2 on unknown topic / bad args.
//
//	tree    — the resolved topic tree
//	args    — positional and --format args after "help"
//	out     — stdout of the CLI
//	version — binary version string for HelpPayload.Version
//	isTTY   — whether out is a TTY (governs text vs markdown default format)
//	style   — glamour theme name: "dark", "light", or "" for no-ANSI
func RunHelp(tree *Tree, args []string, out io.Writer, version string, isTTY bool, style string) int {
	format := "auto"
	var positional []string
	for _, a := range args {
		if strings.HasPrefix(a, "--format=") {
			format = strings.TrimPrefix(a, "--format=")
			continue
		}
		if a == "--format" {
			fmt.Fprintln(out, "cyoda help: --format requires = value (e.g. --format=json)")
			return 2
		}
		positional = append(positional, a)
	}

	if !validFormat(format) {
		fmt.Fprintf(out, "cyoda help: unknown --format %q (want: auto, text, markdown, json)\n", format)
		return 2
	}

	// No positional: render tree summary. In json mode emit the full payload.
	if len(positional) == 0 {
		if format == "json" {
			return writeFullTreeJSON(tree, out, version)
		}
		return writeTreeSummary(tree, out, style)
	}

	// "config all" is both a registered action (HTTP, and the generic
	// CLI action dispatch below) and a CLI special-case here — the
	// special-case runs first so plain-text is the CLI default and
	// --format=json is honored, which the generic action dispatch
	// (ContentType is fixed, ignores --format) cannot do.
	if len(positional) == 2 && positional[0] == "config" && positional[1] == "all" {
		if resolveFormat(format, isTTY) == "json" {
			return writeConfigAllJSONVersion(out, version)
		}
		return writeConfigAllText(out)
	}

	// Topic lookup.
	topic := tree.Find(positional)
	if topic == nil {
		// Action lookup: if positional[:len-1] is a known topic and
		// positional[len-1] is a registered action on it, dispatch.
		if len(positional) >= 2 {
			parentPath := positional[:len(positional)-1]
			actionName := positional[len(positional)-1]
			parent := tree.Find(parentPath)
			if parent != nil {
				if entry, ok := lookupAction(parent.DottedPath(), actionName); ok {
					return entry.Handler(out)
				}
				// Dynamic action resolvers (issue #111): the openapi
				// topic accepts any tag slug as an action, resolved at
				// dispatch time. If a resolver matches, use it; if not,
				// fall through to the unknown-action error which now
				// includes "(and any tag slug — see 'cyoda help openapi
				// tags')" for discoverability.
				if parent.DottedPath() == "openapi" {
					if handler, ok := lookupOpenAPITagAction(actionName, format); ok {
						return handler(out)
					}
				}
				// Topic exists but the action name is unknown — improve the error.
				if avail := actionsFor(parent.DottedPath()); avail != nil {
					hint := ""
					if parent.DottedPath() == "openapi" {
						hint = " (or any tag slug — see 'cyoda help openapi tags')"
					}
					fmt.Fprintf(out, "cyoda help %s: unknown action %q. Available actions: %s%s\n",
						strings.Join(parentPath, " "), actionName, strings.Join(avail, ", "), hint)
					return 2
				}
			}
		}
		writeUnknownTopicError(tree, positional, out)
		return 2
	}

	switch resolveFormat(format, isTTY) {
	case "json":
		return writeTopicJSON(topic, out)
	case "markdown":
		return writeTopicMarkdown(topic, out)
	default:
		return writeTopicText(topic, out, style)
	}
}

func validFormat(f string) bool {
	switch f {
	case "auto", "", "text", "markdown", "json", "yaml":
		return true
	}
	return false
}

func resolveFormat(f string, isTTY bool) string {
	switch f {
	case "json", "markdown", "text":
		return f
	}
	// "auto" or "" — choose based on TTY.
	if isTTY {
		return "text"
	}
	return "markdown"
}

func writeTopicText(t *Topic, out io.Writer, style string) int {
	cleaned := renderer.StripSeeAlsoSection(t.Body)
	if err := renderer.RenderText(out, cleaned, style); err != nil {
		fmt.Fprintf(out, "cyoda help: render failed: %v\n", err)
		return 1
	}
	if len(t.Children) > 0 {
		bold, reset := "", ""
		if style != "" {
			bold = "\x1b[1m"
			reset = "\x1b[0m"
		}
		fmt.Fprintf(out, "\n%sSUBTOPICS%s\n", bold, reset)
		// Children are already sorted in Load via sortTree — emit in order.
		for _, c := range t.Children {
			// c.Path contains the full path; emit only the last segment as the invocable child.
			fmt.Fprintf(out, "  cyoda help %s\n", strings.Join(c.Path, " "))
		}
	}
	if actions := actionsFor(t.DottedPath()); len(actions) > 0 {
		bold, reset := "", ""
		if style != "" {
			bold = "\x1b[1m"
			reset = "\x1b[0m"
		}
		fmt.Fprintf(out, "\n%sACTIONS%s\n", bold, reset)
		for _, a := range actions {
			fmt.Fprintf(out, "  cyoda help %s %s\n", strings.Join(t.Path, " "), a)
		}
	}
	if len(t.SeeAlso) > 0 {
		fmt.Fprintln(out, "\nSEE ALSO")
		for _, s := range t.SeeAlso {
			fmt.Fprintf(out, "  • %s\n", dottedToCLIArgs(s))
		}
	}
	return 0
}

// dottedToCLIArgs converts a canonical dotted topic identifier
// (e.g. "errors.VALIDATION_FAILED") to the space-separated form the
// CLI accepts (e.g. "errors VALIDATION_FAILED"). Used only in text-
// mode SEE ALSO output; markdown and JSON keep the dotted form.
func dottedToCLIArgs(dotted string) string {
	return strings.ReplaceAll(dotted, ".", " ")
}

func writeTopicMarkdown(t *Topic, out io.Writer) int {
	renderer.RenderMarkdown(out, t.Body, t.SeeAlso)
	if len(t.Children) > 0 {
		fmt.Fprintln(out, "\n## SUBTOPICS")
		fmt.Fprintln(out)
		for _, c := range t.Children {
			fmt.Fprintf(out, "- `cyoda help %s`\n", strings.Join(c.Path, " "))
		}
	}
	if actions := actionsFor(t.DottedPath()); len(actions) > 0 {
		fmt.Fprintln(out, "\n## ACTIONS")
		fmt.Fprintln(out)
		for _, a := range actions {
			fmt.Fprintf(out, "- `cyoda help %s %s`\n", strings.Join(t.Path, " "), a)
		}
	}
	return 0
}

func writeTopicJSON(t *Topic, out io.Writer) int {
	d := t.Descriptor()
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	_ = enc.Encode(d)
	return 0
}

func writeFullTreeJSON(tree *Tree, out io.Writer, version string) int {
	payload := renderer.HelpPayload{
		Schema:  1,
		Version: version,
		Topics:  tree.WalkDescriptors(),
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
	return 0
}

func writeTreeSummary(tree *Tree, out io.Writer, style string) int {
	// ANSI bold accents — only when a theme is active (i.e. a TTY).
	bold, reset := "", ""
	if style != "" {
		bold = "\x1b[1m"
		reset = "\x1b[0m"
	}

	fmt.Fprintf(out, "%scyoda help — browse the topic tree%s\n\n", bold, reset)
	fmt.Fprintf(out, "%sUSAGE%s\n", bold, reset)
	fmt.Fprintln(out, "  cyoda help [<topic>...] [--format=<fmt>]")
	fmt.Fprintln(out, "  cyoda --help                  alias for 'cyoda help'")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "%sFLAGS%s\n", bold, reset)
	fmt.Fprintln(out, "  --format=<fmt>   output format: auto (default), text, markdown, json")
	fmt.Fprintln(out, "                   auto selects text on a TTY, markdown off-TTY")
	fmt.Fprintln(out, "  --help, -h       show this summary")
	fmt.Fprintln(out, "  --version, -v    print version info")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "%sTOPICS%s\n\n", bold, reset)

	buckets := map[string][]*Topic{}
	for _, t := range tree.Root.Children {
		buckets[t.Stability] = append(buckets[t.Stability], t)
	}
	for _, stab := range []string{"stable", "evolving", "experimental"} {
		list := buckets[stab]
		if len(list) == 0 {
			continue
		}
		sort.Slice(list, func(i, j int) bool {
			return list[i].Path[0] < list[j].Path[0]
		})
		title := bucketTitle(stab)
		fmt.Fprintln(out, title)
		for _, t := range list {
			fmt.Fprintf(out, "  %-16s %s\n", t.Path[0], renderer.ExtractTagline(t.Body))
		}
		fmt.Fprintln(out)
	}
	if topics := topicsWithActions(); len(topics) > 0 {
		fmt.Fprintf(out, "%sTOPIC ACTIONS%s\n", bold, reset)
		fmt.Fprintln(out, "  Some topics support machine-readable output via actions:")
		fmt.Fprintln(out)
		for _, tp := range topics {
			acts := actionsFor(tp)
			fmt.Fprintf(out, "  cyoda help %s %s\n",
				strings.ReplaceAll(tp, ".", " "),
				strings.Join(acts, "|"))
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintln(out, "Run 'cyoda help <topic>' for details.")
	return 0
}

// bucketTitle returns the human-readable bucket header for a stability level.
// (strings.Title is deprecated for Unicode reasons — we enumerate explicitly.)
func bucketTitle(stab string) string {
	switch stab {
	case "stable":
		return "Stable"
	case "evolving":
		return "Evolving"
	case "experimental":
		return "Experimental — content pending"
	default:
		return stab
	}
}

func writeUnknownTopicError(tree *Tree, args []string, out io.Writer) {
	// Find the nearest existing parent and list its children.
	parent := tree.Root
	matched := 0
	for i, seg := range args {
		found := false
		for _, c := range parent.Children {
			if len(c.Path) > 0 && c.Path[len(c.Path)-1] == seg {
				parent = c
				matched = i + 1
				found = true
				break
			}
		}
		if !found {
			break
		}
	}
	if matched >= len(args) {
		// Defensive: should not happen because Find would have returned non-nil.
		fmt.Fprintf(out, "cyoda help: topic lookup failed for %q\n", strings.Join(args, " "))
		return
	}
	missing := args[matched]
	if matched == 0 {
		fmt.Fprintf(out, "cyoda help: no such topic: %q. Run 'cyoda help' to list available topics.\n", missing)
		return
	}
	parentPath := strings.Join(args[:matched], " ")
	var kids []string
	for _, c := range parent.Children {
		kids = append(kids, c.Path[len(c.Path)-1])
	}
	sort.Strings(kids)
	fmt.Fprintf(out, "cyoda help: topic %q has no subtopic %q. Available: %s. Run 'cyoda help %s' for an overview.\n",
		parentPath, missing, strings.Join(kids, ", "), parentPath)
}
