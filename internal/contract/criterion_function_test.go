package contract

import (
	"encoding/json"
	"testing"
)

func TestParseCriterionFunction(t *testing.T) {
	no := false
	cases := []struct {
		name    string
		in      string
		want    CriterionFunction
		wantErr bool
	}{
		{
			name: "full envelope",
			in: `{"type":"function","function":{"name":"min-amount","config":{
				"calculationNodesTags":"pricing,eu","attachEntity":false,
				"responseTimeoutMs":5000,"retryPolicy":"NONE","context":"role=a"}}}`,
			want: CriterionFunction{Name: "min-amount", Config: CriterionFunctionConfig{
				CalculationNodesTags: "pricing,eu", AttachEntity: &no,
				ResponseTimeoutMs: 5000, RetryPolicy: "NONE", Context: "role=a"}},
		},
		{
			name: "no config: every field at its zero value, attachEntity unset",
			in:   `{"type":"function","function":{"name":"f"}}`,
			want: CriterionFunction{Name: "f"},
		},
		{
			name: "unknown members are ignored; the criterion is client-owned JSON",
			in:   `{"type":"function","function":{"name":"f","calculationNodesTags":"legacy","config":{"x":1}}}`,
			want: CriterionFunction{Name: "f"},
		},
		{name: "responseTimeoutMs of the wrong type", in: `{"type":"function","function":{"name":"f","config":{"responseTimeoutMs":"5s"}}}`, wantErr: true},
		{name: "retryPolicy of the wrong type", in: `{"type":"function","function":{"name":"f","config":{"retryPolicy":3}}}`, wantErr: true},
		{name: "not JSON", in: `{`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseCriterionFunction(json.RawMessage(tc.in))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseCriterionFunction = %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCriterionFunction: %v", err)
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
