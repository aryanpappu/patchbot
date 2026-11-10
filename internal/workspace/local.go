package workspace

import (
	"cmp"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
)

// maxSymlinkHops bounds how many symlinks one path may lead through. A loop
// runs into it.
const maxSymlinkHops = 32

// The modes of files and directories the agent creates. A checkout is meant
// to be read by the build and test commands, so they are the usual ones.
const (
	newFileMode fs.FileMode = 0o644
	newDirMode  fs.FileMode = 0o755
)

// Local is a Workspace on a local directory. It is safe for concurrent use.
//
// Every operation works in three steps:
//
//  1. The path is validated as text and checked against the protected-path
//     rules (Policy).
//  2. The path is resolved by hand, one component at a time, inside the
//     os.Root. A symlink is replaced by its target, and the rules are
//     applied again to every path this produces, so an alias of a protected
//     path is refused. A target that is absolute or climbs out of the
//     checkout is refused. Each real file or directory on the way is also
//     compared by identity with the protected paths that exist, which
//     catches a name the file system folds onto a protected one in a way
//     the rules did not predict.
//  3. The operation runs on the resolved, symlink-free path. The directory
//     that holds the file is opened level by level through os.Root, and
//     each level is verified not to be a symlink, so a path swapped for a
//     symlink between step 2 and step 3 is refused instead of followed.
//
// os.Root is the boundary for leaving the checkout: even if steps 1 and 2
// had a bug, step 3 cannot open anything outside it.
type Local struct {
	dir    string // absolute path of the checkout
	runDir string // per-run directory that holds dir, or "" when not made by a Manager
	root   *os.Root
	policy *Policy
	opts   Options

	closeOnce sync.Once
	closeErr  error
	onClose   func(keep bool) error
	// testHook forces namespace changes at acquisition/use boundaries in tests.
	// It is set before an operation starts and is nil in production.
	testHook func(stage string)
}

var _ Workspace = (*Local)(nil)

// Open returns a workspace on an existing directory.
func Open(dir string, opts Options) (*Local, error) {
	policy, err := NewPolicy(opts.Protected)
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	root, err := os.OpenRoot(abs + string(os.PathSeparator) + ".")
	if err != nil {
		return nil, fmt.Errorf("workspace: open checkout: %w", err)
	}
	return localFromRoot(abs, root, policy, opts), nil
}

// localFromRoot takes ownership of a handle acquired through the caller's
// pinned root. In particular, Manager must never reopen dir by host pathname.
func localFromRoot(dir string, root *os.Root, policy *Policy, opts Options) *Local {
	return &Local{dir: dir, root: root, policy: policy, opts: opts.withDefaults()}
}

func (l *Local) hook(stage string) {
	if l.testHook != nil {
		l.testHook(stage)
	}
}

// Dir returns the absolute host path of the checkout.
func (l *Local) Dir() string { return l.dir }

// MaxWriteBytes returns the allocation limit used by file editing tools.
func (l *Local) MaxWriteBytes() int64 { return l.opts.MaxWriteBytes }

// RunDir returns the per-run directory that holds the checkout, where the
// harness may keep other per-run files next to it. It is empty for a
// workspace that was not made by a Manager.
func (l *Local) RunDir() string { return l.runDir }

// Policy returns the path rules of the workspace.
func (l *Local) Policy() *Policy { return l.policy }

// Close releases the checkout. Unless keep is true, a checkout made by a
// Manager is deleted. Later calls return the result of the first.
func (l *Local) Close(keep bool) error {
	l.closeOnce.Do(func() {
		l.closeErr = l.root.Close()
		if l.onClose != nil {
			l.closeErr = errors.Join(l.closeErr, l.onClose(keep))
		}
	})
	return l.closeErr
}

// ReadFile returns the content of a regular file.
func (l *Local) ReadFile(name string) ([]byte, error) {
	const op = "read"
	r, err := l.resolve(name, false)
	if err != nil {
		return nil, l.fail(op, name, err)
	}
	if err := needRegular(r.info); err != nil {
		return nil, l.fail(op, name, err)
	}
	limit := l.opts.MaxReadBytes
	if r.info.Size() > limit {
		return nil, l.fail(op, name, tooLarge(r.info.Size(), limit))
	}
	pinned, err := l.pinDir(r, path.Dir(r.path), false)
	if err != nil {
		return nil, l.fail(op, name, err)
	}
	defer pinned.close()
	if err := pinned.validate(); err != nil {
		return nil, l.fail(op, name, err)
	}
	f, err := openRegular(pinned.root(), path.Base(r.path))
	if err != nil {
		return nil, l.fail(op, name, err)
	}
	defer func() { _ = f.Close() }()
	if err := pinned.checkFile(f); err != nil {
		return nil, l.fail(op, name, err)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, l.fail(op, name, err)
	}
	if int64(len(data)) > limit {
		return nil, l.fail(op, name, tooLarge(int64(len(data)), limit))
	}
	if err := pinned.checkFile(f); err != nil {
		return nil, l.fail(op, name, err)
	}
	return data, nil
}

// WriteFile creates or replaces a regular file. The content goes to a
// temporary file in the same directory, which is then renamed over the
// destination, so a reader sees the old content or the new and never a
// partial file. An existing file keeps its permission bits.
//
// The temporary file is not synced to disk: a checkout is rebuilt from the
// remote after a crash, so surviving one is not needed.
func (l *Local) WriteFile(name string, data []byte) error {
	const op = "write"
	if limit := l.opts.MaxWriteBytes; int64(len(data)) > limit {
		return l.fail(op, name, tooLarge(int64(len(data)), limit))
	}
	r, err := l.resolve(name, true)
	if err != nil {
		return l.fail(op, name, err)
	}
	if r.path == "." {
		return l.fail(op, name, &lexError{kind: ErrNotRegular, detail: "it is a directory"})
	}
	mode := newFileMode
	if r.info != nil {
		if err := needRegular(r.info); err != nil {
			return l.fail(op, name, err)
		}
		mode = r.info.Mode().Perm()
	}
	pinned, err := l.pinDir(r, path.Dir(r.path), true)
	if err != nil {
		return l.fail(op, name, err)
	}
	defer pinned.close()
	if err := pinned.validate(); err != nil {
		return l.fail(op, name, err)
	}
	validate := func() error { return pinned.validateTarget(path.Base(r.path)) }
	if err := validate(); err != nil {
		return l.fail(op, name, err)
	}
	if err := writeAtomic(pinned.root(), path.Base(r.path), data, mode, validate); err != nil {
		return l.fail(op, name, err)
	}
	return nil
}

// writeAtomic writes data to a temporary file in dir and renames it to base.
func writeAtomic(dir *os.Root, base string, data []byte, mode fs.FileMode, validate func() error) (err error) {
	tmp := ".patchbot-" + rand.Text() + ".tmp"
	f, err := dir.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = dir.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	// The destination was a regular file or missing when the path was
	// resolved. If it has become anything else, leave it alone. A rename
	// replaces the directory entry and never writes through a link.
	switch li, statErr := dir.Lstat(base); {
	case statErr == nil && !li.Mode().IsRegular():
		return &lexError{kind: ErrNotRegular, detail: "the destination changed during the write"}
	case statErr != nil && !errors.Is(statErr, fs.ErrNotExist):
		return statErr
	}
	if err := validate(); err != nil {
		return err
	}
	return dir.Rename(tmp, base)
}

// Stat describes the file a path resolves to.
func (l *Local) Stat(name string) (fs.FileInfo, error) {
	r, err := l.resolve(name, false)
	if err != nil {
		return nil, l.fail("stat", name, err)
	}
	dir := path.Dir(r.path)
	if r.info.IsDir() {
		dir = r.path
	}
	pinned, err := l.pinDir(r, dir, false)
	if err != nil {
		return nil, l.fail("stat", name, err)
	}
	defer pinned.close()
	if err := pinned.validate(); err != nil {
		return nil, l.fail("stat", name, err)
	}
	return r.info, nil
}

// ReadDir lists a directory sorted by name, without its protected entries.
func (l *Local) ReadDir(name string) ([]fs.DirEntry, error) {
	const op = "list"
	r, err := l.resolve(name, false)
	if err != nil {
		return nil, l.fail(op, name, err)
	}
	if !r.info.IsDir() {
		return nil, l.fail(op, name, ErrNotDir)
	}
	pinned, err := l.pinDir(r, r.path, false)
	if err != nil {
		return nil, l.fail(op, name, err)
	}
	defer pinned.close()
	if err := pinned.validate(); err != nil {
		return nil, l.fail(op, name, err)
	}
	dir := pinned.root()
	f, err := dir.Open(".")
	if err != nil {
		return nil, l.fail(op, name, err)
	}
	defer func() { _ = f.Close() }()

	limit := l.opts.MaxDirEntries
	var names []string
	for len(names) <= limit {
		batch, err := f.Readdirnames(limit + 1 - len(names))
		names = append(names, batch...)
		if errors.Is(err, io.EOF) || (err == nil && len(batch) == 0) {
			break
		}
		if err != nil {
			return nil, l.fail(op, name, err)
		}
	}
	truncated := len(names) > limit
	if truncated {
		names = names[:limit]
	}
	entries := make([]fs.DirEntry, 0, len(names))
	for _, child := range names {
		// A protected entry is left out. So is an entry whose name is not
		// a valid path here, since no tool could address it.
		if _, err := l.policy.Check(path.Join(r.path, child)); err != nil {
			continue
		}
		// The info comes from the pinned directory, not from the lazy
		// DirEntry.Info of package os, which looks the name up by host
		// path again.
		info, err := dir.Lstat(child)
		if err != nil {
			continue // removed since it was listed
		}
		entries = append(entries, fs.FileInfoToDirEntry(info))
	}
	if err := pinned.validate(); err != nil {
		return nil, l.fail(op, name, err)
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return cmp.Compare(a.Name(), b.Name()) })
	if truncated {
		return entries, l.fail(op, name, &lexError{
			kind:   ErrDirTruncated,
			detail: fmt.Sprintf("only %d entries were read", limit),
		})
	}
	return entries, nil
}

// resolved is the result of resolving a path inside the checkout.
type resolved struct {
	// path is clean, relative to the checkout and free of symlinks at the
	// time it was resolved.
	path string
	// info describes path. It is nil when the path does not exist yet,
	// which only happens when resolving for a write.
	info fs.FileInfo
	// dirs binds each existing directory to the dev+ino approved by resolve.
	dirs  map[string]fs.FileInfo
	guard protectedSet
}

// component is one name still to be resolved. fromLink marks a name that
// came from a symlink's target rather than from the caller.
type component struct {
	name     string
	fromLink bool
}

// resolve validates name and follows it through the checkout by hand. With
// create set, the tail of the path may be missing.
func (l *Local) resolve(name string, create bool) (resolved, error) {
	clean, err := l.policy.Check(name)
	if err != nil {
		return resolved{}, err
	}
	var pending []component
	if clean != "." {
		for part := range strings.SplitSeq(clean, "/") {
			pending = append(pending, component{name: part})
		}
	}
	guard := l.protectedFiles()
	rootInfo, err := l.root.Stat(".")
	if err != nil {
		return resolved{}, err
	}
	dirs := map[string]fs.FileInfo{".": rootInfo}

	var (
		stack   []string    // the resolved path so far
		info    fs.FileInfo // describes stack, when known
		hops    int
		missing bool // the rest of the path does not exist
	)
	for len(pending) > 0 {
		c := pending[0]
		pending = pending[1:]
		switch c.name {
		case "", ".":
			continue
		case "..":
			// Only a symlink target can hold "..". The stack is free of
			// symlinks, so going up is a plain pop.
			if len(stack) == 0 {
				return resolved{}, escapes("a symlink on the path points outside the repository")
			}
			stack, info = stack[:len(stack)-1], nil
			continue
		}
		candidate := c.name
		if len(stack) > 0 {
			candidate = strings.Join(stack, "/") + "/" + c.name
		}
		if rule, ok := l.policy.Protected(candidate); ok {
			return resolved{}, &lexError{kind: ErrPathProtected, detail: rule}
		}
		if missing {
			stack, info = append(stack, c.name), nil
			continue
		}
		fi, err := l.root.Lstat(candidate)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if c.fromLink {
				return resolved{}, &lexError{kind: ErrSymlink, detail: "a symlink on the path points to a missing file"}
			}
			if !create {
				return resolved{}, fs.ErrNotExist
			}
			missing = true
			stack, info = append(stack, c.name), nil
			continue
		case err != nil:
			return resolved{}, err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			hops++
			if hops > maxSymlinkHops {
				return resolved{}, &lexError{kind: ErrSymlink, detail: "too many symlinks on the path; it may be a loop"}
			}
			target, err := l.root.Readlink(candidate)
			if err != nil {
				return resolved{}, err
			}
			parts, err := linkTarget(target)
			if err != nil {
				return resolved{}, err
			}
			pending = append(parts, pending...)
			continue
		}
		if rule, ok := guard.match(fi); ok {
			return resolved{}, &lexError{kind: ErrPathProtected, detail: rule}
		}
		if fi.IsDir() {
			dirs[candidate] = fi
		}
		stack, info = append(stack, c.name), fi
	}

	out := resolved{path: ".", dirs: dirs, guard: guard}
	if len(stack) > 0 {
		out.path = strings.Join(stack, "/")
	}
	if missing {
		return out, nil
	}
	if info == nil {
		if info, err = l.root.Lstat(out.path); err != nil {
			return resolved{}, err
		}
	}
	out.info = info
	return out, nil
}

// linkTarget splits a symlink target into components to resolve.
func linkTarget(target string) ([]component, error) {
	slashed := filepath.ToSlash(target)
	switch {
	case target == "":
		return nil, &lexError{kind: ErrSymlink, detail: "a symlink on the path has an empty target"}
	case filepath.IsAbs(target) || strings.HasPrefix(slashed, "/") || filepath.VolumeName(target) != "":
		return nil, escapes("a symlink on the path points outside the repository")
	case len(target) > MaxPathBytes:
		return nil, &lexError{kind: ErrSymlink, detail: "a symlink on the path has an over-long target"}
	}
	var parts []component
	for part := range strings.SplitSeq(slashed, "/") {
		if part != "" && part != "." && part != ".." {
			if _, err := cleanPath(part); err != nil {
				return nil, err
			}
		}
		parts = append(parts, component{name: part, fromLink: true})
	}
	return parts, nil
}

// protectedSet holds the identity of each protected path that exists.
type protectedSet struct {
	infos []fs.FileInfo
	rules []string
}

// protectedFiles looks up the protected paths that have a fixed location:
// .git at the root and every prefix rule.
func (l *Local) protectedFiles() protectedSet {
	var set protectedSet
	add := func(name, rule string) {
		// Lstat describes the entry itself and Stat what it leads to. They
		// differ when the protected path is a symlink.
		if fi, err := l.root.Lstat(name); err == nil {
			set.infos, set.rules = append(set.infos, fi), append(set.rules, rule)
		}
		if fi, err := l.root.Stat(name); err == nil {
			set.infos, set.rules = append(set.infos, fi), append(set.rules, rule)
		}
	}
	add(".git", ".git")
	for _, r := range l.policy.rules {
		add(r.display, r.display)
	}
	return set
}

func (s protectedSet) match(fi fs.FileInfo) (rule string, ok bool) {
	for i, info := range s.infos {
		if os.SameFile(fi, info) {
			return s.rules[i], true
		}
	}
	return "", false
}

// openDirectory checks the entry without opening special files. The trailing
// "/." deliberately makes name an intermediate component of Root.OpenRoot:
// Go's Unix root walker opens it with O_DIRECTORY (unlike a terminal component).
// Thus even a swap to a FIFO after Lstat cannot block. The final "." refers to
// that already opened directory, and all acquisition stays relative to parent.
func openDirectory(parent *os.Root, name string, approved fs.FileInfo, beforeOpen func()) (*os.Root, error) {
	entry, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !entry.IsDir() {
		return nil, ErrNotDir
	}
	if !os.SameFile(entry, approved) {
		return nil, changedPath()
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	next, err := parent.OpenRoot(name + "/.")
	if err != nil {
		return nil, err
	}
	opened, err := next.Stat(".")
	if err == nil && !os.SameFile(opened, approved) {
		err = changedPath()
	}
	entry, statErr := parent.Lstat(name)
	if err == nil {
		err = statErr
		if err == nil && (!entry.IsDir() || !os.SameFile(entry, approved)) {
			err = changedPath()
		}
	}
	if err != nil {
		_ = next.Close()
		return nil, err
	}
	return next, nil
}

func changedPath() error {
	return &lexError{kind: ErrSymlink, detail: "the path changed during the operation"}
}

type pinnedLevel struct {
	root *os.Root
	path string
	info fs.FileInfo
}

type pinnedDir struct {
	local  *Local
	levels []pinnedLevel // retain parents so namespace edges can be rechecked
	guard  protectedSet
}

func (p *pinnedDir) root() *os.Root { return p.levels[len(p.levels)-1].root }

func (p *pinnedDir) close() {
	for i := len(p.levels) - 1; i > 0; i-- {
		_ = p.levels[i].root.Close()
	}
}

// validate rechecks both the approved directory identities and their namespace
// edges, plus protected identities captured during resolution and at use time.
// A retained descriptor must not become an alias of a protected directory.
func (p *pinnedDir) validate() error {
	current := p.local.protectedFiles()
	for i, level := range p.levels {
		opened, err := level.root.Stat(".")
		if err != nil {
			return err
		}
		if !os.SameFile(opened, level.info) {
			return changedPath()
		}
		if i == 0 {
			continue // the checkout root itself is the jail boundary
		}
		for _, guard := range []protectedSet{p.guard, current} {
			if rule, ok := guard.match(opened); ok {
				return &lexError{kind: ErrPathProtected, detail: rule}
			}
		}
		entry, err := p.levels[i-1].root.Lstat(path.Base(level.path))
		if err != nil {
			return err
		}
		if !entry.IsDir() || !os.SameFile(entry, level.info) {
			return changedPath()
		}
	}
	return nil
}

func (p *pinnedDir) checkFile(f *os.File) error {
	if err := p.validate(); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	for _, guard := range []protectedSet{p.guard, p.local.protectedFiles()} {
		if rule, ok := guard.match(info); ok {
			return &lexError{kind: ErrPathProtected, detail: rule}
		}
	}
	return nil
}

// validateTarget also checks a destination that has appeared or changed since
// resolution, including aliases of configured protected regular files.
func (p *pinnedDir) validateTarget(base string) error {
	if err := p.validate(); err != nil {
		return err
	}
	info, err := p.root().Lstat(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, guard := range []protectedSet{p.guard, p.local.protectedFiles()} {
		if rule, ok := guard.match(info); ok {
			return &lexError{kind: ErrPathProtected, detail: rule}
		}
	}
	return nil
}

// pinDir binds every acquired handle to the identities approved by resolve.
// A missing directory is approved only after this operation creates it.
func (l *Local) pinDir(r resolved, dir string, create bool) (_ *pinnedDir, err error) {
	l.hook("resolved")
	p := &pinnedDir{local: l, guard: r.guard, levels: []pinnedLevel{{root: l.root, path: ".", info: r.dirs["."]}}}
	defer func() {
		if err != nil {
			p.close()
		}
	}()
	if dir != "." {
		full := ""
		for name := range strings.SplitSeq(dir, "/") {
			full = path.Join(full, name)
			cur := p.root()
			approved := r.dirs[full]
			if approved == nil {
				if !create {
					return nil, changedPath()
				}
				// Do not adopt an entry planted since resolution. If another
				// operation created it, the caller can retry from resolution.
				if err := p.validate(); err != nil {
					return nil, err
				}
				if err := cur.Mkdir(name, newDirMode); err != nil {
					return nil, err
				}
				approved, err = cur.Lstat(name)
				if err != nil {
					return nil, err
				}
			}
			next, err := openDirectory(cur, name, approved, func() { l.hook("before-open-dir") })
			if err != nil {
				return nil, err
			}
			p.levels = append(p.levels, pinnedLevel{root: next, path: full, info: approved})
			if err := p.validate(); err != nil {
				return nil, err
			}
		}
	}
	l.hook("pinned")
	return p, nil
}

// openAttempts is how often openRegular tries again when the file is
// replaced between its check and its open, as an atomic write by another
// goroutine or a test command does.
const openAttempts = 16

// openRegular opens a regular file in a pinned directory without following
// a symlink. os.Root follows symlinks that stay inside the checkout and has
// no way to turn that off, so the name is checked first to be a regular
// file, and the opened file must be that same file. If the name was swapped
// for a symlink in between, the identities differ and the file is dropped.
func openRegular(dir *os.Root, base string) (*os.File, error) {
	for range openAttempts {
		entry, err := dir.Lstat(base)
		if err != nil {
			return nil, err
		}
		if err := needRegular(entry); err != nil {
			if entry.Mode()&fs.ModeSymlink != 0 {
				break
			}
			return nil, err
		}
		// O_NONBLOCK keeps the open from hanging if the file was swapped
		// for a FIFO. It has no effect on a regular file.
		f, err := dir.OpenFile(base, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		opened, err := f.Stat()
		if err == nil && os.SameFile(entry, opened) {
			return f, nil
		}
		_ = f.Close()
		if err != nil {
			return nil, err
		}
	}
	return nil, &lexError{kind: ErrSymlink, detail: "the path changed during the operation"}
}

func needRegular(info fs.FileInfo) error {
	switch {
	case info.IsDir():
		return &lexError{kind: ErrNotRegular, detail: "it is a directory"}
	case !info.Mode().IsRegular():
		return &lexError{kind: ErrNotRegular, detail: "it is a device, FIFO, socket or other special file"}
	}
	return nil
}

func tooLarge(size, limit int64) error {
	return &lexError{kind: ErrTooLarge, detail: fmt.Sprintf("%d bytes, the limit is %d", size, limit)}
}

// fail turns an internal error into the PathError a caller sees.
func (l *Local) fail(op, name string, err error) error {
	var le *lexError
	if errors.As(err, &le) {
		return pathErr(op, name, le.kind, le.detail)
	}
	return pathErr(op, name, err, "")
}
