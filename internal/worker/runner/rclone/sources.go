package rclone

import (
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// sourcesRules returns the filter rules ("- pattern") that keep rclone out
// of the worker's folder of backups of remotes (Config.SourcesDir) when a
// local side of a transfer holds it, such as "/" or the data directory
// (the path policy refuses the folder itself and everything in it). rclone
// applies a transfer's filters to paths relative to each side's root, and
// a file at path X on one side lands at X on the other, so one rule per
// form of the folder's path relative to such a side (localfs.Inside: as
// written and resolved), anchored at the root, covers both sides:
// "- /<path>/**". Placed before the user's filters, the rules decide first
// (the first matching rule wins), so rclone neither lists, reads, writes
// nor deletes anything there, on either side, whatever the mode. A side
// that is not an absolute path (a remote) adds nothing. Errors are
// PATH_NOT_ALLOWED: a side that cannot be compared is not used.
func (r *Runner) sourcesRules(sides ...string) ([]string, error) {
	if r.cfg.SourcesDir == "" {
		return nil, nil
	}
	var rules []string
	for _, side := range sides {
		if !filepath.IsAbs(side) {
			continue
		}
		rels, err := localfs.Inside(side, r.cfg.SourcesDir)
		if err != nil {
			return nil, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED,
				"cannot tell whether %s holds the worker's folder of backups of remotes: %w", side, err)
		}
		for _, rel := range rels {
			if rule := excludeRule(rel, r.goos); !slices.Contains(rules, rule) {
				rules = append(rules, rule)
			}
		}
	}
	return rules, nil
}

// withRules inserts --filter flags for rules before the first filter flag
// of args, or before its "--" when it has none, so they come first.
func withRules(args, rules []string) []string {
	if len(rules) == 0 {
		return args
	}
	i := slices.IndexFunc(args, func(a string) bool { return a == "--" || strings.HasPrefix(a, "--filter=") })
	if i < 0 {
		return args
	}
	flags := make([]string, len(rules))
	for k, rule := range rules {
		flags[k] = "--filter=" + rule
	}
	return slices.Insert(slices.Clip(args), i, flags...)
}

// excludeRule is the rclone filter rule that leaves out the folder at rel
// and everything in it: "- /a/b/**", anchored at the root rclone walks.
// Each name is escaped for rclone's globs; a component with several names
// becomes an alternation ("{long,SHORT~1}"). On Windows, where NTFS opens a
// name in any case and by its short name, every letter matches in either
// case, so a path spelled otherwise on the other side cannot reach the
// folder.
func excludeRule(rel localfs.RelPath, goos string) string {
	var b strings.Builder
	b.WriteString("- ")
	for _, names := range rel {
		b.WriteByte('/')
		var alts []string
		for _, n := range names {
			if g := globName(n, goos); !slices.Contains(alts, g) {
				alts = append(alts, g)
			}
		}
		if len(alts) == 1 {
			b.WriteString(alts[0])
			continue
		}
		b.WriteString("{" + strings.Join(alts, ",") + "}")
	}
	b.WriteString("/**")
	return b.String()
}

// globName writes a file name as an rclone glob that matches exactly that
// name: a backslash before each of \ * ? [ ] { } and "," (the separator of
// an alternation); on Windows each letter becomes a class of its case
// variants ("[sS]").
func globName(name, goos string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case strings.ContainsRune(`\*?[]{},`, r):
			b.WriteByte('\\')
			b.WriteRune(r)
		case goos == "windows" && unicode.SimpleFold(r) != r:
			b.WriteByte('[')
			b.WriteRune(r)
			for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
				b.WriteRune(f)
			}
			b.WriteByte(']')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
