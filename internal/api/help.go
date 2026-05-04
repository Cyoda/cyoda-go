package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/cyoda-platform/cyoda-go/cmd/cyoda/help"
	"github.com/cyoda-platform/cyoda-go/cmd/cyoda/help/renderer"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// topicPathPattern accepts both . and / as topic-path separators.
// Must start and end with alphanumeric; internal characters allow
// letters, digits, underscore, hyphen, and the two separators (. and /).
// Note: Go's http.ServeMux cleans consecutive slashes (cli//help → cli/help)
// before the handler runs, so double-slash cannot be detected at this layer.
var topicPathPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._/-]*[A-Za-z0-9])?$`)

// RegisterHelpRoutes mounts GET {contextPath}/help and
// GET {contextPath}/help/{topic} on the given mux. contextPath must NOT
// have a trailing slash. An empty contextPath mounts at "/help".
// version is closed over by the handlers and reported in the full-tree
// payload. CORS for /help is handled by the unified middleware in
// internal/api/middleware/cors.go — these handlers no longer manage CORS
// themselves.
func RegisterHelpRoutes(mux *http.ServeMux, tree *help.Tree, contextPath, version string) {
	prefix := strings.TrimRight(contextPath, "/") + "/help"
	mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			common.WriteError(w, r, common.Operational(
				http.StatusMethodNotAllowed,
				common.ErrCodeBadRequest,
				"method not allowed; GET only",
			))
			return
		}
		if r.URL.Path != prefix {
			common.WriteError(w, r, common.Operational(
				http.StatusNotFound,
				common.ErrCodeHelpTopicNotFound,
				"no such help topic at this path",
			))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		if err := enc.Encode(renderer.HelpPayload{
			Schema:  1,
			Version: version,
			Topics:  tree.WalkDescriptors(),
		}); err != nil {
			slog.Error("help: failed to encode response", "error", err, "path", r.URL.Path)
		}
	})
	mux.HandleFunc(prefix+"/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			common.WriteError(w, r, common.Operational(
				http.StatusMethodNotAllowed,
				common.ErrCodeBadRequest,
				"method not allowed; GET only",
			))
			return
		}
		topic := strings.TrimPrefix(r.URL.Path, prefix+"/")
		if !topicPathPattern.MatchString(topic) {
			common.WriteError(w, r, common.Operational(
				http.StatusBadRequest,
				common.ErrCodeBadRequest,
				"invalid topic path: contains disallowed characters",
			))
			return
		}
		// Normalise: / and . are equivalent separators. Split on either,
		// then reject any empty segment (double separator, leading/trailing).
		normalized := strings.ReplaceAll(topic, "/", ".")
		segs := strings.Split(normalized, ".")
		for _, s := range segs {
			if s == "" {
				common.WriteError(w, r, common.Operational(
					http.StatusBadRequest,
					common.ErrCodeBadRequest,
					"invalid topic path: empty segment",
				))
				return
			}
		}
		node := tree.Find(segs)
		if node == nil {
			common.WriteError(w, r, common.Operational(
				http.StatusNotFound,
				common.ErrCodeHelpTopicNotFound,
				"no such help topic: "+topic,
			))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		if err := enc.Encode(node.Descriptor()); err != nil {
			slog.Error("help: failed to encode response", "error", err, "path", r.URL.Path)
		}
	})
}
