package restic

import (
	"encoding/csv"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// rcloneArgs are the arguments restic passes to rclone ("-o rclone.args").
// They replace restic's default ("serve restic --stdio --b2-hard-delete"):
// "--config=" makes rclone ignore any rclone.conf of the worker's user, since
// remotes come only from the environment; the other two flags keep restic's
// defaults that its documentation asks for (real deletes on B2 and Google
// Drive).
func rcloneArgs() []string {
	return []string{"serve", "restic", "--stdio", "--config=", "--b2-hard-delete", "--drive-use-trash=false"}
}

// rcloneOptions returns the restic global flags that make restic start the
// rclone binary program with args for an "rclone:" repository.
//
// restic does not take a list. Each flag value goes through three layers
// before rclone is started (restic 0.19.1):
//
//  1. "--option" is a pflag StringSlice, so its value is read as one CSV
//     record (encoding/csv, comma-separated, with "..." quoting): a comma
//     would start a second option and a '"' inside an unquoted field is an
//     error.
//  2. options.splitKeyValue cuts each option at its first '=' and trims
//     white space around key and value.
//  3. rclone.newBackend splits the values of rclone.program and rclone.args
//     with backend.SplitShellStrings, whose rules are not a POSIX shell's:
//     - outside quotes, a backslash is a separator like white space, so
//     `D:\a\rclone.exe` would become the program "D:" and the arguments
//     "a" and "rclone.exe";
//     - a single or double quote starts a quoted run that ends at the same
//     quote; quotes always end the current field and are dropped, and
//     inside a quoted run every character is literal;
//     - a backslash is never removed; it only stops the character after it
//     from opening or closing a quote, so a quoted run cannot end with one;
//     - an empty quoted run produces no field at all.
//
// The result is passed to exec.Command, which quotes each argument for the
// Windows command line itself.
//
// So each element is emitted as is when it contains no white space,
// backslash or quote, and otherwise enclosed in whichever quote it does not
// contain (resticQuote); then the whole "key=value" is CSV-quoted when it
// contains a comma or a '"' (optionFlag). That covers drive letters, UNC
// paths, backslashes, spaces, commas, '$', '%' and either quote character.
// Elements restic cannot receive (empty, ending with a backslash, containing
// both quote characters, or a line break, which CSV would rewrite) are an
// error.
func rcloneOptions(program string, args []string) ([]string, error) {
	prog, err := resticQuote(program)
	if err != nil {
		return nil, fmt.Errorf("rclone program %q: %w", program, err)
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		if quoted[i], err = resticQuote(a); err != nil {
			return nil, fmt.Errorf("rclone argument %q: %w", a, err)
		}
	}
	progFlag, err := optionFlag("rclone.program", prog)
	if err != nil {
		return nil, err
	}
	argsFlag, err := optionFlag("rclone.args", strings.Join(quoted, " "))
	if err != nil {
		return nil, err
	}
	return []string{progFlag, argsFlag}, nil
}

// optionFlag returns the restic flag "--option=key=value", with key=value
// encoded as the single CSV field that pflag's StringSlice reads back.
func optionFlag(key, value string) (string, error) {
	var b strings.Builder
	w := csv.NewWriter(&b)
	if err := w.Write([]string{key + "=" + value}); err != nil {
		return "", err
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return "", err
	}
	return "--option=" + strings.TrimSuffix(b.String(), "\n"), nil
}

// resticQuote encodes s as a single field for restic's SplitShellStrings (see
// rcloneOptions).
func resticQuote(s string) (string, error) {
	switch {
	case s == "":
		return "", errors.New("restic cannot pass an empty argument")
	case strings.ContainsAny(s, "\x00\r\n"):
		// NUL cannot be in an argument; restic's CSV reader rewrites CR LF.
		return "", errors.New("restic cannot pass an argument with a NUL or line break")
	case !strings.ContainsFunc(s, func(r rune) bool { return r == '\\' || r == '"' || r == '\'' || unicode.IsSpace(r) }):
		return s, nil
	case strings.HasSuffix(s, `\`):
		// The backslash would keep the closing quote from closing.
		return "", errors.New("restic cannot pass an argument that ends with a backslash")
	case !strings.Contains(s, `"`):
		return `"` + s + `"`, nil
	case !strings.Contains(s, `'`):
		return `'` + s + `'`, nil
	}
	return "", errors.New("restic cannot pass an argument that contains both quote characters")
}
