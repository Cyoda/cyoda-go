package auth_test

import (
	"context"
	"slices"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// TestValidator_Mapping pins the claims-to-principal mapping: which claims
// make an on-behalf-of user, a client, or the operator, which claims put a
// client-token marker beside the principal, and which claim combinations are
// refused so that every accepted token maps to exactly one row.
func TestValidator_Mapping(t *testing.T) {
	key := generateTestKey(t)
	v := auth.NewValidatorFromSource(staticKeySource{"k1": &key.PublicKey}, "iss")
	base := func(extra map[string]any) map[string]any {
		c := map[string]any{"iss": "iss", "sub": "x", "caas_user_id": "alice",
			"caas_org_id": "acme", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()}
		for k, val := range extra {
			c[k] = val
		}
		return c
	}
	cases := []struct {
		name       string
		claims     map[string]any
		kind       spi.PrincipalKind
		roles      []string
		executor   *spi.Principal
		marker     *contract.ClientToken
		wantRefuse bool
	}{
		{"obo", base(map[string]any{"act": map[string]any{"sub": "OBOCLIENT0000001"}, "scopes": []string{"ROLE_M2M"}}),
			spi.PrincipalUser, []string{"ROLE_M2M"}, &spi.Principal{ID: "OBOCLIENT0000001", Kind: spi.PrincipalService}, nil, false},
		{"obo ignores cgen", base(map[string]any{"act": map[string]any{"sub": "OBOCLIENT0000001"}, "scopes": []string{"ROLE_M2M"}, "cgen": 2}),
			spi.PrincipalUser, []string{"ROLE_M2M"}, &spi.Principal{ID: "OBOCLIENT0000001", Kind: spi.PrincipalService}, nil, false},
		{"client with cgen", base(map[string]any{"caas_user_id": "CLIENT0000000001", "scopes": []string{"ROLE_M2M"}, "cgen": 3}),
			spi.PrincipalService, []string{"ROLE_M2M"}, nil, &contract.ClientToken{ClientID: "CLIENT0000000001", Gen: 3}, false},
		{"client with cgen zero", base(map[string]any{"caas_user_id": "CLIENT0000000001", "scopes": []string{"ROLE_M2M"}, "cgen": 0}),
			spi.PrincipalService, []string{"ROLE_M2M"}, nil, &contract.ClientToken{ClientID: "CLIENT0000000001", Gen: 0}, false},
		{"client without cgen", base(map[string]any{"scopes": []string{"ROLE_M2M"}}),
			spi.PrincipalService, []string{"ROLE_M2M"}, nil, nil, false},
		{"client with empty scopes", base(map[string]any{"scopes": []string{}}),
			spi.PrincipalService, []string{}, nil, nil, false},
		{"operator", base(map[string]any{"user_roles": []string{"ROLE_ADMIN"}}),
			spi.PrincipalUser, []string{"ROLE_ADMIN"}, nil, nil, false},
		{"operator with empty user_roles", base(map[string]any{"user_roles": []string{}}),
			spi.PrincipalUser, []string{}, nil, nil, false},
		{"neither scopes nor user_roles", base(nil),
			spi.PrincipalUser, []string{}, nil, nil, false},
		{"act without sub", base(map[string]any{"act": map[string]any{}, "scopes": []string{"ROLE_M2M"}}), "", nil, nil, nil, true},
		{"act sub empty", base(map[string]any{"act": map[string]any{"sub": ""}, "scopes": []string{"ROLE_M2M"}}), "", nil, nil, nil, true},
		{"act sub not a string", base(map[string]any{"act": map[string]any{"sub": 7}, "scopes": []string{"ROLE_M2M"}}), "", nil, nil, nil, true},
		{"act not object", base(map[string]any{"act": "OBOCLIENT0000001", "scopes": []string{"ROLE_M2M"}}), "", nil, nil, nil, true},
		{"act null", base(map[string]any{"act": nil, "scopes": []string{"ROLE_M2M"}}), "", nil, nil, nil, true},
		{"act without scopes", base(map[string]any{"act": map[string]any{"sub": "OBOCLIENT0000001"}}), "", nil, nil, nil, true},
		{"act with user_roles", base(map[string]any{"act": map[string]any{"sub": "OBOCLIENT0000001"}, "scopes": []string{"ROLE_M2M"}, "user_roles": []string{"ROLE_M2M"}}), "", nil, nil, nil, true},
		{"scopes with user_roles", base(map[string]any{"scopes": []string{"ROLE_M2M"}, "user_roles": []string{"ROLE_ADMIN"}}), "", nil, nil, nil, true},
		{"cgen negative", base(map[string]any{"scopes": []string{"ROLE_M2M"}, "cgen": -1}), "", nil, nil, nil, true},
		{"cgen fractional", base(map[string]any{"scopes": []string{"ROLE_M2M"}, "cgen": 1.5}), "", nil, nil, nil, true},
		{"cgen string", base(map[string]any{"scopes": []string{"ROLE_M2M"}, "cgen": "3"}), "", nil, nil, nil, true},
		{"cgen beyond exact float range", base(map[string]any{"scopes": []string{"ROLE_M2M"}, "cgen": float64(1<<53) * 2}), "", nil, nil, nil, true},
		{"cgen 2^53", base(map[string]any{"scopes": []string{"ROLE_M2M"}, "cgen": float64(1 << 53)}), "", nil, nil, nil, true},
		{"cgen 2^53-1", base(map[string]any{"caas_user_id": "CLIENT0000000001", "scopes": []string{"ROLE_M2M"}, "cgen": float64(1<<53 - 1)}),
			spi.PrincipalService, []string{"ROLE_M2M"}, nil, &contract.ClientToken{ClientID: "CLIENT0000000001", Gen: 1<<53 - 1}, false},
		{"cgen with caas_user_id not a client id", base(map[string]any{"caas_user_id": "alice@example", "scopes": []string{"ROLE_M2M"}, "cgen": 1}), "", nil, nil, nil, true},
		{"scopes a string", base(map[string]any{"scopes": "ROLE_M2M"}), "", nil, nil, nil, true},
		{"scopes a number", base(map[string]any{"scopes": 1}), "", nil, nil, nil, true},
		{"scopes null", base(map[string]any{"scopes": nil}), "", nil, nil, nil, true},
		{"scopes with a non-string item", base(map[string]any{"scopes": []any{"ROLE_M2M", 1}}), "", nil, nil, nil, true},
		{"obo scopes a string", base(map[string]any{"act": map[string]any{"sub": "OBOCLIENT0000001"}, "scopes": "ROLE_M2M"}), "", nil, nil, nil, true},
		{"user_roles a string", base(map[string]any{"user_roles": "ROLE_ADMIN"}), "", nil, nil, nil, true},
		{"user_roles a number", base(map[string]any{"user_roles": 1}), "", nil, nil, nil, true},
		{"user_roles with a non-string item", base(map[string]any{"user_roles": []any{"ROLE_ADMIN", true}}), "", nil, nil, nil, true},
		{"act sub not a client id", base(map[string]any{"act": map[string]any{"sub": "not a client"}, "scopes": []string{"ROLE_M2M"}}), "", nil, nil, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok, err := auth.Sign(context.Background(), tc.claims, auth.NewRSASigner(key), "k1")
			if err != nil {
				t.Fatal(err)
			}
			uc, marker, err := v.Validate(tok)
			if tc.wantRefuse {
				if err == nil {
					t.Fatalf("accepted %v", tc.claims)
				}
				if uc != nil || marker != nil {
					t.Fatalf("refusal returned uc=%+v marker=%+v", uc, marker)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if uc.Kind != tc.kind || !slices.Equal(uc.Roles, tc.roles) {
				t.Fatalf("uc = %+v", uc)
			}
			if (uc.Executor == nil) != (tc.executor == nil) || (uc.Executor != nil && *uc.Executor != *tc.executor) {
				t.Fatalf("executor = %+v, want %+v", uc.Executor, tc.executor)
			}
			if (marker == nil) != (tc.marker == nil) || (marker != nil && *marker != *tc.marker) {
				t.Fatalf("marker = %+v, want %+v", marker, tc.marker)
			}
		})
	}
}
