package api

import (
	"context"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// userHandler implements syncr.v1.UserService (admin only, see policy.go;
// the use cases check the role too).
type userHandler struct {
	syncrv1connect.UnimplementedUserServiceHandler

	users UserAdminService
	errs  errorMapper
}

var _ syncrv1connect.UserServiceHandler = (*userHandler)(nil)

func (h *userHandler) fail(ctx context.Context, procedure string, err error) error {
	return h.errs.toConnect(ctx, procedure, "user", err)
}

// ListUsers lists every user, by username.
func (h *userHandler) ListUsers(ctx context.Context, _ *connect.Request[syncrv1.ListUsersRequest]) (*connect.Response[syncrv1.ListUsersResponse], error) {
	actor, _ := ActorFrom(ctx)
	us, err := h.users.List(ctx, actor)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.UserServiceListUsersProcedure, err)
	}
	resp := &syncrv1.ListUsersResponse{Users: make([]*syncrv1.User, 0, len(us))}
	for _, u := range us {
		resp.Users = append(resp.Users, userToProto(u))
	}
	return connect.NewResponse(resp), nil
}

// GetUser returns one user.
func (h *userHandler) GetUser(ctx context.Context, req *connect.Request[syncrv1.GetUserRequest]) (*connect.Response[syncrv1.GetUserResponse], error) {
	id, err := parseUserID(req.Msg.GetUserId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	u, err := h.users.Get(ctx, actor, id)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.UserServiceGetUserProcedure, err)
	}
	return connect.NewResponse(&syncrv1.GetUserResponse{User: userToProto(u)}), nil
}

// CreateUser creates an account.
func (h *userHandler) CreateUser(ctx context.Context, req *connect.Request[syncrv1.CreateUserRequest]) (*connect.Response[syncrv1.CreateUserResponse], error) {
	m := req.Msg
	role, ok := roleFromProto(m.GetRole())
	if !ok {
		return nil, invalidField("role", "is not a known role")
	}
	actor, _ := ActorFrom(ctx)
	u, err := h.users.Create(ctx, actor, users.CreateParams{
		Username: m.GetUsername(), DisplayName: m.GetDisplayName(), Password: m.GetPassword(), Role: role,
	})
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.UserServiceCreateUserProcedure, err)
	}
	return connect.NewResponse(&syncrv1.CreateUserResponse{User: userToProto(u)}), nil
}

// UpdateUser changes a user's display name, role or disabled state.
func (h *userHandler) UpdateUser(ctx context.Context, req *connect.Request[syncrv1.UpdateUserRequest]) (*connect.Response[syncrv1.UpdateUserResponse], error) {
	m := req.Msg
	id, err := parseUserID(m.GetUserId())
	if err != nil {
		return nil, err
	}
	role, ok := roleFromProto(m.GetRole())
	if !ok {
		return nil, invalidField("role", "is not a known role")
	}
	actor, _ := ActorFrom(ctx)
	u, err := h.users.Update(ctx, actor, id, users.UpdateParams{
		Version: m.GetVersion(), DisplayName: m.DisplayName, Role: role, Disabled: m.Disabled,
	})
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.UserServiceUpdateUserProcedure, err)
	}
	return connect.NewResponse(&syncrv1.UpdateUserResponse{User: userToProto(u)}), nil
}

// ResetPassword sets a user's password and ends their sessions.
func (h *userHandler) ResetPassword(ctx context.Context, req *connect.Request[syncrv1.ResetPasswordRequest]) (*connect.Response[syncrv1.ResetPasswordResponse], error) {
	id, err := parseUserID(req.Msg.GetUserId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	u, err := h.users.ResetPassword(ctx, actor, id, req.Msg.GetNewPassword())
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.UserServiceResetPasswordProcedure, err)
	}
	return connect.NewResponse(&syncrv1.ResetPasswordResponse{User: userToProto(u)}), nil
}

// DeleteUser deletes a user who owns nothing.
func (h *userHandler) DeleteUser(ctx context.Context, req *connect.Request[syncrv1.DeleteUserRequest]) (*connect.Response[syncrv1.DeleteUserResponse], error) {
	id, err := parseUserID(req.Msg.GetUserId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	if err := h.users.Delete(ctx, actor, id); err != nil {
		return nil, h.fail(ctx, syncrv1connect.UserServiceDeleteUserProcedure, err)
	}
	return connect.NewResponse(&syncrv1.DeleteUserResponse{}), nil
}

func parseUserID(s string) (domain.UserID, error) {
	if s == "" {
		return domain.UserID{}, invalidField("user_id", "is required")
	}
	id, err := domain.ParseUserID(s)
	if err != nil || id.IsZero() {
		return domain.UserID{}, invalidField("user_id", "is not a valid ID")
	}
	return id, nil
}

// userToProto converts a user; the password hash never leaves.
func userToProto(u users.User) *syncrv1.User {
	return &syncrv1.User{
		Id: u.ID.String(), Username: u.Username, DisplayName: u.DisplayName, Role: roleToProto(u.Role),
		Disabled: u.Disabled(), CreatedAt: ts(u.CreatedAt), LastLoginAt: ts(u.LastLoginAt), Version: u.Version,
	}
}

func roleToProto(r domain.Role) syncrv1.UserRole {
	switch r {
	case domain.RoleAdmin:
		return syncrv1.UserRole_USER_ROLE_ADMIN
	case domain.RoleUser:
		return syncrv1.UserRole_USER_ROLE_USER
	default:
		return syncrv1.UserRole_USER_ROLE_UNSPECIFIED
	}
}

// roleFromProto maps UNSPECIFIED to "" (keep, or missing); ok is false for
// an unknown value.
func roleFromProto(r syncrv1.UserRole) (domain.Role, bool) {
	switch r {
	case syncrv1.UserRole_USER_ROLE_UNSPECIFIED:
		return "", true
	case syncrv1.UserRole_USER_ROLE_ADMIN:
		return domain.RoleAdmin, true
	case syncrv1.UserRole_USER_ROLE_USER:
		return domain.RoleUser, true
	default:
		return "", false
	}
}
