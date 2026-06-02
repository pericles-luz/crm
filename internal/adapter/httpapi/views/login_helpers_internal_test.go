package views

// SIN-63941 / UX-F4 — table-driven coverage for the loginTenantName,
// loginTenantLogo, and loginWhiteLabel FuncMap helpers. Each helper
// reads a single named field via reflection; the cases here pin every
// branch (nil, struct, pointer, interface wrapper, missing field,
// wrong type, non-struct) so a future refactor of stringFieldOnPageData
// cannot silently change the lookup contract for a single helper.

import (
	"testing"
)

type loginHelperStringCase struct {
	name string
	in   any
	want string
}

func TestLoginTenantName_TableDriven(t *testing.T) {
	t.Parallel()

	type withField struct {
		TenantName string
	}
	type otherType struct {
		TenantName int
	}
	type noField struct {
		Other string
	}

	cases := []loginHelperStringCase{
		{"nil", nil, ""},
		{"struct with string field", withField{TenantName: "Acme"}, "Acme"},
		{"pointer to struct", &withField{TenantName: "Acme"}, "Acme"},
		{"empty string field", withField{TenantName: ""}, ""},
		{"struct without field", noField{Other: "x"}, ""},
		{"struct with wrong field type", otherType{TenantName: 42}, ""},
		{"non-struct", "just a string", ""},
		{"nil pointer", (*withField)(nil), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := loginTenantName(tc.in); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoginTenantName_InterfaceWrapper(t *testing.T) {
	t.Parallel()
	type d struct{ TenantName string }
	var iface any = d{TenantName: "Acme"}
	if got := loginTenantName(iface); got != "Acme" {
		t.Fatalf("interface unwrap failed: %q", got)
	}
}

func TestLoginTenantLogo_TableDriven(t *testing.T) {
	t.Parallel()

	type withField struct {
		TenantLogo string
	}
	type otherType struct {
		TenantLogo []byte
	}

	cases := []loginHelperStringCase{
		{"nil", nil, ""},
		{"struct with string field", withField{TenantLogo: "https://static/t/1/logo"}, "https://static/t/1/logo"},
		{"pointer to struct", &withField{TenantLogo: "https://static/t/1/logo"}, "https://static/t/1/logo"},
		{"empty string field", withField{TenantLogo: ""}, ""},
		{"struct with wrong field type", otherType{TenantLogo: []byte("x")}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := loginTenantLogo(tc.in); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoginWhiteLabel_TableDriven(t *testing.T) {
	t.Parallel()

	type withField struct {
		WhiteLabel bool
	}
	type otherType struct {
		WhiteLabel string
	}
	type noField struct {
		Other string
	}

	cases := []struct {
		name string
		in   any
		want bool
	}{
		{"nil", nil, false},
		{"struct true", withField{WhiteLabel: true}, true},
		{"struct false", withField{WhiteLabel: false}, false},
		{"pointer true", &withField{WhiteLabel: true}, true},
		{"nil pointer", (*withField)(nil), false},
		{"missing field", noField{Other: "x"}, false},
		{"wrong field type", otherType{WhiteLabel: "true"}, false},
		{"non-struct", 42, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := loginWhiteLabel(tc.in); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoginWhiteLabel_InterfaceWrapper(t *testing.T) {
	t.Parallel()
	type d struct{ WhiteLabel bool }
	var iface any = d{WhiteLabel: true}
	if got := loginWhiteLabel(iface); !got {
		t.Fatalf("interface unwrap failed: got %v", got)
	}
}
