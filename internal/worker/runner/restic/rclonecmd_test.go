package restic

import (
	"encoding/csv"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode"
)

// resticSplit is restic's backend.SplitShellStrings and its shellSplitter,
// copied verbatim (apart from names and the error package) from restic
// v0.19.1, internal/backend/shell_split.go. Copyright (c) 2014, Alexander
// Neumann <alexander@bumpern.de>; BSD 2-Clause License.
func resticSplit(data string) (strs []string, err error) {
	s := &resticShellSplitter{}

	// derived from strings.SplitFunc
	fieldStart := -1 // Set to -1 when looking for start of field.
	for i, r := range data {
		if s.isSplitChar(r) {
			if fieldStart >= 0 {
				strs = append(strs, data[fieldStart:i])
				fieldStart = -1
			}
		} else if fieldStart == -1 {
			fieldStart = i
		}
	}
	if fieldStart >= 0 { // Last field might end at EOF.
		strs = append(strs, data[fieldStart:])
	}

	switch s.quote {
	case '\'':
		return nil, errors.New("single-quoted string not terminated")
	case '"':
		return nil, errors.New("double-quoted string not terminated")
	}

	if len(strs) == 0 {
		return nil, errors.New("command string is empty")
	}

	return strs, nil
}

type resticShellSplitter struct {
	quote    rune
	lastChar rune
}

func (s *resticShellSplitter) isSplitChar(c rune) bool {
	// only test for quotes if the last char was not a backslash
	if s.lastChar != '\\' {

		// quote ended
		if s.quote != 0 && c == s.quote {
			s.quote = 0
			return true
		}

		// quote starts
		if s.quote == 0 && (c == '"' || c == '\'') {
			s.quote = c
			return true
		}
	}

	s.lastChar = c

	// within quote
	if s.quote != 0 {
		return false
	}

	// outside quote
	return c == '\\' || unicode.IsSpace(c)
}

// pflagReadAsCSV is how pflag v1.0.10 (restic 0.19.1's) reads the value of
// a StringSlice flag such as --option (readAsCSV in string_slice.go).
func pflagReadAsCSV(val string) ([]string, error) {
	if val == "" {
		return []string{}, nil
	}
	return csv.NewReader(strings.NewReader(val)).Read()
}

// resticRcloneCommand does what restic 0.19.1 does with the flags of
// rcloneOptions: pflag reads each --option value as CSV,
// options.splitKeyValue cuts each option at its first '=' and trims spaces,
// options.Parse rejects conflicting keys, and rclone.newBackend splits
// program and args. It returns the argv of the rclone process, without the
// remote restic appends.
func resticRcloneCommand(flags []string) ([]string, error) {
	values := map[string]string{}
	for _, f := range flags {
		val, ok := strings.CutPrefix(f, "--option=")
		if !ok {
			return nil, fmt.Errorf("flag %q is not an --option", f)
		}
		opts, err := pflagReadAsCSV(val)
		if err != nil {
			return nil, fmt.Errorf("pflag cannot read %q: %w", val, err)
		}
		for _, o := range opts {
			k, v, _ := strings.Cut(o, "=")
			k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
			if old, dup := values[k]; dup && old != v {
				return nil, fmt.Errorf("key %q present more than once", k)
			}
			values[k] = v
		}
	}
	if len(values) != 2 {
		return nil, fmt.Errorf("options %q, want rclone.program and rclone.args only", values)
	}
	var argv []string
	for _, key := range []string{"rclone.program", "rclone.args"} {
		v, ok := values[key]
		if !ok {
			return nil, fmt.Errorf("no %s in %q", key, values)
		}
		a, err := resticSplit(v)
		if err != nil {
			return nil, fmt.Errorf("restic cannot split %s=%q: %w", key, v, err)
		}
		argv = append(argv, a...)
	}
	return argv, nil
}

func TestResticSplitMatchesRestic(t *testing.T) {
	t.Parallel()
	// restic's own test cases (shell_split_test.go), plus the behavior the
	// encoding relies on.
	tests := []struct {
		in   string
		want []string
	}{
		{`foo`, []string{"foo"}},
		{`'foo'`, []string{"foo"}},
		{`foo bar baz`, []string{"foo", "bar", "baz"}},
		{`foo 'bar' baz`, []string{"foo", "bar", "baz"}},
		{`'bar box' baz`, []string{"bar box", "baz"}},
		{`"bar 'box'" baz`, []string{"bar 'box'", "baz"}},
		{`'bar "box"' baz`, []string{`bar "box"`, "baz"}},
		{`\"bar box baz`, []string{`"bar`, "box", "baz"}},
		{`"bar/foo/x" "box baz"`, []string{"bar/foo/x", "box baz"}},
		// the CI failure: backslashes split outside quotes
		{`D:\a\_temp\rclone.exe serve`, []string{"D:", "a", "_temp", "rclone.exe", "serve"}},
		{`"D:\a\_temp\rclone.exe" serve`, []string{`D:\a\_temp\rclone.exe`, "serve"}},
		{`a"b c"d`, []string{"a", "b c", "d"}},
		{`x "" y`, []string{"x", "y"}},
	}
	for _, tt := range tests {
		if got, err := resticSplit(tt.in); err != nil || !slices.Equal(got, tt.want) {
			t.Errorf("resticSplit(%q) = (%q, %v), want %q", tt.in, got, err, tt.want)
		}
	}
	if _, err := resticSplit(`"C:\dir\"`); err == nil {
		t.Error(`resticSplit("C:\dir\") succeeded; a backslash before the closing quote should keep it open`)
	}
}

func TestResticQuote(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		// Windows
		{`D:\a\_temp\syncr-tools\rclone\v1.71.0\rclone.exe`, `"D:\a\_temp\syncr-tools\rclone\v1.71.0\rclone.exe"`},
		{`C:\Program Files\Syncr\tools\rclone.exe`, `"C:\Program Files\Syncr\tools\rclone.exe"`},
		{`C:\Users\O'Brien\AppData\rclone.exe`, `"C:\Users\O'Brien\AppData\rclone.exe"`},
		{`C:\Users\RUNNER~1\$x %TEMP% (x86)\rclone.exe`, `"C:\Users\RUNNER~1\$x %TEMP% (x86)\rclone.exe"`},
		{`\\srv\share\tools\rclone.exe`, `"\\srv\share\tools\rclone.exe"`},
		{`C:\tools\'q'\rclone.exe`, `"C:\tools\'q'\rclone.exe"`},
		{`D:/a/_temp/rclone.exe`, `D:/a/_temp/rclone.exe`},
		{`C:\Données\ツール\rclone.exe`, `"C:\Données\ツール\rclone.exe"`},
		// Unix
		{`/usr/local/bin/rclone`, `/usr/local/bin/rclone`},
		{`/opt/my tools/rclone`, `"/opt/my tools/rclone"`},
		{`/opt/it's/rclone`, `"/opt/it's/rclone"`},
		{`/opt/say "hi"/rclone`, `'/opt/say "hi"/rclone'`},
		{`/opt/back\slash/rclone`, `"/opt/back\slash/rclone"`},
		{`/opt/back\"slash/rclone`, `'/opt/back\"slash/rclone'`},
		{`/opt/back\'slash/rclone`, `"/opt/back\'slash/rclone"`},
		{"/opt/tab\there/rclone", "\"/opt/tab\there/rclone\""},
		{"/opt/nbsp\u00a0dir/rclone", "\"/opt/nbsp\u00a0dir/rclone\""},
		{`/opt/$HOME/%x%/;&|*?/rclone`, `/opt/$HOME/%x%/;&|*?/rclone`},
		// arguments
		{"--config=", "--config="},
		{"serve", "serve"},
		{" ", `" "`},
		{`--config=C:\Users\me\rclone.conf`, `"--config=C:\Users\me\rclone.conf"`},
	}
	for _, tt := range tests {
		got, err := resticQuote(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("resticQuote(%q) = (%q, %v), want %q", tt.in, got, err, tt.want)
			continue
		}
		if split, err := resticSplit(got); err != nil || !slices.Equal(split, []string{tt.in}) {
			t.Errorf("restic splits %q into (%q, %v), want [%q]", got, split, err, tt.in)
		}
	}

	for _, in := range []string{"", `C:\dir\`, `/opt/a\`, `/opt/it's "x"/rclone`} {
		if got, err := resticQuote(in); err == nil {
			t.Errorf("resticQuote(%q) = %q, want an error", in, got)
		}
	}
}

func TestRcloneOptions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		program string
		want    []string
	}{
		{"/fake/bin/rclone", []string{
			"--option=rclone.program=/fake/bin/rclone",
			"--option=rclone.args=serve restic --stdio --config= --b2-hard-delete --drive-use-trash=false",
		}},
		{`D:\a\_temp\syncr-tools\rclone\v1.71.0\rclone.exe`, []string{
			`--option="rclone.program=""D:\a\_temp\syncr-tools\rclone\v1.71.0\rclone.exe"""`,
			"--option=rclone.args=serve restic --stdio --config= --b2-hard-delete --drive-use-trash=false",
		}},
		{`/opt/say "hi", it/rclone`, []string{
			`--option="rclone.program='/opt/say ""hi"", it/rclone'"`,
			"--option=rclone.args=serve restic --stdio --config= --b2-hard-delete --drive-use-trash=false",
		}},
	}
	for _, tt := range tests {
		got, err := rcloneOptions(tt.program, rcloneArgs())
		if err != nil || !slices.Equal(got, tt.want) {
			t.Errorf("rcloneOptions(%q) = (%q, %v), want %q", tt.program, got, err, tt.want)
		}
	}

	programs := []string{
		`D:\a\_temp\syncr-tools\rclone\v1.71.0\rclone.exe`,
		`C:\Program Files\Syncr\it's $x %y%\rclone.exe`,
		`C:\Users\Doe, John\tools\rclone.exe`,
		`\\srv\share\tools\rclone.exe`,
		`D:/a/_temp/rclone.exe`,
		`/usr/local/bin/rclone`,
		`/dev/shm/syncr it's \ $x %y%/rclone`,
		`/dev/shm/syncr "dq" dir/rclone`,
		`/dev/shm/a,b/"c",d/rclone`,
		" /lead and trail/rclone ",
	}
	args := [][]string{
		rcloneArgs(),
		{"serve", "restic", "--config=", `--cache-dir=C:\Users\a b\cache`, `it's`, `"quoted"`, `\x`, "a,b", `,"`, "=", " x "},
	}
	for _, prog := range programs {
		for _, a := range args {
			flags, err := rcloneOptions(prog, a)
			if err != nil {
				t.Errorf("rcloneOptions(%q, %q): %v", prog, a, err)
				continue
			}
			want := append([]string{prog}, a...)
			if got, err := resticRcloneCommand(flags); err != nil || !slices.Equal(got, want) {
				t.Errorf("restic runs (%q, %v), want %q (flags %q)", got, err, want, flags)
			}
		}
	}

	for _, bad := range []struct {
		program string
		args    []string
	}{
		{`/opt/a'b"c/rclone`, rcloneArgs()},
		{`C:\tools\`, rcloneArgs()},
		{"", rcloneArgs()},
		{"/opt/new\nline/rclone", rcloneArgs()},
		{"rclone", []string{"serve", ""}},
		{"rclone", []string{"serve", "a\r\nb"}},
	} {
		if got, err := rcloneOptions(bad.program, bad.args); err == nil {
			t.Errorf("rcloneOptions(%q, %q) = %q, want an error", bad.program, bad.args, got)
		}
	}
}

func TestNewRejectsUnencodableRclonePath(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	cfg := te.cfg
	cfg.RclonePath = `/opt/a'b"c/rclone`
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "RclonePath") {
		t.Errorf("New with an unencodable RclonePath: err = %v", err)
	}
}

func FuzzRcloneOptions(f *testing.F) {
	for _, s := range []string{`D:\a\b.exe`, "/opt/x y/z", `it's`, `"q"`, `a\'b`, `\"`, " ", "", `x\`, "\t\n", "a,b", `"a,b"`, "k=v"} {
		f.Add(s, s)
	}
	f.Fuzz(func(t *testing.T, program, arg string) {
		args := []string{"serve", arg, "--config="}
		flags, err := rcloneOptions(program, args)
		if err != nil {
			for _, s := range []string{program, arg} {
				if s == "" || strings.HasSuffix(s, `\`) || strings.ContainsAny(s, "\x00\r\n") ||
					strings.Contains(s, `"`) && strings.Contains(s, `'`) {
					return
				}
			}
			t.Fatalf("rcloneOptions(%q, %q): %v", program, args, err)
		}
		want := append([]string{program}, args...)
		if got, err := resticRcloneCommand(flags); err != nil || !slices.Equal(got, want) {
			t.Fatalf("flags %q: restic runs (%q, %v), want %q", flags, got, err, want)
		}
	})
}
