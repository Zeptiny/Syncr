package notify_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Zeptiny/Syncr/internal/notify"
)

func TestSendErrorVisible(t *testing.T) {
	t.Parallel()
	var reply *notify.SendError
	if !errors.As(notify.Permanent(404, "no route"), &reply) {
		t.Fatal("not a SendError")
	}
	if st, r := reply.Visible(true); st != 404 || r != "no route" {
		t.Errorf("Visible(admin) = (%d, %q)", st, r)
	}
	if st, r := reply.Visible(false); st != 0 || r != notify.GenericReason {
		t.Errorf("Visible(user) = (%d, %q)", st, r)
	}

	var own *notify.SendError
	if !errors.As(notify.Public(fmt.Errorf("wrapped: %w", notify.Permanent(0, "the configuration is not valid"))), &own) || !own.Public {
		t.Fatalf("Public did not mark the error: %+v", own)
	}
	if st, r := own.Visible(false); st != 0 || r != "the configuration is not valid" {
		t.Errorf("Visible(user, public) = (%d, %q)", st, r)
	}
	if reply.Public {
		t.Error("Public changed the original error")
	}

	plain := errors.New("plain")
	if got := notify.Public(plain); !errors.Is(got, plain) {
		t.Errorf("Public(non-SendError) = %v", got)
	}
}
