package openapivalidator

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// kin-openapi checks a schema of a 3.1 document with its JSON Schema 2020-12
// validator (santhosh-tekuri/jsonschema) unless the schema holds a component
// reference and reaches no JSON Schema 2020-12 keyword, or the library cannot
// compile it for another reason than an unresolved reference (kin
// openapi3/schema_jsonschema_validator.go:400-421). The library returns
// one error for the whole value. kin converts its tree of errors into a tree
// of SchemaErrors: each node carries the JSON pointer of the failing value and
// the failing keyword, each leaf the library's error. A node that wraps one
// cause, such as a failing $ref, carries the keyword of its innermost cause. kin's text of that tree
// repeats the errors of every member of every union, and segments of each
// pointer. jsonSchemaRows writes the tree as rows instead.

// jsonSchemaRows returns the rows of err when err is a SchemaError of kin's
// JSON Schema 2020-12 validator, and false otherwise. A row is a leaf of the
// tree: the JSON pointer of the failing value and the library's message.
// Below a oneOf, only the members the value can be are followed. Whether a member refuses the value is judged from the member's
// own errors, not those of a union inside it:
//   - a member that refuses the kind of the value (an object for null, say)
//     is left out, unless every member does;
//   - under a discriminator, a member that refuses the value's key is left
//     out; when every member does and the key is absent or not mapped, the
//     key is the defect, and one row says so.
//
// The rows are sorted by JSON pointer, array indexes in numeric order, then by
// message; a row whose text equals another's is left out.
func jsonSchemaRows(err error) ([]error, bool) {
	se, ok := err.(*openapi3.SchemaError)
	if !ok {
		return nil, false
	}
	var ve *jsonschema.ValidationError
	if !errors.As(se, &ve) {
		return nil, false
	}
	b := &rowBuilder{seen: map[string]bool{}}
	b.node(se)
	slices.SortFunc(b.rows, func(x, y jsonSchemaRow) int {
		return cmp.Or(comparePointers(x.pointer, y.pointer), strings.Compare(x.message, y.message))
	})
	rows := make([]error, len(b.rows))
	for i, r := range b.rows {
		rows[i] = r
	}
	return rows, true
}

type jsonSchemaRow struct {
	pointer string
	message string
}

func (r jsonSchemaRow) Error() string {
	return fmt.Sprintf("Error at %q: %s", r.pointer, r.message)
}

var rowPrinter = message.NewPrinter(language.English)

type rowBuilder struct {
	rows []jsonSchemaRow
	seen map[string]bool
}

func (b *rowBuilder) add(r jsonSchemaRow) {
	if !b.seen[r.Error()] {
		b.seen[r.Error()] = true
		b.rows = append(b.rows, r)
	}
}

func (b *rowBuilder) node(se *openapi3.SchemaError) {
	kids := children(se)
	if len(kids) == 0 {
		b.leaf(se)
		return
	}
	if se.SchemaField == "oneOf" {
		kids = b.members(se, kids)
	}
	for _, k := range kids {
		b.node(k)
	}
}

func (b *rowBuilder) leaf(se *openapi3.SchemaError) {
	msg := se.Reason
	var ve *jsonschema.ValidationError
	if errors.As(se, &ve) {
		msg = ve.ErrorKind.LocalizedString(rowPrinter)
	}
	b.add(jsonSchemaRow{pointer: pointerOf(se.JSONPointer()), message: msg})
}

// members returns the members of the oneOf se, given as its child errors,
// that the value can be (see jsonSchemaRows). It adds the key row itself
// when no member takes the value's key.
func (b *rowBuilder) members(se *openapi3.SchemaError, kids []*openapi3.SchemaError) []*openapi3.SchemaError {
	at := pointerOf(se.JSONPointer())
	fits := slices.DeleteFunc(slices.Clone(kids), func(k *openapi3.SchemaError) bool {
		return ownLeaf(k, func(l *openapi3.SchemaError) bool {
			return l.SchemaField == "type" && pointerOf(l.JSONPointer()) == at
		})
	})
	if len(fits) == 0 {
		return kids
	}
	if se.Schema == nil || se.Schema.Discriminator == nil {
		return fits
	}
	d := se.Schema.Discriminator
	keyed := slices.DeleteFunc(slices.Clone(fits), func(k *openapi3.SchemaError) bool {
		return refusesKey(k, at, d.PropertyName)
	})
	if len(keyed) > 0 {
		return keyed
	}
	obj, _ := se.Value.(map[string]any)
	key, present := obj[d.PropertyName]
	if s, ok := key.(string); ok {
		if _, mapped := d.Mapping[s]; mapped {
			// A mapped key is not the defect. A node that wraps the union
			// (a failing $ref) gets here too, with the union as its member.
			return fits
		}
	}
	if !present {
		b.add(jsonSchemaRow{pointer: at, message: fmt.Sprintf("missing discriminator property %q", d.PropertyName)})
		return nil
	}
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range d.Mapping {
			if !yield(k) {
				return
			}
		}
	})
	quoted := make([]string, len(keys))
	for i, k := range keys {
		quoted[i] = fmt.Sprintf("%q", k)
	}
	value, _ := json.Marshal(key)
	b.add(jsonSchemaRow{
		pointer: at + "/" + pointerEscaper.Replace(d.PropertyName),
		message: fmt.Sprintf("%s is not one of the mapped keys %s", value, strings.Join(quoted, ", ")),
	})
	return nil
}

// refusesKey reports whether the member error k refuses the key property of
// the value at the pointer at: a failure at the key itself, or the key
// missing from a required list.
func refusesKey(k *openapi3.SchemaError, at, property string) bool {
	keyAt := at + "/" + pointerEscaper.Replace(property)
	return ownLeaf(k, func(l *openapi3.SchemaError) bool {
		p := pointerOf(l.JSONPointer())
		if p == keyAt {
			return true
		}
		var ve *jsonschema.ValidationError
		if p != at || !errors.As(l, &ve) {
			return false
		}
		r, ok := ve.ErrorKind.(*kind.Required)
		return ok && slices.Contains(r.Missing, property)
	})
}

// children returns the SchemaErrors a node of kin's tree holds as its causes.
func children(se *openapi3.SchemaError) []*openapi3.SchemaError {
	var multi openapi3.MultiError
	if se.Origin == nil || !errors.As(se.Origin, &multi) {
		return nil
	}
	out := make([]*openapi3.SchemaError, 0, len(multi))
	for _, e := range multi {
		var k *openapi3.SchemaError
		if errors.As(e, &k) {
			out = append(out, k)
		}
	}
	return out
}

// ownLeaf reports whether f holds for a leaf of the member error se, not
// counting the leaves below a union inside the member.
func ownLeaf(se *openapi3.SchemaError, f func(*openapi3.SchemaError) bool) bool {
	kids := children(se)
	if len(kids) == 0 {
		return f(se)
	}
	return slices.ContainsFunc(kids, func(k *openapi3.SchemaError) bool {
		return k.SchemaField != "oneOf" && k.SchemaField != "anyOf" && ownLeaf(k, f)
	})
}

// comparePointers orders JSON pointers token by token, comparing two array
// indexes as numbers.
func comparePointers(x, y string) int {
	xs, ys := strings.Split(x, "/"), strings.Split(y, "/")
	for i := 0; i < len(xs) && i < len(ys); i++ {
		xn, xerr := strconv.Atoi(xs[i])
		yn, yerr := strconv.Atoi(ys[i])
		if c := cmp.Compare(xs[i], ys[i]); xerr == nil && yerr == nil {
			c = cmp.Compare(xn, yn)
			if c != 0 {
				return c
			}
		} else if c != 0 {
			return c
		}
	}
	return cmp.Compare(len(xs), len(ys))
}

func pointerOf(tokens []string) string {
	var p strings.Builder
	for _, t := range tokens {
		p.WriteString("/")
		p.WriteString(pointerEscaper.Replace(t))
	}
	return p.String()
}

// pointerEscaper escapes a JSON pointer reference token (RFC 6901).
var pointerEscaper = strings.NewReplacer("~", "~0", "/", "~1")
