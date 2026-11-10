// Package workspace holds the checkout a run works in and the path jail that
// every file tool goes through. All access is relative to an os.Root opened
// on the checkout, so no path can leave it, and a policy on top keeps the
// agent away from protected paths such as .git and .env files.
//
// Git operations (clone, diff, commit, push) are not part of this package
// yet.
package workspace

import (
	"errors"
	"io/fs"
	"os"
	"strconv"
)

// Workspace is the jailed view of one checkout. Every path argument is a
// relative, slash-separated path inside the checkout; "." is the checkout
// itself. Every method applies the same rules: the path must stay inside the
// checkout and must not be, or lead through a symlink to, a protected path.
type Workspace interface {
	// Dir returns the absolute host path of the checkout. It is for the
	// harness (git, the executor), never for the model.
	Dir() string
	// ReadFile returns the content of a regular file, up to the read limit.
	ReadFile(path string) ([]byte, error)
	// WriteFile creates or replaces a regular file atomically, creating
	// missing parent directories. An existing file keeps its mode.
	WriteFile(path string, data []byte) error
	// Stat describes the file a path resolves to. Symlinks inside the
	// checkout are followed.
	Stat(path string) (fs.FileInfo, error)
	// ReadDir lists a directory sorted by name. Protected entries are left
	// out. Symlink entries are reported as symlinks, not followed. When the
	// directory has more entries than the limit, it returns the entries it
	// read together with an error that wraps ErrDirTruncated.
	ReadDir(path string) ([]fs.DirEntry, error)
	// Close releases the checkout. Unless keep is true, a checkout made by
	// a Manager is deleted.
	Close(keep bool) error
}

// The errors a Workspace reports. Each comes wrapped in a *PathError.
var (
	// ErrInvalidPath means the path is malformed: empty, too long, or
	// holding a NUL, a control character or a backslash.
	ErrInvalidPath = errors.New("invalid path")
	// ErrPathEscape means the path points outside the checkout.
	ErrPathEscape = errors.New("path escapes workspace")
	// ErrPathProtected means the path is, or resolves to, a protected path.
	ErrPathProtected = errors.New("path is protected")
	// ErrSymlink means a symlink on the path cannot be followed safely: it
	// is dangling, part of a loop, or changed while it was being used.
	ErrSymlink = errors.New("unsafe symlink")
	// ErrNotRegular means the path is not the kind of file the operation
	// needs, such as a directory, device, FIFO or socket given to ReadFile.
	ErrNotRegular = errors.New("not a regular file")
	// ErrNotDir means a directory was needed and something else was found.
	ErrNotDir = errors.New("not a directory")
	// ErrTooLarge means a read or write is over the size limit.
	ErrTooLarge = errors.New("file too large")
	// ErrDirTruncated means a directory has more entries than the limit.
	ErrDirTruncated = errors.New("directory listing truncated")
)

// PathError is the error type of every Workspace method. Its text names the
// path as the caller gave it and never holds a host path, so it is safe to
// show to the model.
type PathError struct {
	Op   string // read, write, stat or list
	Path string // the path as the caller gave it
	Err  error  // one of the errors above, or an operating-system error
	// Detail says more about Err, such as which rule protected the path.
	Detail string
}

func (e *PathError) Error() string {
	msg := e.Op + " " + strconv.Quote(e.Path) + ": " + e.Err.Error()
	if e.Detail != "" {
		msg += " (" + e.Detail + ")"
	}
	return msg
}

func (e *PathError) Unwrap() error { return e.Err }

// pathErr builds a PathError. An operating-system error is reduced to its
// cause, because the wrapper os adds may name a host path.
func pathErr(op, path string, err error, detail string) *PathError {
	var existing *PathError
	if errors.As(err, &existing) {
		return &PathError{Op: op, Path: path, Err: existing.Err, Detail: existing.Detail}
	}
	var pe *fs.PathError
	var le *os.LinkError
	switch {
	case errors.As(err, &pe):
		err = pe.Err
	case errors.As(err, &le):
		err = le.Err
	}
	return &PathError{Op: op, Path: path, Err: err, Detail: detail}
}

// The default size limits.
const (
	DefaultMaxReadBytes  = 4 << 20
	DefaultMaxWriteBytes = 1 << 20
	DefaultMaxDirEntries = 10000
)

// Options configures a workspace. A zero limit means the default.
type Options struct {
	// Protected lists extra repository-relative paths the agent may neither
	// read nor write, on top of the built-in ones. A path covers itself and
	// everything below it.
	Protected []string
	// MaxReadBytes is the largest file ReadFile returns.
	MaxReadBytes int64
	// MaxWriteBytes is the largest file WriteFile accepts.
	MaxWriteBytes int64
	// MaxDirEntries is the most entries ReadDir reads from one directory.
	MaxDirEntries int
}

func (o Options) withDefaults() Options {
	if o.MaxReadBytes <= 0 {
		o.MaxReadBytes = DefaultMaxReadBytes
	}
	if o.MaxWriteBytes <= 0 {
		o.MaxWriteBytes = DefaultMaxWriteBytes
	}
	if o.MaxDirEntries <= 0 {
		o.MaxDirEntries = DefaultMaxDirEntries
	}
	return o
}
