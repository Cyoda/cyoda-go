package common

import (
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
)

// DecodeJSON decodes one JSON value — a client's typed request body — into
// dst. A member name must spell a field exactly: a name that differs only in
// case, a duplicate name and an unknown name are each refused, as is anything
// but whitespace after the value. A refusal is a *DecodeError, whose text
// names the JSON location and never a Go type, so a caller may put it in a
// 400 detail as it is.
//
// Every typed request body goes through here, and so does a type that decodes
// itself from an UnmarshalJSON method: its error then names the path from the
// top-level value. Entity payloads do not: in those, "a" and "A" are
// different fields.
func DecodeJSON(data []byte, dst any, opts ...DecodeOption) error {
	options := []json.Options{json.RejectUnknownMembers(true)}
	for _, o := range opts {
		options = append(options, o.options)
	}
	if err := json.Unmarshal(data, dst, options...); err != nil {
		return describeDecodeError(err)
	}
	return nil
}

// DecodeOption changes one rule of DecodeJSON.
type DecodeOption struct{ options json.Options }

var (
	// PreserveNumbers makes a JSON number held in an `any`, at any depth, a
	// json.Number with the literal as written, instead of a float64 that
	// rounds an integer above 2^53.
	PreserveNumbers = DecodeOption{json.WithUnmarshalers(json.UnmarshalFromFunc(numberAsLiteral))}

	// AllowUnknownMembers ignores a member name the target does not declare,
	// for a probe that reads only some members of a body and for a shape a
	// standard leaves open (a JWK). Names still match exactly, and a
	// duplicate is still refused.
	AllowUnknownMembers = DecodeOption{json.RejectUnknownMembers(false)}
)

func numberAsLiteral(dec *jsontext.Decoder, v *any) error {
	if dec.PeekKind() != '0' {
		return errors.ErrUnsupported // decoded as v2 decodes it
	}
	tok, err := dec.ReadToken()
	if err != nil {
		return err
	}
	*v = jsonv1.Number(tok.String())
	return nil
}

// DecodeBoundedJSON reads at most max bytes of the request body and decodes
// them with DecodeJSON. Returns a non-nil error on an oversize body or a
// body DecodeJSON refuses; callers translate that to 400 BAD_REQUEST via
// WriteError.
func DecodeBoundedJSON(w http.ResponseWriter, r *http.Request, max int64, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	return DecodeJSON(data, dst)
}

// DecodeError is a body DecodeJSON refused: where (a JSON Pointer, or a byte
// offset where the text is not JSON) and why.
type DecodeError struct {
	at     jsontext.Pointer
	offset int64
	syntax bool
	reason string
}

func (e *DecodeError) Error() string {
	if e.syntax {
		return fmt.Sprintf("invalid JSON at offset %d: %s", e.offset, e.reason)
	}
	return fmt.Sprintf("invalid JSON at %q: %s", e.at, e.reason)
}

func describeDecodeError(err error) *DecodeError {
	var syn *jsontext.SyntacticError
	if errors.As(err, &syn) {
		if errors.Is(syn.Err, jsontext.ErrDuplicateName) {
			return &DecodeError{at: syn.JSONPointer.Parent(), reason: fmt.Sprintf("duplicate member name %q", syn.JSONPointer.LastToken())}
		}
		return &DecodeError{syntax: true, offset: syn.ByteOffset, reason: syn.Err.Error()}
	}
	var sem *json.SemanticError
	if !errors.As(err, &sem) {
		return &DecodeError{reason: "not accepted"}
	}
	var inner *DecodeError
	switch {
	case errors.As(sem.Err, &inner):
		// A value that decoded itself through DecodeJSON: its location is
		// relative to that value.
		if inner.syntax {
			return &DecodeError{at: sem.JSONPointer, reason: inner.reason}
		}
		return &DecodeError{at: sem.JSONPointer + inner.at, reason: inner.reason}
	case errors.Is(sem.Err, json.ErrUnknownName):
		return &DecodeError{at: sem.JSONPointer.Parent(), reason: fmt.Sprintf("unknown member name %q", sem.JSONPointer.LastToken())}
	case errors.Is(sem.Err, strconv.ErrRange):
		return &DecodeError{at: sem.JSONPointer, reason: "a JSON number out of range"}
	case sem.Err == nil, errors.Is(sem.Err, strconv.ErrSyntax):
		return &DecodeError{at: sem.JSONPointer, reason: fmt.Sprintf("%s where %s is expected", jsonKindName(sem.JSONKind), expectedJSON(sem.GoType))}
	default:
		// An UnmarshalJSON method's own words.
		return &DecodeError{at: sem.JSONPointer, reason: sem.Err.Error()}
	}
}

// jsonKindName names the kind of a JSON value as a client reads it.
func jsonKindName(k jsontext.Kind) string {
	switch k {
	case 'n':
		return "a JSON null"
	case 't', 'f':
		return "a JSON boolean"
	case '"':
		return "a JSON string"
	case '0':
		return "a JSON number"
	case '{':
		return "a JSON object"
	case '[':
		return "a JSON array"
	default:
		return "a JSON value"
	}
}

// expectedJSON names, in JSON terms, what a Go type accepts.
func expectedJSON(t reflect.Type) string {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return "another value"
	}
	switch t.Kind() {
	case reflect.Bool:
		return "a boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "an integer"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.String:
		return "a string"
	case reflect.Slice, reflect.Array:
		return "an array"
	case reflect.Struct, reflect.Map:
		return "an object"
	default:
		return "another value"
	}
}
