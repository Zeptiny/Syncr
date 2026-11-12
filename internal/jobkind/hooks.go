package jobkind

import (
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// hooksField is the field path of the hooks in a definition.
const hooksField = "definition.hooks"

// checkHooks checks def's hooks (JobDefinition.hooks), for every kind: each
// list names at most link.MaxHooksPerPhase hooks, every name is a hook name
// (link.ValidHookName) and none is listed twice in one list. Whether the
// job's worker defines them is not known here: the API, the scheduler and
// the dispatcher check that against what the worker reported.
//
// Field paths are "definition.hooks.<list>" (the list is too long) and
// "definition.hooks.<list>[i]" (a name), e.g. "definition.hooks.before[0]".
func checkHooks(def *syncrv1.JobDefinition) *domain.ValidationError {
	var fields []domain.FieldError
	for _, phase := range link.HookPhases() {
		names := link.HooksOf(def.GetHooks(), phase)
		field := hooksField + "." + link.HookPhaseName(phase)
		if len(names) > link.MaxHooksPerPhase {
			fields = append(fields, domain.FieldError{Field: field, Message: fmt.Sprintf("names more than %d hooks", link.MaxHooksPerPhase)})
			continue
		}
		for i, name := range names {
			switch {
			case !link.ValidHookName(name):
				fields = append(fields, domain.FieldError{Field: fmt.Sprintf("%s[%d]", field, i),
					Message: fmt.Sprintf("is not a hook name: 1 to %d lower-case letters, digits, '.', '_' and '-', starting with a letter or digit", link.MaxHookNameLen)})
			case slices.Contains(names[:i], name):
				fields = append(fields, domain.FieldError{Field: fmt.Sprintf("%s[%d]", field, i), Message: "is listed twice"})
			}
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return &domain.ValidationError{Fields: fields}
}

// HooksOf returns a copy of the hooks def names (JobDefinition.hooks), or
// nil when it names none: what the worker's spec carries.
func HooksOf(def *syncrv1.JobDefinition) *syncrv1.JobHooks {
	h := def.GetHooks()
	if !link.HasHooks(h) {
		return nil
	}
	out, _ := proto.Clone(h).(*syncrv1.JobHooks)
	return out
}

// HookNames returns every hook def names, once each, in the order they
// run; nil when it names none. A worker must define all of them to run a
// job of def.
func HookNames(def *syncrv1.JobDefinition) []string {
	return link.HookNames(def.GetHooks())
}

// MissingHooks returns the hooks of def that are not among defined (the
// names a worker reported), in the order they run; nil when the worker
// defines them all.
func MissingHooks(def *syncrv1.JobDefinition, defined []string) []string {
	var missing []string
	for _, name := range HookNames(def) {
		if !slices.Contains(defined, name) {
			missing = append(missing, name)
		}
	}
	return missing
}

// HookField returns the field path of the first place def names the hook:
// "definition.hooks.before[1]". It is "definition.hooks" for a hook def
// does not name.
func HookField(def *syncrv1.JobDefinition, name string) string {
	for _, phase := range link.HookPhases() {
		if i := slices.Index(link.HooksOf(def.GetHooks(), phase), name); i >= 0 {
			return fmt.Sprintf("%s.%s[%d]", hooksField, link.HookPhaseName(phase), i)
		}
	}
	return hooksField
}
