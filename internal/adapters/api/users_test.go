package api

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

func TestUserAdministration(t *testing.T) {
	t.Parallel()
	e := newIdentityEnv(t, identityOptions{})
	admin := e.completeSetup()
	uc := e.userClient(admin)

	created := e.addUser(admin, "alice", syncrv1.UserRole_USER_ROLE_USER)
	if created.GetUsername() != "alice" || created.GetRole() != syncrv1.UserRole_USER_ROLE_USER || created.GetDisabled() ||
		created.GetVersion() != 1 || !created.GetCreatedAt().IsValid() || created.GetLastLoginAt() != nil {
		t.Errorf("CreateUser = %v", created)
	}
	_, err := uc.CreateUser(t.Context(), connect.NewRequest(&syncrv1.CreateUserRequest{Username: "alice", Password: idPassword, Role: syncrv1.UserRole_USER_ROLE_USER}))
	wantCode(t, err, connect.CodeAlreadyExists)
	_, err = uc.CreateUser(t.Context(), connect.NewRequest(&syncrv1.CreateUserRequest{Username: "Bob", Password: "short"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	var fields []string
	for _, v := range violations(t, err) {
		fields = append(fields, v.GetField())
	}
	if strings.Join(fields, ",") != "username,password,role" {
		t.Errorf("violations on %v", fields)
	}
	_, err = uc.CreateUser(t.Context(), connect.NewRequest(&syncrv1.CreateUserRequest{Username: "bob", Password: idPassword, Role: syncrv1.UserRole(42)}))
	wantCode(t, err, connect.CodeInvalidArgument)

	list, err := uc.ListUsers(t.Context(), connect.NewRequest(&syncrv1.ListUsersRequest{}))
	if err != nil || len(list.Msg.GetUsers()) != 2 || list.Msg.GetUsers()[0].GetUsername() != "admin" || list.Msg.GetUsers()[1].GetUsername() != "alice" {
		t.Fatalf("ListUsers = (%v, %v)", list, err)
	}
	got, err := uc.GetUser(t.Context(), connect.NewRequest(&syncrv1.GetUserRequest{UserId: created.GetId()}))
	if err != nil || !proto.Equal(got.Msg.GetUser(), created) {
		t.Errorf("GetUser = (%v, %v)", got, err)
	}
	for _, id := range []string{"", "nope", "00000000-0000-0000-0000-000000000000"} {
		_, err := uc.GetUser(t.Context(), connect.NewRequest(&syncrv1.GetUserRequest{UserId: id}))
		wantCode(t, err, connect.CodeInvalidArgument)
		if v := violations(t, err); len(v) != 1 || v[0].GetField() != "user_id" {
			t.Errorf("GetUser(%q) violations = %v", id, v)
		}
	}
	_, err = uc.GetUser(t.Context(), connect.NewRequest(&syncrv1.GetUserRequest{UserId: "00000000-0000-7000-8000-0000000000ff"}))
	wantCode(t, err, connect.CodeNotFound)

	// Update: unset fields keep their value, the version guards the change.
	alice := e.login("alice", idPassword)
	cur, _ := uc.GetUser(t.Context(), connect.NewRequest(&syncrv1.GetUserRequest{UserId: created.GetId()}))
	version := cur.Msg.GetUser().GetVersion()
	_, err = uc.UpdateUser(t.Context(), connect.NewRequest(&syncrv1.UpdateUserRequest{UserId: created.GetId(), Version: version - 1, DisplayName: proto.String("x")}))
	wantCode(t, err, connect.CodeAborted)
	upd, err := uc.UpdateUser(t.Context(), connect.NewRequest(&syncrv1.UpdateUserRequest{UserId: created.GetId(), Version: version, DisplayName: proto.String("Alice")}))
	if err != nil || upd.Msg.GetUser().GetDisplayName() != "Alice" || upd.Msg.GetUser().GetRole() != syncrv1.UserRole_USER_ROLE_USER {
		t.Fatalf("UpdateUser(display name) = (%v, %v)", upd, err)
	}
	if _, err := e.getSession(alice); err != nil {
		t.Errorf("a display name change ended the session: %v", err)
	}
	upd, err = uc.UpdateUser(t.Context(), connect.NewRequest(&syncrv1.UpdateUserRequest{
		UserId: created.GetId(), Version: upd.Msg.GetUser().GetVersion(), Disabled: proto.Bool(true),
	}))
	if err != nil || !upd.Msg.GetUser().GetDisabled() || upd.Msg.GetUser().GetDisplayName() != "Alice" {
		t.Fatalf("UpdateUser(disable) = (%v, %v)", upd, err)
	}
	_, err = e.getSession(alice)
	wantCode(t, err, connect.CodeUnauthenticated)
	_, err = e.authClient(creds{csrf: true}).Login(t.Context(), connect.NewRequest(&syncrv1.LoginRequest{Username: "alice", Password: idPassword}))
	wantCode(t, err, connect.CodeUnauthenticated)
	upd, err = uc.UpdateUser(t.Context(), connect.NewRequest(&syncrv1.UpdateUserRequest{
		UserId: created.GetId(), Version: upd.Msg.GetUser().GetVersion(), Disabled: proto.Bool(false), Role: syncrv1.UserRole_USER_ROLE_ADMIN,
	}))
	if err != nil || upd.Msg.GetUser().GetDisabled() || upd.Msg.GetUser().GetRole() != syncrv1.UserRole_USER_ROLE_ADMIN {
		t.Fatalf("UpdateUser(enable, promote) = (%v, %v)", upd, err)
	}

	// Reset the password: the old one stops working, sessions end.
	alice = e.login("alice", idPassword)
	reset, err := uc.ResetPassword(t.Context(), connect.NewRequest(&syncrv1.ResetPasswordRequest{UserId: created.GetId(), NewPassword: idPassword2}))
	if err != nil || reset.Msg.GetUser().GetId() != created.GetId() {
		t.Fatalf("ResetPassword = (%v, %v)", reset, err)
	}
	_, err = e.getSession(alice)
	wantCode(t, err, connect.CodeUnauthenticated)
	e.login("alice", idPassword2)
	_, err = uc.ResetPassword(t.Context(), connect.NewRequest(&syncrv1.ResetPasswordRequest{UserId: created.GetId(), NewPassword: "short"}))
	wantCode(t, err, connect.CodeInvalidArgument)

	if _, err := uc.DeleteUser(t.Context(), connect.NewRequest(&syncrv1.DeleteUserRequest{UserId: created.GetId()})); err != nil {
		t.Fatal(err)
	}
	_, err = uc.DeleteUser(t.Context(), connect.NewRequest(&syncrv1.DeleteUserRequest{UserId: created.GetId()}))
	wantCode(t, err, connect.CodeNotFound)
}

func TestLastAdminThroughTheAPI(t *testing.T) {
	t.Parallel()
	e := newIdentityEnv(t, identityOptions{})
	admin := e.completeSetup()
	me, err := e.getSession(admin)
	if err != nil {
		t.Fatal(err)
	}
	u := me.GetUser()
	uc := e.userClient(admin)
	_, err = uc.UpdateUser(t.Context(), connect.NewRequest(&syncrv1.UpdateUserRequest{UserId: u.GetId(), Version: u.GetVersion(), Role: syncrv1.UserRole_USER_ROLE_USER}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	if err != nil && !strings.Contains(err.Error(), "last active admin") {
		t.Errorf("the message does not say why: %v", err)
	}
	_, err = uc.UpdateUser(t.Context(), connect.NewRequest(&syncrv1.UpdateUserRequest{UserId: u.GetId(), Version: u.GetVersion(), Disabled: proto.Bool(true)}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	_, err = uc.DeleteUser(t.Context(), connect.NewRequest(&syncrv1.DeleteUserRequest{UserId: u.GetId()}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	if _, err := e.getSession(admin); err != nil {
		t.Errorf("refused changes ended the admin's session: %v", err)
	}
}

func TestUserServiceNeedsAnAdmin(t *testing.T) {
	t.Parallel()
	e := newIdentityEnv(t, identityOptions{})
	admin := e.completeSetup()
	target := e.addUser(admin, "alice", syncrv1.UserRole_USER_ROLE_USER)
	e.addUser(admin, "bob", syncrv1.UserRole_USER_ROLE_USER)
	bob := e.login("bob", idPassword)
	created, err := e.authClient(bob).CreateAPIToken(t.Context(), connect.NewRequest(&syncrv1.CreateAPITokenRequest{Name: "bob's"}))
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]creds{"session": bob, "api token": bearer(created.Msg.GetSecret())} {
		uc := e.userClient(c)
		for rpc, call := range map[string]func() error{
			"ListUsers": func() error {
				_, err := uc.ListUsers(t.Context(), connect.NewRequest(&syncrv1.ListUsersRequest{}))
				return err
			},
			"GetUser": func() error {
				_, err := uc.GetUser(t.Context(), connect.NewRequest(&syncrv1.GetUserRequest{UserId: target.GetId()}))
				return err
			},
			"CreateUser": func() error {
				_, err := uc.CreateUser(t.Context(), connect.NewRequest(&syncrv1.CreateUserRequest{Username: "eve", Password: idPassword, Role: syncrv1.UserRole_USER_ROLE_ADMIN}))
				return err
			},
			"UpdateUser": func() error {
				_, err := uc.UpdateUser(t.Context(), connect.NewRequest(&syncrv1.UpdateUserRequest{UserId: target.GetId(), Version: 1, Role: syncrv1.UserRole_USER_ROLE_ADMIN}))
				return err
			},
			"ResetPassword": func() error {
				_, err := uc.ResetPassword(t.Context(), connect.NewRequest(&syncrv1.ResetPasswordRequest{UserId: target.GetId(), NewPassword: idPassword2}))
				return err
			},
			"DeleteUser": func() error {
				_, err := uc.DeleteUser(t.Context(), connect.NewRequest(&syncrv1.DeleteUserRequest{UserId: target.GetId()}))
				return err
			},
		} {
			if code := codeOf(t, call()); code != connect.CodePermissionDenied {
				t.Errorf("%s with a user's %s: %v, want permission denied", rpc, name, code)
			}
		}
	}
	// Nothing changed.
	got, err := e.userClient(admin).GetUser(t.Context(), connect.NewRequest(&syncrv1.GetUserRequest{UserId: target.GetId()}))
	if err != nil || got.Msg.GetUser().GetVersion() != 1 || got.Msg.GetUser().GetRole() != syncrv1.UserRole_USER_ROLE_USER {
		t.Errorf("the target changed: (%v, %v)", got, err)
	}
}
