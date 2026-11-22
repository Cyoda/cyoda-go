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
// openapi3/schema_jsonschema_validator.go:400-421). The library returns one
// error for the whole value. kin converts its tree of errors into a tree of
// SchemaErrors: each node carries the JSON pointer of the failing value and
// the failing keyword, each leaf the library's error. A node that wraps one
// cause, such as a failing $ref, carries the keyword and pointer of its
// innermost cause. kin's text of that tree repeats the errors of every member
// of every union, and segments of each pointer. jsonSchemaRows writes the tree
// as rows instead.

// jsonSchemaRows returns the rows of err when err is a SchemaError of kin's
// JSON Schema 2020-12 validator, and false otherwise. A row is a leaf of the
// tree: the JSON pointer of the failing value and the library's message.
//
// The library checks a property name as a value of its own and gives the
// error no reliable location (v6.0.3 validator.go:296 stores its location
// without copying it), so a row below a propertyNames failure says it is about
// a property name and gives no pointer. Such rows are told apart per failing
// property name, so two objects' failures are two rows.
//
// Below a oneOf or anyOf, only the members the value can be are followed.
// Whether a member refuses the value is judged from the member's own errors,
// not those of a union inside it, and a member that is a union is judged by
// its own branches:
//   - a member that refuses the kind of the value (an object for null, say)
//     is left out, unless every member does;
//   - under a discriminator, a member that refuses the value's key is left
//     out; when every member does, the discriminator has a mapping, and the
//     key is absent or not one of the mapping's keys, the key is the defect,
//     and one row says so.
//
// The rows are sorted by JSON pointer, a pointer before those below it, then
// by message, property-name rows last. Pointers compare token by token: an
// integer token before any other, integers by value and then spelling, other
// tokens by text. A
// row whose text equals another's is left out, except a property-name row of
// another failing property name.
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
		return cmp.Or(cmp.Compare(boolInt(x.propertyName), boolInt(y.propertyName)),
			comparePointers(x.pointer, y.pointer), strings.Compare(x.message, y.message))
	})
	rows := make([]error, len(b.rows))
	for i, r := range b.rows {
		rows[i] = r
	}
	return rows, true
}

type jsonSchemaRow struct {
	pointer      string
	message      string
	propertyName bool // the row is about a property name; pointer is unknown
}

func (r jsonSchemaRow) Error() string {
	if r.propertyName {
		return "Error at a property name: " + r.message
	}
	return fmt.Sprintf("Error at %q: %s", r.pointer, r.message)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

var rowPrinter = message.NewPrinter(language.English)

type rowBuilder struct {
	rows []jsonSchemaRow
	seen map[string]bool
	// propertyName is the propertyNames failure the current node is below,
	// or nil.
	propertyName *openapi3.SchemaError
}

func (b *rowBuilder) add(r jsonSchemaRow) {
	key := r.Error()
	if r.propertyName {
		key = fmt.Sprintf("%p %s", b.propertyName, key)
	}
	if !b.seen[key] {
		b.seen[key] = true
		b.rows = append(b.rows, r)
	}
}

func (b *rowBuilder) node(se *openapi3.SchemaError) {
	if se.SchemaField == "propertyNames" {
		b.propertyName = se
		defer func() { b.propertyName = nil }()
	}
	kids := children(se)
	if len(kids) == 0 {
		b.leaf(se)
		return
	}
	if isUnion(se) {
		kids = b.members(se, kids)
	}
	for _, k := range kids {
		b.node(k)
	}
}

func isUnion(se *openapi3.SchemaError) bool {
	return se.SchemaField == "oneOf" || se.SchemaField == "anyOf"
}

// leaf adds the row of a leaf of kin's tree. kin records the library's error
// on every node of the tree (withCause), and a leaf unwraps to it.
func (b *rowBuilder) leaf(se *openapi3.SchemaError) {
	var ve *jsonschema.ValidationError
	errors.As(se, &ve)
	msg := ve.ErrorKind.LocalizedString(rowPrinter)
	if b.propertyName != nil {
		b.add(jsonSchemaRow{message: msg, propertyName: true})
		return
	}
	b.add(jsonSchemaRow{pointer: pointerOf(se.JSONPointer()), message: msg})
}

// members returns the members of the union se, given as its child errors,
// that the value can be (see jsonSchemaRows). It adds the key row itself
// when no member takes the value's key.
func (b *rowBuilder) members(se *openapi3.SchemaError, kids []*openapi3.SchemaError) []*openapi3.SchemaError {
	at := pointerOf(se.JSONPointer())
	fits := slices.DeleteFunc(slices.Clone(kids), func(k *openapi3.SchemaError) bool {
		return !isUnion(k) && ownLeaf(k, func(l *openapi3.SchemaError) bool {
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
		return !isUnion(k) && refusesKey(k, at, d.PropertyName)
	})
	if len(keyed) > 0 {
		return keyed
	}
	if len(d.Mapping) == 0 { // no keys to name: the members' rows say why
		return fits
	}
	obj, _ := se.Value.(map[string]any)
	key, present := obj[d.PropertyName]
	if s, ok := key.(string); ok {
		if _, mapped := d.Mapping[s]; mapped {
			// A mapped key is not the defect, even when the members
			// refuse it: their rows say why.
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

// children returns the SchemaErrors a node of kin's tree holds as its
// causes; kin builds each cause as a SchemaError.
func children(se *openapi3.SchemaError) []*openapi3.SchemaError {
	var multi openapi3.MultiError
	if !errors.As(se.Origin, &multi) {
		return nil
	}
	out := make([]*openapi3.SchemaError, len(multi))
	for i, e := range multi {
		out[i] = e.(*openapi3.SchemaError)
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
		return !isUnion(k) && ownLeaf(k, f)
	})
}

// comparePointers orders JSON pointers token by token: an integer token
// before any other, two integers by value and then by spelling, other tokens
// by text; a pointer before the pointers below it.
func comparePointers(x, y string) int {
	xs, ys := strings.Split(x, "/"), strings.Split(y, "/")
	for i := 0; i < len(xs) && i < len(ys); i++ {
		xn, xerr := strconv.Atoi(xs[i])
		yn, yerr := strconv.Atoi(ys[i])
		if c := cmp.Compare(boolInt(xerr != nil), boolInt(yerr != nil)); c != 0 {
			return c
		}
		if xerr == nil {
			if c := cmp.Compare(xn, yn); c != 0 {
				return c
			}
		}
		if c := cmp.Compare(xs[i], ys[i]); c != 0 {
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
