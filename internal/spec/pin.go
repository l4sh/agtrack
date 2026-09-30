package spec

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/l4sh/agtrack/internal/errs"
)

// PinComment is the line InsertPin writes, without its line ending.
func PinComment(key string) string { return "<!-- agtrack:id " + key + " -->" }

// InsertPin splices a pin comment line for key immediately above sec's
// heading. It is a byte-level splice into src (which must be the Source sec
// was parsed from): nothing else in the document changes, and the inserted
// line uses the heading line's own ending (LF or CRLF).
//
// It is idempotent: a section already pinned as key returns src unchanged, as
// does the preamble, whose identity is structural. A section pinned under a
// different key, or a block (whose key is its anchor), is an error.
func InsertPin(src []byte, sec *Section, key string) ([]byte, error) {
	if !ValidKey(key) {
		return nil, errs.New("invalid pin id %q: use letters, digits, '_', '.' or '-', starting with a letter or digit", key)
	}
	if sec.PinKey == key || (sec.Kind == KindRoot && sec.Level == 0) {
		return src, nil
	}
	if sec.PinKey != "" {
		return nil, errs.Conflict("section %s is already pinned as %q", sec.PathSlug, sec.PinKey)
	}
	if sec.Kind != KindHeading && sec.Kind != KindRoot {
		return nil, errs.New("section %s is a block; its id is its anchor key", sec.PathSlug)
	}
	off := sec.HeadingByteStart
	cur, err := Parse(src)
	if err != nil {
		return nil, err
	}
	if c := cur.Section(sec.Ordinal); c == nil || c.PathSlug != sec.PathSlug || c.HeadingByteStart != off {
		return nil, errs.Conflict("section %s is not where it was parsed; re-parse before pinning", sec.PathSlug)
	}
	eol := "\n"
	if i := bytes.IndexByte(src[off:], '\n'); i > 0 && src[off+i-1] == '\r' {
		eol = "\r\n"
	} else if i < 0 && bytes.IndexByte(src[off:], '\r') >= 0 {
		eol = "\r"
	}
	line := PinComment(key) + eol
	out := make([]byte, 0, len(src)+len(line))
	out = append(out, src[:off]...)
	out = append(out, line...)
	out = append(out, src[off:]...)
	// The splice must yield exactly this pin on this section; anything else
	// means the heading sits where a comment line would change its meaning.
	next, err := Parse(out)
	if err != nil {
		return nil, err
	}
	if c := next.Section(sec.Ordinal); c == nil || c.PinKey != key || c.PathSlug != sec.PathSlug {
		return nil, errs.New("cannot pin section %s: a comment line above its heading would not attach to it", sec.PathSlug)
	}
	return out, nil
}

// WriteFileAtomic replaces path with data, but only if the file still hashes
// (normalised, as SHA does) to expectSHA; otherwise it returns a conflict and
// touches nothing. An empty expectSHA skips the check. The data goes to a
// temp file in the same directory, is fsynced and renamed over path, so a
// failure never leaves a half-written document; the original mode is kept.
func WriteFileAtomic(path string, data []byte, expectSHA string) error {
	mode := fs.FileMode(0o644)
	cur, err := os.ReadFile(path)
	switch {
	case err == nil:
		if expectSHA != "" && SHA(cur) != expectSHA {
			return errs.Conflict("%s changed since it was read; re-run the command to pick up the new content", path)
		}
		if fi, err := os.Stat(path); err == nil {
			mode = fi.Mode().Perm()
		}
	case errors.Is(err, fs.ErrNotExist):
		if expectSHA != "" {
			return errs.Conflict("%s was removed since it was read", path)
		}
	default:
		return errs.New("cannot read %s: %v", path, err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".agtrack-*")
	if err != nil {
		return errs.New("cannot write %s: %v", path, err)
	}
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(name)
		return errs.New("cannot write %s: %v", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return errs.New("cannot write %s: %v", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return errs.New("cannot write %s: %v", path, err)
	}
	if df, err := os.Open(dir); err == nil {
		_ = df.Sync() // best effort: make the rename itself durable
		df.Close()
	}
	return nil
}
