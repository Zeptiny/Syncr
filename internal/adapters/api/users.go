package api

import (
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
)

// userHandler implements syncr.v1.UserService (admin only, see policy.go).
//
// m3: pending track A: every RPC is Unimplemented until then.
type userHandler struct {
	syncrv1connect.UnimplementedUserServiceHandler

	users UserAdminService
	errs  errorMapper
}

var _ syncrv1connect.UserServiceHandler = (*userHandler)(nil)
