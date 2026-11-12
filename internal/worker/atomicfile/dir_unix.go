//go:build !windows

package atomicfile

import (
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"
)

func mkdirPrivate(path string, opts DirOptions) (DirResult, error) {
	perm := opts.Perm
	if perm == 0 {
		perm = 0o700
	}
	if err := os.MkdirAll(path, perm); err != nil {
		return DirResult{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return DirResult{}, err
	}
	if mode := info.Mode(); mode&0o002 != 0 && mode&fs.ModeSticky == 0 {
		return DirResult{}, fmt.Errorf("every user can write to %s, so anyone could change the files the worker keeps and runs there: "+
			"remove that permission (chmod o-w %s) or choose another directory", path, path)
	}
	var res DirResult
	if problems := statExposure(info); len(problems) > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %s, so another account can change the files the worker keeps there", path, strings.Join(problems, "; ")))
	}
	return res, nil
}

func exposure(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return statExposure(info), nil
}

func statExposure(info fs.FileInfo) []string {
	var out []string
	st, ok := info.Sys().(*syscall.Stat_t)
	if ok && st.Uid != 0 && int(st.Uid) != os.Geteuid() {
		out = append(out, "it is owned by "+userName(st.Uid))
	}
	mode := info.Mode()
	if mode&0o020 != 0 {
		group := "its group"
		if ok {
			group = "its group " + groupName(st.Gid)
		}
		out = append(out, group+" can write to it")
	}
	if mode&0o002 != 0 {
		out = append(out, "every user can write to it")
	}
	return out
}

func userName(uid uint32) string {
	id := strconv.FormatUint(uint64(uid), 10)
	if u, err := user.LookupId(id); err == nil {
		return u.Username + " (uid " + id + ")"
	}
	return "uid " + id
}

func groupName(gid uint32) string {
	id := strconv.FormatUint(uint64(gid), 10)
	if g, err := user.LookupGroupId(id); err == nil {
		return g.Name + " (gid " + id + ")"
	}
	return "gid " + id
}
