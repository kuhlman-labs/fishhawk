package account

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
)

// fakeRoleReader records the params it was called with and returns a
// programmed (role, err). rows==false makes it return pgx.ErrNoRows (the
// no-membership case).
type fakeRoleReader struct {
	role    *string
	rows    bool
	lastArg accountdb.GetAccountMemberRoleParams
}

func (f *fakeRoleReader) GetAccountMemberRole(_ context.Context, arg accountdb.GetAccountMemberRoleParams) (*string, error) {
	f.lastArg = arg
	if !f.rows {
		return nil, pgx.ErrNoRows
	}
	return f.role, nil
}

func strptr(s string) *string { return &s }

func TestMemberRole_StripsProviderPrefixGenerically(t *testing.T) {
	acct := uuid.New()
	cases := []struct {
		name         string
		provider     string
		subject      string
		wantRef      string
		role         *string
		wantResolved string
	}{
		{
			name:         "github admin",
			provider:     "github",
			subject:      "github:octocat",
			wantRef:      "octocat",
			role:         strptr(RoleAdmin),
			wantResolved: RoleAdmin,
		},
		{
			// The forge-agnostic case the plan requires: a non-github
			// subject must resolve its member_ref via the generic
			// "<provider>:" strip, NOT a hard-coded "github:" literal.
			name:         "gitlab member",
			provider:     "gitlab",
			subject:      "gitlab:some.user",
			wantRef:      "some.user",
			role:         strptr(RoleMember),
			wantResolved: RoleMember,
		},
		{
			name:         "null role maps to member-tier",
			provider:     "github",
			subject:      "github:nobody",
			wantRef:      "nobody",
			role:         nil,
			wantResolved: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fr := &fakeRoleReader{role: tc.role, rows: true}
			s := NewStore(fr)
			got, err := s.MemberRole(context.Background(), acct.String(), tc.provider, tc.subject)
			if err != nil {
				t.Fatalf("MemberRole error: %v", err)
			}
			if got != tc.wantResolved {
				t.Errorf("role = %q, want %q", got, tc.wantResolved)
			}
			if fr.lastArg.MemberRef != tc.wantRef {
				t.Errorf("member_ref = %q, want %q (generic prefix strip)", fr.lastArg.MemberRef, tc.wantRef)
			}
			if fr.lastArg.Provider != tc.provider {
				t.Errorf("provider = %q, want %q", fr.lastArg.Provider, tc.provider)
			}
			if fr.lastArg.AccountID != acct {
				t.Errorf("account_id = %v, want %v", fr.lastArg.AccountID, acct)
			}
		})
	}
}

func TestMemberRole_NoMembershipIsMemberTier(t *testing.T) {
	fr := &fakeRoleReader{rows: false} // pgx.ErrNoRows
	s := NewStore(fr)
	got, err := s.MemberRole(context.Background(), uuid.New().String(), "github", "github:stranger")
	if err != nil {
		t.Fatalf("MemberRole error: %v", err)
	}
	if got != "" {
		t.Errorf("role = %q, want \"\" (no membership -> member-tier)", got)
	}
}

func TestMemberRole_DefensiveInputs(t *testing.T) {
	s := NewStore(&fakeRoleReader{rows: true, role: strptr(RoleAdmin)})
	// Empty account id, empty provider, and a non-uuid account id all return
	// "" WITHOUT consulting the reader (defensive — the caller supplies a
	// resolved Identity).
	for _, tc := range []struct{ acct, provider string }{
		{"", "github"},
		{uuid.New().String(), ""},
		{"not-a-uuid", "github"},
	} {
		got, err := s.MemberRole(context.Background(), tc.acct, tc.provider, "github:x")
		if err != nil || got != "" {
			t.Errorf("MemberRole(%q,%q) = (%q,%v), want (\"\",nil)", tc.acct, tc.provider, got, err)
		}
	}
}

// A nil Store / nil reader is the untenanted-allow posture (no DB wired): it
// returns member-tier without panicking.
func TestMemberRole_NilStoreAllows(t *testing.T) {
	var s *Store
	got, err := s.MemberRole(context.Background(), uuid.New().String(), "github", "github:x")
	if err != nil || got != "" {
		t.Errorf("nil Store MemberRole = (%q,%v), want (\"\",nil)", got, err)
	}
	got, err = NewStore(nil).MemberRole(context.Background(), uuid.New().String(), "github", "github:x")
	if err != nil || got != "" {
		t.Errorf("nil-reader Store MemberRole = (%q,%v), want (\"\",nil)", got, err)
	}
}
