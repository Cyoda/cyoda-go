package grpc

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// jsonMembers returns the JSON member names a struct type declares.
func jsonMembers(t reflect.Type) []string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var names []string
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func memberType(t reflect.Type, name string) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	for i := range t.NumField() {
		if n, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); n == name {
			return t.Field(i).Type
		}
	}
	return nil
}

// The compute-answer types decode strictly, so a schema member they do not
// declare makes a valid answer unreadable. They must declare exactly the
// members of the generated types, the error object's included.
func TestAnswerTypes_DeclareTheSchemaMembers(t *testing.T) {
	for name, pair := range map[string][2]reflect.Type{
		"processor": {reflect.TypeFor[processorAnswer](), reflect.TypeFor[events.EntityProcessorCalculationResponseJson]()},
		"criteria":  {reflect.TypeFor[criteriaAnswer](), reflect.TypeFor[events.EntityCriteriaCalculationResponseJson]()},
		"function":  {reflect.TypeFor[functionAnswer](), reflect.TypeFor[events.EntityFunctionCalculationResponseJson]()},
		"payload":   {reflect.TypeFor[processorPayload](), reflect.TypeFor[events.DataPayloadJson]()},
	} {
		ours, schema := pair[0], pair[1]
		if got, want := jsonMembers(ours), jsonMembers(schema); !slices.Equal(got, want) {
			t.Errorf("%s: members %v, schema %v", name, got, want)
		}
		if e := memberType(schema, "error"); e != nil {
			if got, want := jsonMembers(memberType(ours, "error")), jsonMembers(e); !slices.Equal(got, want) {
				t.Errorf("%s error: members %v, schema %v", name, got, want)
			}
		}
	}
}
