// Package union is the backend descriptor of union remotes, which combine
// other remotes (rclone backend "union"). The members are managed by the
// remotes use case; the job builder turns them into the union's upstreams.
package union

import (
	"strconv"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Type is the union backend type.
type Type struct{}

var _ backend.Exampler = Type{}

// policies are rclone's union policies
// (https://rclone.org/union/#policy-descriptions).
func policies() []backend.Choice {
	return []backend.Choice{
		{Value: "epall", Label: "Existing path, all"},
		{Value: "epff", Label: "Existing path, first found"},
		{Value: "eplfs", Label: "Existing path, least free space"},
		{Value: "eplus", Label: "Existing path, least used space"},
		{Value: "eplno", Label: "Existing path, least number of objects"},
		{Value: "epmfs", Label: "Existing path, most free space"},
		{Value: "eprand", Label: "Existing path, random"},
		{Value: "all", Label: "All"},
		{Value: "ff", Label: "First found"},
		{Value: "lfs", Label: "Least free space"},
		{Value: "lus", Label: "Least used space"},
		{Value: "lno", Label: "Least number of objects"},
		{Value: "mfs", Label: "Most free space"},
		{Value: "newest", Label: "Newest"},
		{Value: "rand", Label: "Random"},
	}
}

// ID implements backend.Type.
func (Type) ID() string { return "union" }

// Label implements backend.Type.
func (Type) Label() string { return "Union" }

// Description implements backend.Type.
func (Type) Description() string {
	return "Several of your remotes combined into one, like a pooled disk."
}

// Fields implements backend.Type.
func (Type) Fields() []backend.Field {
	return []backend.Field{
		{Name: "members", Label: "Members", Kind: backend.KindMembers, Required: true,
			Help: "At least two remotes, each with a path and a mode."},
		{Name: "action_policy", Label: "Action policy", Kind: backend.KindEnum, Default: "epall", Choices: policies(),
			Help: "Picks the members that changes and deletions apply to."},
		{Name: "create_policy", Label: "Create policy", Kind: backend.KindEnum, Default: "epmfs", Choices: policies(),
			Help: "Picks the member that new files go to."},
		{Name: "search_policy", Label: "Search policy", Kind: backend.KindEnum, Default: "ff", Choices: policies(),
			Help: "Picks the member that files are read from."},
		{Name: "cache_time", Label: "Cache time (seconds)", Kind: backend.KindInt, Default: "120", Advanced: true,
			Help: "How long free and used space are cached, for the space-based policies."},
	}
}

// Capabilities implements backend.Type.
func (Type) Capabilities() backend.Capabilities { return backend.Capabilities{Union: true} }

// Validate implements backend.Type: the field checks and a cache time that
// is not negative. Members are checked by the remotes use case.
func (t Type) Validate(cfg backend.Config) error {
	var v domain.Validation
	v.AddAll("", backend.CheckFields(t.Fields(), cfg))
	if n, err := strconv.ParseInt(cfg.Options["cache_time"], 10, 64); err == nil && n < 0 {
		v.Add("options.cache_time", "must not be negative")
	}
	return v.Err()
}

// ToRclone implements backend.Type: the policies only; the job builder adds
// the upstreams.
func (t Type) ToRclone(cfg backend.Config) (backend.Rclone, error) {
	r := backend.MapFields(t.Fields(), cfg)
	r.Backend = "union"
	return r, nil
}

// Examples implements backend.Exampler.
func (Type) Examples() []backend.Config {
	return []backend.Config{
		{Options: map[string]string{}},
		{Options: map[string]string{"action_policy": "all", "create_policy": "lfs", "search_policy": "newest", "cache_time": "60"}},
	}
}
