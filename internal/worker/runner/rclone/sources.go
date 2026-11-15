package rclone

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// reservedRules returns the filter rules ("- pattern") that keep rclone
// out of the folders and files the worker keeps for its own use
// (Config.Reserved: its data directory, with the folder of backups of
// remotes, its tools directory, its config and TLS files) when a local
// side of a transfer holds one, such as "/" or the data directory's
// parent (the path policy refuses each one and everything in it). rclone
// applies a transfer's filters to paths relative to each side's root, and
// a file at path X on one side lands at X on the other, so one rule per
// form of the reserved path relative to such a side (localfs.Inside: as
// written and resolved), anchored at the root, covers both sides:
// "- /<path>/**" for a folder, "- /<path>" for a file. Placed before the
// user's filters, the rules decide first (the first matching rule wins),
// so rclone neither lists, reads, writes nor deletes anything there, on
// either side, whatever the mode. A side that is not an absolute path (a
// remote) adds nothing. Errors are PATH_NOT_ALLOWED: a side that cannot be
// compared is not used.
func (r *Runner) reservedRules(sides ...string) ([]string, error) {
	var rules []string
	for _, reserved := range r.cfg.Reserved {
		file := false
		if info, err := os.Stat(reserved); err == nil && !info.IsDir() {
			file = true
		}
		for _, side := range sides {
			if !filepath.IsAbs(side) {
				continue
			}
			rels, err := localfs.Inside(side, reserved)
			if err != nil {
				return nil, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED,
					"cannot tell whether %s holds %s, which the worker keeps for its own use: %w", side, reserved, err)
			}
			for _, rel := range rels {
				rule := excludeRule(rel, r.goos)
				if file {
					rule = strings.TrimSuffix(rule, "/**")
				}
				if !slices.Contains(rules, rule) {
					rules = append(rules, rule)
				}
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
