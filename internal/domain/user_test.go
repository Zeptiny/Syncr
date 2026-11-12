package domain

import (
	"errors"
	"testing"
)

func TestParseRole(t *testing.T) {
	t.Parallel()
	for _, r := range []Role{RoleAdmin, RoleUser} {
		got, err := ParseRole(string(r))
		if err != nil || got != r {
			t.Errorf("ParseRole(%q) = (%q, %v)", r, got, err)
		}
	}
	for _, bad := range []string{"", "ADMIN", "root"} {
		if _, err := ParseRole(bad); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("ParseRole(%q) err = %v, want ErrInvalidArgument", bad, err)
		}
	}
}

func TestActor(t *testing.T) {
	t.Parallel()
	alice, bob := NewUserID(), NewUserID()
	user := Actor{UserID: alice, Role: RoleUser}
	admin := Actor{UserID: bob, Role: RoleAdmin}
	nobody := Actor{}

	tests := []struct {
		name          string
		actor         Actor
		owner         UserID
		owns, canView bool
	}{
		{"own object", user, alice, true, true},
		{"other's object", user, bob, false, false},
		{"admin, other's object", admin, alice, false, true},
		{"admin, own object", admin, bob, true, true},
		{"zero actor, zero owner", nobody, UserID{}, false, false},
		{"user, zero owner", user, UserID{}, false, false},
	}
	for _, tt := range tests {
		if got := tt.actor.Owns(tt.owner); got != tt.owns {
			t.Errorf("%s: Owns = %v, want %v", tt.name, got, tt.owns)
		}
		if got := tt.actor.CanView(tt.owner); got != tt.canView {
			t.Errorf("%s: CanView = %v, want %v", tt.name, got, tt.canView)
		}
	}
	if user.IsAdmin() || !admin.IsAdmin() || nobody.IsAdmin() {
		t.Error("IsAdmin is wrong")
	}
}
