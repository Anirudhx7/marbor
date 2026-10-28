package admin

// backup_verify.go - reading a backup file back to say whether it is good, and
// the cache that keeps unchanged files from being re-read.
//
// A file is verified through one open descriptor: the same descriptor is used
// to sniff it, read the fingerprint record at its end, decompress or hash it and
// stat it, so a file swapped in under the path while it is being read can never
// be judged by another file's bytes. "ok" means the gzip checksums of every
// member pass, the decompressed bytes start with a sane SQLite header at least
// as long as the header claims, and the fingerprint record at the end of the
// file matches the content. A restore runs the full SQLite integrity and schema
// check on top of that before it is accepted.
//
// Results are cached by path and trusted only while the file's size,
// modification time and fingerprint record are unchanged, its identity (the
// same file, not a replacement) matches, and the result is under a day old: a
// file that rots on disk without its modification time changing is caught
// within a day. The cache lives in memory only and is bounded (least recently
// used goes first). It is deliberately not saved to disk: a saved file would be
// unauthenticated and could not carry a file's identity, so after a restart
// every file is read again rather than trusted on someone's say-so.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Anirudhx7/marbor/internal/store"
)

const (
	verifiedCacheMax = 256
	verifiedMaxAge   = 24 * time.Hour

	reasonNoTail   = "the fingerprint record at the end of the file does not match its content"
	reasonNotFile  = "not a regular file"
	reasonNotBackp = "not a SQLite database or a gzip of one"
	reasonName     = "the file name says one format but the content is the other"
)

// verifiedEntry is what reading one backup file in full showed. reason is empty
// for a good file.
type verifiedEntry struct {
	size   int64
	mod    time.Time
	tail   string // the fingerprint record found at the end of a gzip file, "" if none
	fp     string // canonical fingerprint of the content, computed here
	raw    int64
	reason string // why the file is not a good backup
	at     time.Time

	id   os.FileInfo // identity of the file that was read
	used uint64      // LRU clock value of the last use
}

// verifiedBackups remembers full-verification results.
type verifiedBackups struct {
	mu    sync.Mutex
	m     map[string]*verifiedEntry
	clock uint64
	dir   string           // the directory the entries belong to
	now   func() time.Time // replaceable in tests
}

func (v *verifiedBackups) nowLocked() time.Time {
	if v.now != nil {
		return v.now()
	}
	return time.Now()
}

// switchDirLocked points the cache at dir, dropping the entries of another
// directory. The caller holds v.mu.
func (v *verifiedBackups) switchDirLocked(dir string) {
	if v.m != nil && v.dir == dir {
		return
	}
	v.dir, v.m = dir, map[string]*verifiedEntry{}
}

// get returns the cached result for path when it still describes the file
// that fi and tail describe.
func (v *verifiedBackups) get(path string, fi os.FileInfo, tail string) (verifiedEntry, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.switchDirLocked(filepath.Dir(path))
	e, ok := v.m[path]
	if !ok || e.size != fi.Size() || !e.mod.Equal(fi.ModTime()) || e.tail != tail {
		return verifiedEntry{}, false
	}
	if !os.SameFile(e.id, fi) {
		return verifiedEntry{}, false
	}
	if v.nowLocked().Sub(e.at) >= verifiedMaxAge {
		return verifiedEntry{}, false
	}
	v.clock++
	e.used = v.clock
	return *e, true
}

// put stores a result, evicting the least recently used entry when full.
func (v *verifiedBackups) put(path string, fi os.FileInfo, tail string, e verifiedEntry) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.switchDirLocked(filepath.Dir(path))
	if _, exists := v.m[path]; !exists && len(v.m) >= verifiedCacheMax {
		var oldest string
		var oldestUsed uint64
		for p, x := range v.m {
			if oldest == "" || x.used < oldestUsed {
				oldest, oldestUsed = p, x.used
			}
		}
		delete(v.m, oldest)
	}
	v.clock++
	e.size, e.mod, e.tail, e.at, e.id, e.used = fi.Size(), fi.ModTime(), tail, v.nowLocked(), fi, v.clock
	v.m[path] = &e
}

func (v *verifiedBackups) forget(path string) {
	v.mu.Lock()
	delete(v.m, path)
	v.mu.Unlock()
}

// noteVerified records a file this process just wrote and verified, so it is
// not read again. tail is the fingerprint for a gzip file written here.
func (s *Server) noteVerified(path, fp string, raw int64) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return
	}
	tail := ""
	if strings.HasSuffix(path, ".gz") {
		tail, _ = store.ReadFingerprintTailFile(f)
	}
	s.backupVerified.put(path, fi, tail, verifiedEntry{fp: fp, raw: raw})
}

// verifyLimit is how far a compressed file may expand while it is only being
// read (nothing is written, so free space does not apply).
// It fails closed: when the limit cannot be worked out, the file is not read at
// all (an error, shown as "could not be verified") rather than read against a
// guessed bound.
func (s *Server) verifyLimit(compressedSize int64) (int64, error) {
	// No live-database floor here: that floor exists so a restore of a highly
	// compressible database is not refused, and verification must apply the plain
	// expansion bound instead so it cannot read further than a bomb check allows.
	limit, derr := store.DecompressLimit(store.DecompressPlan{
		FreeBytes:       1 << 62,
		CompressedBytes: compressedSize,
		HardCap:         maxDecompressedBackupBytes,
	})
	if derr != nil {
		log.Printf("admin: could not work out the bound for a backup verification: %v", derr)
		return 0, fmt.Errorf("could not work out how far the backup may expand: %w", derr)
	}
	return limit, nil
}

// isFinalBackupPath reports whether path has the name of a kept backup. Only such
// paths are cached: a temporary or scratch name never is, because the file is
// about to be renamed or deleted and the entry would describe nothing.
func isFinalBackupPath(path string) bool {
	return backupFilenameRE.MatchString(filepath.Base(path))
}

// verifyBackupFile reads the backup at path in full and reports what it found.
// A file that is not a good backup is a normal result (entry.reason set), not
// an error; an error means the check itself could not finish (cancelled, an I/O
// failure on a healthy file, or the file changing while it was read). Results
// are cached.
func (s *Server) verifyBackupFile(ctx context.Context, path string) (verifiedEntry, error) {
	return s.verifyBackupFileOpts(ctx, path, true)
}

// verifyBackupFileOpts is verifyBackupFile with the cache switched on or off.
// A file that does not have the name of a kept backup (a temporary write, a
// manual scratch build) is never cached whatever useCache says.
func (s *Server) verifyBackupFileOpts(ctx context.Context, path string, useCache bool) (verifiedEntry, error) {
	useCache = useCache && isFinalBackupPath(path)
	f, fi, err := openBackupFile(path)
	if err != nil {
		if errors.Is(err, errNotRegularBackup) {
			return verifiedEntry{reason: reasonNotFile}, nil
		}
		return verifiedEntry{}, err
	}
	defer f.Close()
	kind, serr := store.SniffBackupReader(f)
	if serr != nil {
		return verifiedEntry{}, serr
	}
	tail := ""
	if kind == store.BackupKindGzip {
		tail, _ = store.ReadFingerprintTailFile(f)
	}
	if useCache {
		if e, ok := s.backupVerified.get(path, fi, tail); ok {
			return e, nil
		}
	}
	var e verifiedEntry
	var bi *store.BackupInvalidError
	named := isFinalBackupPath(path)
	wantGz := strings.HasSuffix(path, ".gz")
	switch {
	case named && kind == store.BackupKindGzip && !wantGz, named && kind == store.BackupKindPlain && wantGz:
		e = verifiedEntry{reason: reasonName}
	case kind == store.BackupKindGzip:
		limit, lerr := s.verifyLimit(fi.Size())
		if lerr != nil {
			return verifiedEntry{}, lerr
		}
		fp, raw, gerr := store.FingerprintGzipFile(ctx, f, limit)
		switch {
		case gerr == nil && tail != "" && tail != fp:
			// A tail that does not match the content is wrong. A missing tail is
			// not: a restore decompresses by content and never reads it, so an
			// intact stream without one (made by the gzip tool) is a good backup.
			e = verifiedEntry{reason: reasonNoTail}
		case gerr == nil:
			e = verifiedEntry{fp: fp, raw: raw}
		case errors.As(gerr, &bi):
			e = verifiedEntry{reason: bi.Reason}
		default:
			return verifiedEntry{}, gerr
		}
	case kind == store.BackupKindPlain:
		if herr := store.CheckPlainBackupHeaderFile(f); herr != nil {
			if errors.As(herr, &bi) {
				e = verifiedEntry{reason: bi.Reason}
				break
			}
			return verifiedEntry{}, herr
		}
		// The header says little about the pages: run the same integrity and schema
		// check a restore runs, so a file with a good header and damaged content is
		// not counted as a kept backup.
		if verr := store.ValidateBackupFileContext(ctx, path); verr != nil {
			if errors.As(verr, &bi) {
				e = verifiedEntry{reason: bi.Reason}
				break
			}
			return verifiedEntry{}, verr
		}
		fp, ferr := store.FileFingerprintFile(ctx, f)
		if ferr != nil {
			return verifiedEntry{}, ferr
		}
		e = verifiedEntry{fp: fp, raw: fi.Size()}
	default:
		e = verifiedEntry{reason: reasonNotBackp}
	}
	after, err := f.Stat()
	if err != nil {
		return verifiedEntry{}, err
	}
	if after.Size() != fi.Size() || !after.ModTime().Equal(fi.ModTime()) {
		return verifiedEntry{}, fmt.Errorf("the backup file changed while it was being verified")
	}
	if useCache {
		s.backupVerified.put(path, fi, tail, e)
	}
	return e, nil
}

// integrityOf is the cheap integrity label for the list: a cached full
// verification when there is one, otherwise structural checks only. A
// compressed file without a fingerprint record is not judged here: the full
// verification decides by content.
func (s *Server) integrityOf(path, name string) string {
	f, fi, err := openBackupFile(path) // the safe opener: no symlink, regular files only
	if err != nil {
		return integrityInvalid
	}
	defer f.Close()
	kind, err := store.SniffBackupReader(f)
	if err != nil {
		return integrityInvalid
	}
	tail := ""
	if kind == store.BackupKindGzip {
		tail, _ = store.ReadFingerprintTailFile(f)
	}
	if e, ok := s.backupVerified.get(path, fi, tail); ok {
		if e.reason != "" {
			return integrityInvalid
		}
		return integrityOK
	}
	switch {
	case strings.HasSuffix(name, ".gz") && kind == store.BackupKindGzip:
		return integrityUnverified
	case !strings.HasSuffix(name, ".gz") && kind == store.BackupKindPlain:
		if store.CheckPlainBackupHeaderFile(f) != nil {
			return integrityInvalid
		}
		return integrityUnverified
	default:
		return integrityInvalid
	}
}
