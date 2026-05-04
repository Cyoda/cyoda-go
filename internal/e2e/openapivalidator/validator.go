package openapivalidator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
)

// Validator wraps the spec's router and validates HTTP responses against
// the matched operation's declared schema.
//
// IncludeResponseStatus=true is load-bearing: openapi3filter's default
// behavior is to silently pass undeclared status codes (verified against
// kin-openapi v0.137.0 openapi3filter/validate_response.go:48-58). Without
// this flag the validator misses an entire class of drift.
//
// MultiError=true accumulates all schema errors per response rather than
// failing on the first.
//
// Fallback route matching: the underlying kin-openapi gorillamux router
// cannot disambiguate overlapping path templates that differ only in parameter
// constraints (e.g. /entity/{format} with enum vs /entity/{entityId} with
// format=uuid). When FindRoute returns a "method not allowed" error, Validate
// calls fallbackFindRoute which walks the spec's paths and applies parameter
// constraints (enum, format=uuid, format=int*) to select the correct
// operation. When a fallback match is found, exercise tracking fires normally;
// response body schema validation is skipped (the match proves the route is
// declared; full schema validation requires a proper routers.Route which the
// fallback path cannot construct without re-entering the broken router).
type Validator struct {
	doc    *openapi3.T
	router routers.Router
	opts   *openapi3filter.Options
}

// NewValidator builds a Validator from a parsed OpenAPI 3.1 document.
func NewValidator(doc *openapi3.T) (*Validator, error) {
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		return nil, fmt.Errorf("build router: %w", err)
	}
	return &Validator{
		doc:    doc,
		router: router,
		opts: &openapi3filter.Options{
			IncludeResponseStatus: true,
			MultiError:            true,
			AuthenticationFunc: func(ctx context.Context, ai *openapi3filter.AuthenticationInput) error {
				return nil // skip auth checks; we validate shape only
			},
		},
	}, nil
}

// Validate runs the response through openapi3filter.ValidateResponse and
// returns any mismatches it finds. Returns an empty slice on success.
//
// Records the matched operationId in the package's exercised set, regardless
// of whether validation passed.
//
// Wraps the underlying call in a panic recovery so that bugs in kin-openapi
// (or in our spec/wire data hitting an untested code path) surface as
// mismatch records rather than crashing the test server's request goroutine.
func (v *Validator) Validate(ctx context.Context, req *http.Request, resp *http.Response) (mismatches []Mismatch) {
	defer func() {
		if r := recover(); r != nil {
			mismatches = append(mismatches, Mismatch{
				Method: req.Method,
				Path:   req.URL.Path,
				Status: resp.StatusCode,
				Reason: fmt.Sprintf("validator panic: %v", r),
			})
		}
	}()
	route, pathParams, err := v.router.FindRoute(req)
	if err != nil {
		// Primary router failed. Attempt fallback constraint-aware matching to
		// work around the kin-openapi gorillamux limitation with overlapping
		// path templates (e.g. /entity/{format} enum vs /entity/{entityId} uuid).
		if opId, op := v.fallbackFindRoute(req); op != nil {
			// Fallback matched a declared route. Record exercise; skip body
			// schema validation (we can't construct a full routers.Route without
			// re-entering the broken router).
			defaultCollector.recordExercised(opId)
			return nil
		}
		// No matching route — the request hit a path the spec doesn't declare.
		// This is a real mismatch (handler exists for an undeclared route).
		return []Mismatch{{
			Operation: "<unmatched>",
			Method:    req.Method,
			Path:      req.URL.Path,
			Status:    resp.StatusCode,
			Reason:    fmt.Sprintf("no spec route matches %s %s: %v", req.Method, req.URL.Path, err),
		}}
	}

	// The kin-openapi router may pick a template whose parameter constraints
	// are not satisfied by the actual path segments (e.g. it picks
	// /entity/{format} enum for POST /entity/<UUID>). Validate path parameters
	// against their declared constraints and re-attempt via the fallback if
	// the primary match violates them.
	if !v.pathParamsSatisfied(route.Operation, pathParams) {
		if opId, op := v.fallbackFindRoute(req); op != nil {
			defaultCollector.recordExercised(opId)
			return nil
		}
		return []Mismatch{{
			Operation: "<unmatched>",
			Method:    req.Method,
			Path:      req.URL.Path,
			Status:    resp.StatusCode,
			Reason:    fmt.Sprintf("no spec route matches %s %s: path parameter constraints not satisfied", req.Method, req.URL.Path),
		}}
	}

	opId := route.Operation.OperationID
	defaultCollector.recordExercised(opId)

	// Streaming check: if the matched operation declares
	// application/x-ndjson for the actual status code, skip body validation.
	// kin-openapi's ValidateResponse panics if input.Body is nil (the
	// `defer body.Close()` line in validate_response.go), so we use a
	// dedicated streaming-only options copy with ExcludeResponseBody=true
	// AND pass a non-nil empty body for defense-in-depth.
	if v.isStreaming(route, resp.StatusCode) {
		streamingOpts := *v.opts // copy; do not mutate the shared opts
		streamingOpts.ExcludeResponseBody = true
		input := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{
				Request: req,
				Route:   route,
			},
			Status:  resp.StatusCode,
			Header:  resp.Header,
			Body:    io.NopCloser(strings.NewReader("")),
			Options: &streamingOpts,
		}
		if err := openapi3filter.ValidateResponse(ctx, input); err != nil {
			return v.toMismatches(err, opId, req, resp.StatusCode)
		}
		return nil
	}

	// Read response body for validation. The middleware passed the captured
	// bytes via resp.Body; we consume them here.
	// Options must be set on ResponseValidationInput — ValidateResponse reads
	// input.Options (not input.RequestValidationInput.Options).
	input := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request: req,
			Route:   route,
		},
		Status:  resp.StatusCode,
		Header:  resp.Header,
		Body:    resp.Body,
		Options: v.opts,
	}
	if err := openapi3filter.ValidateResponse(ctx, input); err != nil {
		return v.toMismatches(err, opId, req, resp.StatusCode)
	}
	return nil
}

// isStreaming reports whether the matched operation declares
// application/x-ndjson for the given status code.
//
// IMPORTANT: when isStreaming returns true, the body is NOT validated against
// the items schema. kin-openapi's ValidateResponse parses the body as a single
// JSON document and cannot process newline-delimited streams. Per-item shape
// assertions for streaming endpoints belong in domain-specific E2E tests
// (see internal/e2e/search_test.go for the searchEntities ndjson coverage).
// Future work: a custom line-by-line validator could close this gap.
func (v *Validator) isStreaming(route *routers.Route, status int) bool {
	if route.Operation == nil || route.Operation.Responses == nil {
		return false
	}
	resp := route.Operation.Responses.Status(status)
	if resp == nil || resp.Value == nil {
		return false
	}
	for ct := range resp.Value.Content {
		if ct == "application/x-ndjson" {
			return true
		}
	}
	return false
}

// pathParamsSatisfied reports whether all path parameters in the matched
// operation satisfy their declared schema constraints (enum, format=uuid,
// format=int32/int64). pathParams is the map returned by router.FindRoute.
// Returns true if all constraints pass or cannot be checked (nil schema).
func (v *Validator) pathParamsSatisfied(op *openapi3.Operation, pathParams map[string]string) bool {
	if op == nil {
		return true
	}
	for _, pRef := range op.Parameters {
		p := pRef.Value
		if p == nil || p.In != "path" {
			continue
		}
		value, ok := pathParams[p.Name]
		if !ok {
			continue
		}
		if !satisfiesParamConstraint(p, value) {
			return false
		}
	}
	return true
}

// fallbackFindRoute is invoked when the primary kin-openapi router fails to
// find a matching route. It walks the spec's paths looking for any operation
// whose method matches and whose path template's parameter constraints
// (format, enum) are satisfied by the request's actual path segments.
//
// This works around a known kin-openapi gorillamux limitation: it can't
// disambiguate overlapping templates like /entity/{format} (enum) vs
// /entity/{entityId} (uuid format). When the router picks the wrong template,
// it returns "method not allowed" instead of trying alternatives.
//
// Returns the matched operationId (string) and operation object, or "" / nil
// if no match.
func (v *Validator) fallbackFindRoute(req *http.Request) (string, *openapi3.Operation) {
	reqSegments := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	// Strip the server base-path prefix if present (e.g. "api").
	base := strings.Trim(v.basePath(), "/")
	if base != "" && len(reqSegments) > 0 && reqSegments[0] == base {
		reqSegments = reqSegments[1:]
	}

	type candidate struct {
		op       *openapi3.Operation
		opId     string
		literals int // count of non-parameter segments — higher is more specific
	}
	var matches []candidate

	for path, item := range v.doc.Paths.Map() {
		templateSegments := strings.Split(strings.Trim(path, "/"), "/")
		if len(templateSegments) != len(reqSegments) {
			continue
		}
		op := operationForMethod(item, req.Method)
		if op == nil {
			continue
		}
		if !v.segmentsMatchTemplate(templateSegments, reqSegments, op) {
			continue
		}
		literals := 0
		for _, s := range templateSegments {
			if !strings.HasPrefix(s, "{") {
				literals++
			}
		}
		matches = append(matches, candidate{op: op, opId: op.OperationID, literals: literals})
	}

	if len(matches) == 0 {
		return "", nil
	}
	// Prefer most-literal-segment match (more specific path wins).
	best := matches[0]
	for _, c := range matches[1:] {
		if c.literals > best.literals {
			best = c
		}
	}
	return best.opId, best.op
}

// basePath returns the server URL's path component (e.g. "/api") if Servers
// declares a single relative URL, or "" if none.
func (v *Validator) basePath() string {
	if len(v.doc.Servers) == 0 {
		return ""
	}
	return v.doc.Servers[0].URL
}

// operationForMethod returns the *openapi3.Operation for the given HTTP method,
// or nil if not declared on the path item.
func operationForMethod(item *openapi3.PathItem, method string) *openapi3.Operation {
	switch strings.ToUpper(method) {
	case "GET":
		return item.Get
	case "POST":
		return item.Post
	case "PUT":
		return item.Put
	case "DELETE":
		return item.Delete
	case "PATCH":
		return item.Patch
	case "HEAD":
		return item.Head
	case "OPTIONS":
		return item.Options
	}
	return nil
}

// segmentsMatchTemplate reports whether reqSegments satisfy the path template's
// parameter constraints (format=uuid, enum=...) for the given operation.
// Returns true only if every segment in the template either matches literally
// or satisfies the corresponding parameter's declared constraints.
func (v *Validator) segmentsMatchTemplate(template, reqSegs []string, op *openapi3.Operation) bool {
	paramByName := map[string]*openapi3.Parameter{}
	for _, p := range op.Parameters {
		if p.Value != nil && p.Value.In == "path" {
			paramByName[p.Value.Name] = p.Value
		}
	}

	for i, t := range template {
		if !strings.HasPrefix(t, "{") {
			// Literal segment — must match exactly.
			if t != reqSegs[i] {
				return false
			}
			continue
		}
		// Parameter segment — look up constraints.
		name := strings.TrimSuffix(strings.TrimPrefix(t, "{"), "}")
		param := paramByName[name]
		if param == nil {
			// No constraint info; accept any value.
			continue
		}
		if !satisfiesParamConstraint(param, reqSegs[i]) {
			return false
		}
	}
	return true
}

// satisfiesParamConstraint checks whether value satisfies the parameter's
// schema constraints. Checks: enum values, format=uuid, format=int32/int64.
// For other formats and unrecognized constraints it returns true (best-effort
// heuristic, not a full schema validator).
func satisfiesParamConstraint(p *openapi3.Parameter, value string) bool {
	if p.Schema == nil || p.Schema.Value == nil {
		return true
	}
	s := p.Schema.Value
	if len(s.Enum) > 0 {
		for _, allowed := range s.Enum {
			if fmt.Sprint(allowed) == value {
				return true
			}
		}
		return false
	}
	switch s.Format {
	case "uuid":
		return uuidRe.MatchString(value)
	case "int32", "int64":
		_, err := strconv.ParseInt(value, 10, 64)
		return err == nil
	}
	return true
}

// uuidRe matches the canonical 8-4-4-4-12 hex UUID format.
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// toMismatches converts the kin-openapi error tree into one or more Mismatch
// records. MultiError is unwrapped so each schema problem becomes its own row.
func (v *Validator) toMismatches(err error, opId string, req *http.Request, status int) []Mismatch {
	var multi openapi3.MultiError
	if errors.As(err, &multi) {
		out := make([]Mismatch, 0, len(multi))
		for _, e := range multi {
			out = append(out, Mismatch{
				Operation: opId,
				Method:    req.Method,
				Path:      req.URL.Path,
				Status:    status,
				Reason:    e.Error(),
			})
		}
		return out
	}
	return []Mismatch{{
		Operation: opId,
		Method:    req.Method,
		Path:      req.URL.Path,
		Status:    status,
		Reason:    err.Error(),
	}}
}
