// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package taildrop contains the implementation of the Taildrop
// functionality including sending and retrieving files.
// This package does not validate permissions, the caller should
// be responsible for ensuring correct authorization.
//
// For related documentation see: http://go/taildrop-how-does-it-work
package taildrop

import (
	"errors"
	"hash/adler32"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"tailscale.com/ipn"
	"tailscale.com/syncs"
	"tailscale.com/tstime"
	"tailscale.com/types/logger"
	"tailscale.com/util/multierr"
)

// ClientID is an opaque identifier for file resumption.
// A client can only list and resume partial files for its own ID.
// It must contain any filesystem specific characters (e.g., slashes).
type ClientID string // e.g., "n12345CNTRL"

func (id ClientID) partialSuffix() string {
	if id == "" {
		return partialSuffix
	}
	return "." + string(id) + partialSuffix // e.g., ".n12345CNTRL.partial"
}

// Manager manages the state for receiving and managing taildropped files.
type Manager struct {
	Logf  logger.Logf
	Clock tstime.Clock

	// Dir is the directory to store received files.
	// This main either be the final location for the files
	// or just a temporary staging directory (see DirectFileMode).
	Dir string

	// DirectFileMode reports whether we are writing files
	// directly to a download directory, rather than writing them to
	// a temporary staging directory.
	//
	// The following methods:
	//	- HasFilesWaiting
	//	- WaitingFiles
	//	- DeleteFile
	//	- OpenFile
	// have no purpose in DirectFileMode.
	// They are only used to check whether files are in the staging directory,
	// copy them out, and then delete them.
	DirectFileMode bool

	// AvoidFinalRename specifies whether in DirectFileMode
	// we should avoid renaming "foo.jpg.partial" to "foo.jpg" after reception.
	//
	// TODO(joetsai,rhea): Delete this. This is currently depended upon
	// in the Apple platforms since it violates the abstraction layer
	// and directly assumes how taildrop represents partial files.
	// Right now, file resumption does not work on Apple.
	AvoidFinalRename bool

	// SendFileNotify is called periodically while a file is actively
	// receiving the contents for the file. There is a final call
	// to the function when reception completes.
	// It is not called if nil.
	SendFileNotify func()

	knownEmpty atomic.Bool

	incomingFiles syncs.Map[incomingFileKey, *incomingFile]

	// renameMu is used to protect os.Rename calls so that they are atomic.
	renameMu sync.Mutex
}

var (
	ErrNoTaildrop      = errors.New("Taildrop disabled; no storage directory")
	ErrInvalidFileName = errors.New("invalid filename")
	ErrFileExists      = errors.New("file already exists")
	ErrNotAccessible   = errors.New("Taildrop folder not configured or accessible")
)

const (
	// partialSuffix is the suffix appended to files while they're
	// still in the process of being transferred.
	partialSuffix = ".partial"

	// deletedSuffix is the suffix for a deleted marker file
	// that's placed next to a file (without the suffix) that we
	// tried to delete, but Windows wouldn't let us. These are
	// only written on Windows (and in tests), but they're not
	// permitted to be uploaded directly on any platform, like
	// partial files.
	deletedSuffix = ".deleted"
)

// redacted is a fake path name we use in errors, to avoid
// accidentally logging actual filenames anywhere.
const redacted = "redacted"

func validFilenameRune(r rune) bool {
	switch r {
	case '/':
		return false
	case '\\', ':', '*', '"', '<', '>', '|':
		// Invalid stuff on Windows, but we reject them everywhere
		// for now.
		// TODO(bradfitz): figure out a better plan. We initially just
		// wrote things to disk URL path-escaped, but that's gross
		// when debugging, and just moves the problem to callers.
		// So now we put the UTF-8 filenames on disk directly as
		// sent.
		return false
	}
	return unicode.IsPrint(r)
}

func (m *Manager) joinDir(baseName string) (fullPath string, ok bool) {
	if !utf8.ValidString(baseName) {
		return "", false
	}
	if strings.TrimSpace(baseName) != baseName {
		return "", false
	}
	if len(baseName) > 255 {
		return "", false
	}
	// TODO: validate unicode normalization form too? Varies by platform.
	clean := path.Clean(baseName)
	if clean != baseName ||
		clean == "." || clean == ".." ||
		strings.HasSuffix(clean, deletedSuffix) ||
		strings.HasSuffix(clean, partialSuffix) {
		return "", false
	}
	for _, r := range baseName {
		if !validFilenameRune(r) {
			return "", false
		}
	}
	if !filepath.IsLocal(baseName) {
		return "", false
	}
	return filepath.Join(m.Dir, baseName), true
}

// IncomingFiles returns a list of active incoming files.
func (m *Manager) IncomingFiles() []ipn.PartialFile {
	// Make sure we always set n.IncomingFiles non-nil so it gets encoded
	// in JSON to clients. They distinguish between empty and non-nil
	// to know whether a Notify should be able about files.
	files := make([]ipn.PartialFile, 0)
	m.incomingFiles.Range(func(k incomingFileKey, f *incomingFile) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		files = append(files, ipn.PartialFile{
			Name:         k.name,
			Started:      f.started,
			DeclaredSize: f.size,
			Received:     f.copied,
			PartialPath:  f.partialPath,
			Done:         f.done,
		})
		return true
	})
	return files
}

type redactedErr struct {
	msg   string
	inner error
}

func (re *redactedErr) Error() string {
	return re.msg
}

func (re *redactedErr) Unwrap() error {
	return re.inner
}

func redactString(s string) string {
	hash := adler32.Checksum([]byte(s))

	var buf [len(redacted) + len(".12345678")]byte
	b := append(buf[:0], []byte(redacted)...)
	b = append(b, '.')
	b = strconv.AppendUint(b, uint64(hash), 16)
	return string(b)
}

func redactErr(root error) error {
	// redactStrings is a list of sensitive strings that were redacted.
	// It is not sufficient to just snub out sensitive fields in Go errors
	// since some wrapper errors like fmt.Errorf pre-cache the error string,
	// which would unfortunately remain unaffected.
	var redactStrings []string

	// Redact sensitive fields in known Go error types.
	var unknownErrors int
	multierr.Range(root, func(err error) bool {
		switch err := err.(type) {
		case *os.PathError:
			redactStrings = append(redactStrings, err.Path)
			err.Path = redactString(err.Path)
		case *os.LinkError:
			redactStrings = append(redactStrings, err.New, err.Old)
			err.New = redactString(err.New)
			err.Old = redactString(err.Old)
		default:
			unknownErrors++
		}
		return true
	})

	// If there are no redacted strings or no unknown error types,
	// then we can return the possibly modified root error verbatim.
	// Otherwise, we must replace redacted strings from any wrappers.
	if len(redactStrings) == 0 || unknownErrors == 0 {
		return root
	}

	// Stringify and replace any paths that we found above, then return
	// the error wrapped in a type that uses the newly-redacted string
	// while also allowing Unwrap()-ing to the inner error type(s).
	s := root.Error()
	for _, toRedact := range redactStrings {
		s = strings.ReplaceAll(s, toRedact, redactString(toRedact))
	}
	return &redactedErr{msg: s, inner: root}
}

var (
	rxExtensionSuffix = regexp.MustCompile(`(\.[a-zA-Z0-9]{0,3}[a-zA-Z][a-zA-Z0-9]{0,3})*$`)
	rxNumberSuffix    = regexp.MustCompile(` \([0-9]+\)`)
)

// NextFilename returns the next filename in a sequence.
// It is used for construction a new filename if there is a conflict.
//
// For example, "Foo.jpg" becomes "Foo (1).jpg" and
// "Foo (1).jpg" becomes "Foo (2).jpg".
func NextFilename(name string) string {
	ext := rxExtensionSuffix.FindString(strings.TrimPrefix(name, "."))
	name = strings.TrimSuffix(name, ext)
	var n uint64
	if rxNumberSuffix.MatchString(name) {
		i := strings.LastIndex(name, " (")
		if n, _ = strconv.ParseUint(name[i+len("( "):len(name)-len(")")], 10, 64); n > 0 {
			name = name[:i]
		}
	}
	return name + " (" + strconv.FormatUint(n+1, 10) + ")" + ext
}
