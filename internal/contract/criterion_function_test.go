package contract

import (
	"encoding/json"
	"testing"
)

func TestParseCriterionFunction(t *testing.T) {
	no := false
	cases := []struct {
		name        string
		in          string
		want        CriterionFunction
		wantPresent bool
		wantErr     bool
	}{
		{
			name: "full envelope",
			in: `{"type":"function","function":{"name":"min-amount","config":{
				"calculationNodesTags":"pricing,eu","attachEntity":false,
				"responseTimeoutMs":5000,"retryPolicy":"NONE","context":"role=a"}}}`,
			want: CriterionFunction{Name: "min-amount", Config: CriterionFunctionConfig{
				CalculationNodesTags: "pricing,eu", AttachEntity: &no,
				ResponseTimeoutMs: 5000, RetryPolicy: "NONE", Context: "role=a"}},
			wantPresent: true,
		},
		{
			name: "no config: every field at its zero value, attachEntity unset",
			in:   `{"type":"function","function":{"name":"f"}}`,
			want: CriterionFunction{Name: "f"}, wantPresent: true,
		},
		// Member names are exact: a member in the wrong place, misspelt or in
		// another case is refused, not ignored. Ignored, tags written beside
		// config instead of in it would route the callout to any member.
		{name: "a config member beside config", in: `{"type":"function","function":{"name":"f","calculationNodesTags":"pricing"}}`, wantErr: true},
		{name: "an unknown config member", in: `{"type":"function","function":{"name":"f","config":{"x":1}}}`, wantErr: true},
		{name: "a misspelt config member", in: `{"type":"function","function":{"name":"f","config":{"attachEntiy":false}}}`, wantErr: true},
		{name: "a case-variant config member", in: `{"type":"function","function":{"name":"f","config":{"CalculationNodesTags":"pricing"}}}`, wantErr: true},
		{name: "a duplicate member", in: `{"type":"function","function":{"name":"f","name":"g"}}`, wantErr: true},
		{name: "an unknown condition member", in: `{"type":"function","function":{"name":"f"},"jsonPath":"$.a"}`, wantErr: true},
		{name: "function member absent", in: `{"type":"function"}`, want: CriterionFunction{}, wantPresent: false},
		{name: "function member null", in: `{"type":"function","function":null}`, want: CriterionFunction{}, wantPresent: false},
		{name: "function member not an object", in: `{"type":"function","function":"f"}`, wantErr: true},
		{name: "responseTimeoutMs of the wrong type", in: `{"type":"function","function":{"name":"f","config":{"responseTimeoutMs":"5s"}}}`, wantErr: true},
		{name: "retryPolicy of the wrong type", in: `{"type":"function","function":{"name":"f","config":{"retryPolicy":3}}}`, wantErr: true},
		{name: "not JSON", in: `{`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, present, err := ParseCriterionFunction(json.RawMessage(tc.in))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseCriterionFunction = %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCriterionFunction: %v", err)
			}
			if present != tc.wantPresent {
				t.Errorf("present = %t, want %t", present, tc.wantPresent)
			}
			if got.Name != tc.want.Name ||
				got.Config.CalculationNodesTags != tc.want.Config.CalculationNodesTags ||
				got.Config.ResponseTimeoutMs != tc.want.Config.ResponseTimeoutMs ||
				got.Config.RetryPolicy != tc.want.Config.RetryPolicy ||
				got.Config.Context != tc.want.Config.Context {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
			switch {
			case tc.want.Config.AttachEntity == nil && got.Config.AttachEntity != nil:
				t.Errorf("AttachEntity = %v, want nil (unset)", *got.Config.AttachEntity)
			case tc.want.Config.AttachEntity != nil &&
				(got.Config.AttachEntity == nil || *got.Config.AttachEntity != *tc.want.Config.AttachEntity):
				t.Errorf("AttachEntity = %v, want %v", got.Config.AttachEntity, *tc.want.Config.AttachEntity)
			}
		})
	}
}

func TestParseCriterionFunction_CarriesThePreCheckAsWritten(t *testing.T) {
	in := `{"type":"function","function":{"name":"f","criterion":{"type":"simple","jsonPath":"$.a","operatorType":"EQUALS","value":12345678901234567890}}}`
	fn, present, err := ParseCriterionFunction(json.RawMessage(in))
	if err != nil || !present {
		t.Fatalf("ParseCriterionFunction: present=%t err=%v", present, err)
	}
	want := `{"type":"simple","jsonPath":"$.a","operatorType":"EQUALS","value":12345678901234567890}`
	if string(fn.Criterion) != want {
		t.Errorf("Criterion = %s, want %s", fn.Criterion, want)
	}
}

func TestParseCriterionFunction_PreCheckAbsentOrNull(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"absent", `{"type":"function","function":{"name":"f"}}`, ""},
		{"null", `{"type":"function","function":{"name":"f","criterion":null}}`, "null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fn, present, err := ParseCriterionFunction(json.RawMessage(tc.in))
			if err != nil || !present {
				t.Fatalf("ParseCriterionFunction: present=%t err=%v", present, err)
			}
			if string(fn.Criterion) != tc.want {
				t.Errorf("Criterion = %q, want %q", fn.Criterion, tc.want)
			}
		})
	}
}
