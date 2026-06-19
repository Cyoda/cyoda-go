package help

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	descriptorpb "google.golang.org/protobuf/types/descriptorpb"
	"gopkg.in/yaml.v3"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	protos "github.com/cyoda-platform/cyoda-go/proto"

	// Blank imports to register the proto files in the global registry.
	_ "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	_ "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
)

// ActionFunc is the signature for a topic-scoped action handler. It
// writes raw content to w and returns a CLI exit code.
type ActionFunc func(w io.Writer) int

// ActionEntry pairs an action handler with the HTTP Content-Type
// header to set when serving the action's output over HTTP. The CLI
// ignores ContentType; the HTTP action-mirror handler in
// internal/api/help.go uses it.
type ActionEntry struct {
	Handler     ActionFunc
	ContentType string
}

// actionRegistry maps topic dotted-path to a map of action-name to
// ActionEntry. Actions are invoked via "cyoda help <topic> <action>"
// or "GET /help/<topic>/<action>". Action names must not collide
// with subtopic names on the same topic.
var actionRegistry = map[string]map[string]ActionEntry{
	"openapi": {
		"json": {Handler: emitOpenAPIJSON, ContentType: "application/json"},
		"yaml": {Handler: emitOpenAPIYAML, ContentType: "application/yaml"},
		"tags": {Handler: emitOpenAPITags, ContentType: "text/plain; charset=utf-8"},
	},
	"grpc": {
		"proto": {Handler: emitGRPCProto, ContentType: "text/plain; charset=utf-8"},
		"json":  {Handler: emitGRPCDescriptorJSON, ContentType: "application/json"},
	},
	"cloudevents": {
		"json": {Handler: emitCloudEventsJSON, ContentType: "application/json"},
	},
}

// LookupAction returns the action entry for a topic, if registered.
// Exported for HTTP-action-mirror consumers in internal/api.
func LookupAction(topic, action string) (ActionEntry, bool) {
	return lookupAction(topic, action)
}

// lookupAction returns the entry for a topic action, if registered.
func lookupAction(topic, action string) (ActionEntry, bool) {
	if m, ok := actionRegistry[topic]; ok {
		if e, ok := m[action]; ok {
			return e, true
		}
	}
	return ActionEntry{}, false
}

// actionsFor returns the sorted list of registered action names for
// a topic, or nil if the topic has none. Used for error messages.
func actionsFor(topic string) []string {
	m, ok := actionRegistry[topic]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// insertion sort for deterministic output without importing sort
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// topicsWithActions returns the sorted list of topic dotted-paths
// that have registered actions.
func topicsWithActions() []string {
	out := make([]string, 0, len(actionRegistry))
	for k := range actionRegistry {
		out = append(out, k)
	}
	// insertion sort for deterministic output without importing sort
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// emitOpenAPIJSON writes the embedded OpenAPI spec to w as pretty JSON.
func emitOpenAPIJSON(w io.Writer) int {
	swagger, err := genapi.GetSwagger()
	if err != nil {
		fmt.Fprintf(w, "cyoda help openapi json: %v\n", err)
		return 1
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(swagger); err != nil {
		fmt.Fprintf(w, "cyoda help openapi json: encode: %v\n", err)
		return 1
	}
	return 0
}

// emitOpenAPIYAML writes the embedded OpenAPI spec to w as YAML.
// Round-trips via JSON because openapi3.T carries json tags only.
func emitOpenAPIYAML(w io.Writer) int {
	swagger, err := genapi.GetSwagger()
	if err != nil {
		fmt.Fprintf(w, "cyoda help openapi yaml: %v\n", err)
		return 1
	}
	jsonBytes, err := json.Marshal(swagger)
	if err != nil {
		fmt.Fprintf(w, "cyoda help openapi yaml: marshal json: %v\n", err)
		return 1
	}
	var tree interface{}
	if err := yaml.Unmarshal(jsonBytes, &tree); err != nil {
		fmt.Fprintf(w, "cyoda help openapi yaml: build tree: %v\n", err)
		return 1
	}
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(tree); err != nil {
		fmt.Fprintf(w, "cyoda help openapi yaml: encode yaml: %v\n", err)
		return 1
	}
	_ = enc.Close()
	return 0
}

// emitGRPCProto writes the raw .proto source to w, concatenating the
// cyoda-cloud-api.proto and the cloudevents.proto with separator
// comments so they are human-readable in one stream.
func emitGRPCProto(w io.Writer) int {
	fmt.Fprintln(w, "// ====================================================================")
	fmt.Fprintln(w, "// proto/cyoda/cyoda-cloud-api.proto")
	fmt.Fprintln(w, "// ====================================================================")
	fmt.Fprintln(w, protos.CyodaCloudAPIProto)
	fmt.Fprintln(w, "// ====================================================================")
	fmt.Fprintln(w, "// proto/cloudevents/cloudevents.proto")
	fmt.Fprintln(w, "// ====================================================================")
	fmt.Fprintln(w, protos.CloudEventsProto)
	return 0
}

// emitGRPCDescriptorJSON emits the FileDescriptorSet for the cyoda-owned
// proto files as protojson. Uses Option A: protoreflect global registry
// populated by the blank imports of the generated pb.go packages.
func emitGRPCDescriptorJSON(w io.Writer) int {
	set := &descriptorpb.FileDescriptorSet{}
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		path := string(fd.Path())
		if strings.HasPrefix(path, "cyoda/") || strings.HasPrefix(path, "cloudevents/") {
			set.File = append(set.File, protodesc.ToFileDescriptorProto(fd))
		}
		return true
	})
	b, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(set)
	if err != nil {
		fmt.Fprintf(w, "cyoda help grpc json: marshal: %v\n", err)
		return 1
	}
	_, err = fmt.Fprintln(w, string(b))
	if err != nil {
		fmt.Fprintf(w, "cyoda help grpc json: write: %v\n", err)
		return 1
	}
	return 0
}
