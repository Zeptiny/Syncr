package localfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ErrLink reports a symbolic link (on Windows also a junction or mount
// point) in a folder that a tool writes into, which leads to a folder jobs
// may not write: outside every root, into a reserved folder or above one,
// or somewhere the worker cannot check (WriteAreas). It is always joined
// with ErrNotAllowed.
var ErrLink = errors.New("a link in the folder leads to a folder jobs may not write")

// WriteAreas checks the folder dir, which Resolve must accept, before a tool
// writes into it, and calls visit with each folder the tool can reach there,
// before that folder is checked: dir itself (resolved), then each folder
// that a link in an already checked folder leads to, unless a folder it
// visited holds it. A tool may follow those links: rclone, asked to write
// "a/b" where "a" is a link to a folder, writes into the link's target
// (it replaces a link to a file, and never creates a link). A visit error
// ends the check and is returned as is. Callers record each folder as
// visited (Claims), so that no job that can create or move links runs
// there during or after the check.
//
// Every link found must lead to a folder inside a root that neither is
// nor lies inside a reserved folder, nor holds one (Reserve), or to a
// file, which a tool replaces rather than writes through. A link whose
// target does not exist must have a target without ".." components that
// Resolve accepts, so that a folder a tool creates cannot bring it to
// life elsewhere. Any other link fails the check with a *PathError
// naming the link and wrapping ErrNotAllowed and ErrLink. Links are never
// followed by the check itself; reserved folders and Linux pseudo
// filesystems below a visited folder are not entered, and folders that
// cannot be read are skipped (a tool running as the same user cannot
// write there either). A dir that does not exist yet is visited but has
// nothing to check.
//
// Errors are also those of Resolve, the file system's for dir itself, and
// ctx's (ErrInvalidPath when dir is not a folder). The check reads every
// entry below dir: it takes about as long as listing the folder.
func (p *Policy) WriteAreas(ctx context.Context, dir string, visit func(folder string) error) error {
	clean, err := p.Resolve(dir)
	if err != nil {
		return err
	}
	start, err := resolve(clean)
	if err != nil {
		return err
	}
	var visited []string
	queue := []string{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if slices.ContainsFunc(visited, func(v string) bool { return within(v, cur) }) {
			continue
		}
		visited = append(visited, cur)
		if visit != nil {
			if err := visit(cur); err != nil {
				return err
			}
		}
		info, err := os.Stat(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist) && cur == start:
			continue // a new destination: the tool creates it
		case err != nil:
			return err
		case !info.IsDir():
			if cur == start {
				return &PathError{Path: dir, Err: fmt.Errorf("%w: not a folder", ErrInvalidPath)}
			}
			continue
		}
		w := &walker{ctx: ctx, open: openDir, pseudo: pseudoFS}
		w.skip = func(d string) bool { return p.isReserved(cur, cur, d) }
		found, err := p.checkTree(w, cur)
		if errors.Is(err, errStop) {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		queue = append(queue, found...)
	}
	return nil
}

// checkTree checks every link below the folder root (resolved) and returns
// the folders the links lead to.
func (p *Policy) checkTree(w *walker, root string) ([]string, error) {
	var found []string
	stack := []string{root}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		err := w.readDir(cur, cur == root, func(de fs.DirEntry) error {
			path := filepath.Join(cur, de.Name())
			switch {
			case de.IsDir():
				stack = append(stack, path)
			case de.Type()&fs.ModeSymlink != 0 || (de.Type()&fs.ModeIrregular != 0 && isLink(path)):
				target, isDir, err := p.checkLink(path)
				if err != nil {
					return err
				}
				if isDir {
					found = append(found, target)
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return found, nil
}

// isLink reports whether the irregular entry at path is a link: Go reports
// Windows junctions and mount points as irregular, but also cloud
// placeholders, which have no target and must not be opened (that could
// download them).
func isLink(path string) bool {
	_, err := os.Readlink(path)
	return err == nil
}

// checkLink checks the link (or Windows junction or mount point) at path,
// whose folder is resolved, and returns its resolved target and whether
// that is a folder.
func (p *Policy) checkLink(path string) (string, bool, error) {
	refuse := func(why string) (string, bool, error) {
		return "", false, &PathError{Path: path, Err: fmt.Errorf("%w: %w: %s", ErrNotAllowed, ErrLink, why)}
	}
	target, err := resolveExisting(path)
	if errors.Is(err, fs.ErrNotExist) {
		return p.checkDangling(path, refuse)
	}
	if err != nil {
		return refuse(fmt.Sprintf("it cannot be resolved (%v)", err))
	}
	info, err := os.Stat(target)
	if err != nil {
		return refuse(fmt.Sprintf("its target %s cannot be read (%v)", target, err))
	}
	if !info.IsDir() {
		return target, false, nil
	}
	if r, ok := p.reservedOverlapping(target); ok {
		return refuse(fmt.Sprintf("it leads to %s, which is or holds %s, a folder the worker keeps for its own use", target, r.clean))
	}
	for _, r := range p.roots {
		if within(r.resolved, target) {
			return target, true, nil
		}
	}
	return refuse(fmt.Sprintf("it leads to %s, outside local_roots", target))
}

// checkDangling checks a link at path whose target does not exist: a tool
// cannot write through it now, but would once the target is created, so
// the target, taken from the link's folder, must contain no ".." (which a
// folder created later can turn into anywhere) and be accepted by Resolve.
func (p *Policy) checkDangling(path string, refuse func(string) (string, bool, error)) (string, bool, error) {
	raw, err := os.Readlink(path)
	if err != nil {
		return refuse(fmt.Sprintf("its target does not exist and cannot be read (%v)", err))
	}
	if slices.Contains(strings.FieldsFunc(raw, func(r rune) bool { return r == '/' || r == filepath.Separator }), "..") {
		return refuse(fmt.Sprintf("its target %q does not exist and contains \"..\"", raw))
	}
	target := raw
	if !filepath.IsAbs(target) {
		if !filepath.IsLocal(target) {
			return refuse(fmt.Sprintf("its target %q does not exist and is not a plain relative path", raw))
		}
		target = filepath.Join(filepath.Dir(path), target)
	}
	if _, err := p.Resolve(target); err != nil {
		return refuse(fmt.Sprintf("its target %s does not exist and is not a path jobs may use (%v)", target, err))
	}
	if r, ok := p.reservedOverlapping(filepath.Clean(target)); ok {
		return refuse(fmt.Sprintf("its target %s does not exist and holds %s, a folder the worker keeps for its own use", target, r.clean))
	}
	return "", false, nil
}

// reservedOverlapping returns the reserved folder that the resolved path
// is, lies inside or holds, compared with both forms of the folder.
func (p *Policy) reservedOverlapping(path string) (root, bool) {
	for _, r := range p.reserved {
		if within(r.clean, path) || within(r.resolved, path) || within(path, r.clean) || within(path, r.resolved) {
			return r, true
		}
	}
	return root{}, false
}
