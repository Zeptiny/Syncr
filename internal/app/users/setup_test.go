package users_test

import (
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/domain"
)

func setupParams(code string) users.SetupParams {
	return users.SetupParams{Code: code, Username: "admin", DisplayName: "The Admin", Password: goodPass, ClientIP: testIP, UserAgent: testAgent}
}

func TestNewSetupCode(t *testing.T) {
	t.Parallel()
	re := regexp.MustCompile(`^[A-Z2-7]{5}-[A-Z2-7]{5}-[A-Z2-7]{5}-[A-Z2-7]{5}$`)
	seen := map[string]bool{}
	for range 50 {
		code, err := users.NewSetupCode(randReader())
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(code) {
			t.Fatalf("code %q does not have the XXXXX-XXXXX-XXXXX-XXXXX shape", code)
		}
		if seen[code] {
			t.Fatalf("code %q repeated", code)
		}
		seen[code] = true
	}
	if _, err := users.NewSetupCode(strings.NewReader("short")); err == nil {
		t.Error("NewSetupCode succeeded without enough randomness")
	}
}

func TestArmSetupRejectsShortCodes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const short = "ABCD-EFGH-IJKL-MNO" // 15 significant characters
	err := e.svc.ArmSetup(short)
	if err == nil {
		t.Fatal("ArmSetup accepted a 15-character code")
	}
	if strings.Contains(err.Error(), "EFGH") {
		t.Errorf("the error contains the code: %v", err)
	}
	if err := e.svc.ArmSetup("abcd efgh ijkl mnop"); err != nil {
		t.Errorf("ArmSetup(16 characters with spaces): %v", err)
	}
}

// A preset code may hold any characters, be longer than a generated one,
// and be entered in another case or with other dashes and spaces.
func TestCompleteSetupPresetCode(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.svc.ArmSetup("correct-horse-battery-staple-01"); err != nil {
		t.Fatal(err)
	}
	for _, wrong := range []string{"CORRE-CTHOR-SEBAT-TERYS", "correct-horse-battery-staple-02", "correct-horse-battery-staple"} {
		_, _, err := e.svc.CompleteSetup(e.ctx(), setupParams(wrong))
		wantFields(t, "CompleteSetup("+wrong+")", err, "setup_code")
	}
	if _, _, err := e.svc.CompleteSetup(e.ctx(), setupParams(" Correct Horse-Battery-STAPLE-01 ")); err != nil {
		t.Fatalf("CompleteSetup(preset code, other case and separators): %v", err)
	}
}

func TestCompleteSetup(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if req, err := e.svc.SetupRequired(e.ctx()); err != nil || !req {
		t.Fatalf("SetupRequired = (%v, %v), want true", req, err)
	}

	_, _, err := e.svc.CompleteSetup(e.ctx(), setupParams(setupCode))
	wantErr(t, "CompleteSetup(nothing armed)", err, domain.ErrFailedPrecondition)

	if err := e.svc.ArmSetup(setupCode); err != nil {
		t.Fatal(err)
	}
	_, _, err = e.svc.CompleteSetup(e.ctx(), setupParams("ABCDE-FGHIJ-KLMNO-PQRS3"))
	wantFields(t, "CompleteSetup(wrong code)", err, "setup_code")
	if err != nil && strings.Contains(err.Error(), "PQRS") {
		t.Errorf("the error contains a code: %v", err)
	}
	bad := setupParams(setupCode)
	bad.Username, bad.Password = "Admin", "short"
	_, _, err = e.svc.CompleteSetup(e.ctx(), bad)
	wantFields(t, "CompleteSetup(invalid fields)", err, "username", "password")
	bad.Code = "wrong"
	_, _, err = e.svc.CompleteSetup(e.ctx(), bad)
	wantFields(t, "CompleteSetup(everything wrong)", err, "setup_code", "username", "password")
	if n, _ := e.store.CountUsers(e.ctx()); n != 0 {
		t.Fatalf("refused setups created %d users", n)
	}

	// Dashes, spaces and case are ignored.
	u, s, err := e.svc.CompleteSetup(e.ctx(), setupParams(" abcdefghij klmno-pqrs2 "))
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "admin" || u.Role != domain.RoleAdmin || u.DisplayName != "The Admin" || u.Version != 1 || !u.LastLoginAt.Equal(t0()) {
		t.Errorf("first user = %+v", u)
	}
	if s.Token == "" || !s.ExpiresAt.Equal(t0().Add(30*day)) || s.MaxAge != 30*day {
		t.Errorf("issued session = %+v", s)
	}
	actor, sess, err := e.svc.AuthenticateSession(e.ctx(), s.Token)
	if err != nil || actor != u.Actor() || sess.ClientIP != testIP || sess.UserAgent != testAgent {
		t.Errorf("AuthenticateSession(setup session) = (%+v, %+v, %v)", actor, sess, err)
	}
	if req, err := e.svc.SetupRequired(e.ctx()); err != nil || req {
		t.Errorf("SetupRequired after setup = (%v, %v)", req, err)
	}

	// The code is spent, and setup is over for good.
	again := setupParams(setupCode)
	again.Username = "intruder"
	_, _, err = e.svc.CompleteSetup(e.ctx(), again)
	wantErr(t, "CompleteSetup(again)", err, domain.ErrFailedPrecondition)
	if err := e.svc.ArmSetup(setupCode); err != nil {
		t.Fatal(err)
	}
	_, _, err = e.svc.CompleteSetup(e.ctx(), again)
	wantErr(t, "CompleteSetup(re-armed)", err, domain.ErrFailedPrecondition)
	if n, _ := e.store.CountUsers(e.ctx()); n != 1 {
		t.Errorf("%d users, want 1", n)
	}
}

func TestSetupRequiredSeesExistingUsers(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addUser("someone", domain.RoleAdmin)
	if req, err := e.svc.SetupRequired(e.ctx()); err != nil || req {
		t.Errorf("SetupRequired = (%v, %v), want false", req, err)
	}
	if err := e.svc.ArmSetup(setupCode); err != nil {
		t.Fatal(err)
	}
	_, _, err := e.svc.CompleteSetup(e.ctx(), setupParams(setupCode))
	wantErr(t, "CompleteSetup(users exist)", err, domain.ErrFailedPrecondition)
}

func TestSetupRateLimits(t *testing.T) {
	t.Parallel()
	t.Run("global", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		if err := e.svc.ArmSetup(setupCode); err != nil {
			t.Fatal(err)
		}
		e.setupLim.setBudget(2)
		for i := range 2 {
			p := setupParams("WRONG-WRONG-WRONG-WRONG")
			p.ClientIP = "198.51.100." + string(rune('1'+i))
			_, _, err := e.svc.CompleteSetup(e.ctx(), p)
			wantFields(t, "wrong code", err, "setup_code")
		}
		// Even the right code from a new address is refused now.
		p := setupParams(setupCode)
		p.ClientIP = "203.0.113.9"
		_, _, err := e.svc.CompleteSetup(e.ctx(), p)
		wantErr(t, "CompleteSetup(over the global limit)", err, domain.ErrRateLimited)
		if keys := e.setupLim.seen(); len(keys) != 3 || keys[0] != keys[2] {
			t.Errorf("global limiter keys = %q, want one key", keys)
		}
	})
	t.Run("per client ip", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		if err := e.svc.ArmSetup(setupCode); err != nil {
			t.Fatal(err)
		}
		e.ipLim.setBudget(1)
		_, _, err := e.svc.CompleteSetup(e.ctx(), setupParams("WRONG-WRONG-WRONG-WRONG"))
		wantFields(t, "wrong code", err, "setup_code")
		_, _, err = e.svc.CompleteSetup(e.ctx(), setupParams(setupCode))
		wantErr(t, "CompleteSetup(over the ip limit)", err, domain.ErrRateLimited)
		other := setupParams(setupCode)
		other.ClientIP = "203.0.113.9"
		if _, _, err := e.svc.CompleteSetup(e.ctx(), other); err != nil {
			t.Errorf("another address: %v", err)
		}
	})
}

func TestSetupRace(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.svc.ArmSetup(setupCode); err != nil {
		t.Fatal(err)
	}
	const n = 20
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			p := setupParams(setupCode)
			p.Username = "admin" + string(rune('a'+i))
			_, _, errs[i] = e.svc.CompleteSetup(e.ctx(), p)
		})
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, domain.ErrFailedPrecondition):
			t.Errorf("CompleteSetup: %v", err)
		}
	}
	if wins != 1 {
		t.Errorf("%d concurrent setups succeeded, want 1", wins)
	}
	if c, _ := e.store.CountUsers(e.ctx()); c != 1 {
		t.Errorf("%d users after the race, want 1", c)
	}
}
