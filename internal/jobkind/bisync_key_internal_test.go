package jobkind

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

func testRemoteID(n int) domain.RemoteID {
	id, err := domain.ParseRemoteID(fmt.Sprintf("00000000-0000-7000-8000-%012x", 4000+n))
	if err != nil {
		panic(err)
	}
	return id
}

// TestBisyncStateInputHasNoSecrets: the bytes the state key hashes hold
// the sides' non-secret settings and never a secret, an obscured key's
// value or an option named like a secret, whatever remote (union members
// included) holds it.
func TestBisyncStateInputHasNoSecrets(t *testing.T) {
	t.Parallel()
	r2, sftp, disk, u := testRemoteID(1), testRemoteID(2), testRemoteID(3), testRemoteID(4)
	secrets := []string{"R2-SECRET-VALUE", "SFTP-PASSWORD-VALUE", "SESSION-TOKEN-VALUE", "MISFILED-SECRET-VALUE", "OBSCURED-OPTION-VALUE", "UNION-SECRET-VALUE"}
	remotes := map[domain.RemoteID]Remote{
		r2: {ID: r2, Rclone: backend.Rclone{
			Backend: "s3", Root: "shared-bucket",
			// "session_token" is also filed as an option by mistake: it
			// stays out because it is a secret's name.
			Options: map[string]string{"provider": "Cloudflare", "endpoint": "https://acct.r2.example", "access_key_id": "AKID", "session_token": "MISFILED-SECRET-VALUE"},
			Secrets: map[string]string{"secret_access_key": "R2-SECRET-VALUE", "session_token": "SESSION-TOKEN-VALUE"},
		}},
		sftp: {ID: sftp, Rclone: backend.Rclone{
			Backend: "sftp", Root: "/home/me",
			Options:     map[string]string{"host": "nas.example", "user": "me", "pass": "OBSCURED-OPTION-VALUE"},
			Secrets:     map[string]string{"key_pem": "SFTP-PASSWORD-VALUE"},
			ObscureKeys: []string{"pass"},
		}},
		disk: {ID: disk, Rclone: backend.Rclone{Root: "/mnt/disk1"}},
		u: {ID: u, Rclone: backend.Rclone{Backend: "union", Options: map[string]string{"action_policy": "epall"}, Secrets: map[string]string{"x": "UNION-SECRET-VALUE"}},
			Members: []Member{{RemoteID: sftp, Path: "pool", Mode: backend.UnionReadWrite}, {RemoteID: disk, Path: "p", Mode: backend.UnionReadOnly}}},
	}
	for name, def := range map[string]*syncrv1.RcloneBisyncDefinition{
		"r2 and a union": {Path1: &syncrv1.JobLocation{RemoteId: r2.String(), Path: "shared"}, Path2: &syncrv1.JobLocation{RemoteId: u.String(), Path: "docs"}},
		"local and sftp": {Path1: &syncrv1.JobLocation{Path: "/srv/shared"}, Path2: &syncrv1.JobLocation{RemoteId: sftp.String(), Path: "x"}},
	} {
		jd := &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneBisync{RcloneBisync: def}}
		refs, err := Bisync{}.Check(jd)
		if err != nil {
			t.Fatal(err)
		}
		rs, err := newSpecRemotes(refs.RemoteIDs, remotes)
		if err != nil {
			t.Fatal(err)
		}
		var sides [2]bisyncSide
		for i, l := range []*syncrv1.JobLocation{def.GetPath1(), def.GetPath2()} {
			loc, err := rs.location("f", l)
			if err != nil {
				t.Fatal(err)
			}
			sides[i] = bisyncSide{loc: loc}
			if l.GetRemoteId() != "" {
				sides[i].remote, _ = domain.ParseRemoteID(l.GetRemoteId())
			}
		}
		in, err := rs.bisyncStateInput(sides)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range secrets {
			if bytes.Contains(in, []byte(s)) {
				t.Errorf("%s: the hashed input contains the secret %q:\n%s", name, s, in)
			}
		}
		for _, k := range []string{"session_token", "pass"} {
			if bytes.Contains(in, []byte(":"+k+",")) {
				t.Errorf("%s: the hashed input names the secret option %q:\n%s", name, k, in)
			}
		}
		if !bytes.HasPrefix(in, []byte("21:syncr-bisync-state-v1,")) {
			t.Errorf("%s: input %q lacks the version", name, in)
		}
		key, err := rs.bisyncStateKey(sides)
		sum := sha256.Sum256(in)
		if err != nil || key != hex.EncodeToString(sum[:]) {
			t.Errorf("%s: key %q (%v) is not the SHA-256 of the input", name, key, err)
		}
	}

	// The exact encoding of the first pair, so that a change of it (which
	// would resync every pair) is deliberate.
	jd := &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneBisync{RcloneBisync: &syncrv1.RcloneBisyncDefinition{
		Path1: &syncrv1.JobLocation{Path: "/srv/shared"}, Path2: &syncrv1.JobLocation{RemoteId: r2.String(), Path: "shared"},
	}}}
	refs, _ := Bisync{}.Check(jd)
	rs, err := newSpecRemotes(refs.RemoteIDs, remotes)
	if err != nil {
		t.Fatal(err)
	}
	in, err := rs.bisyncStateInput([2]bisyncSide{
		{loc: &syncrv1.Location{Path: "/srv/shared"}},
		{loc: &syncrv1.Location{Remote: "r1", Path: "shared-bucket/shared"}, remote: r2},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "21:syncr-bisync-state-v1," +
		"5:local,11:/srv/shared," +
		"6:remote,36:" + r2.String() + ",2:s3," +
		"1:3,13:access_key_id,4:AKID,8:endpoint,23:https://acct.r2.example,8:provider,10:Cloudflare," +
		"1:0," +
		"20:shared-bucket/shared,"
	if string(in) != want {
		t.Errorf("input =\n%s\nwant\n%s", in, want)
	}
}

// TestKeyEncoderIsUnambiguous: fields are length-prefixed, so splitting
// the same bytes differently never gives the same encoding.
func TestKeyEncoderIsUnambiguous(t *testing.T) {
	t.Parallel()
	enc := func(fields ...string) string {
		e := keyEncoder{}
		for _, f := range fields {
			e.field(f)
		}
		return string(e.b)
	}
	pairs := [][2][]string{
		{{"ab", "c"}, {"a", "bc"}},
		{{"a,", "b"}, {"a", ",b"}},
		{{"1:a,"}, {"a"}},
		{{""}, {}},
		{{"local", "/x"}, {"local/x"}},
	}
	for _, p := range pairs {
		if a, b := enc(p[0]...), enc(p[1]...); a == b {
			t.Errorf("%q and %q both encode as %q", p[0], p[1], a)
		}
	}
	if got := enc("", "é,"); got != "0:,3:é,," {
		t.Errorf("encoding = %q", got)
	}
	if !strings.HasPrefix(bisyncLocationKey(location{key: "/x", ok: true}), BisyncKeyPrefix) {
		t.Error("location key without the prefix")
	}
}
