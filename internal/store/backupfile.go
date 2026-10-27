package store

// backupfile.go - gzip handling for marbor.db backups. Everything here
// streams: nothing ever holds a whole database in memory, and the standard
// library's compress/gzip is the only dependency.

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"runtime"
)

// BackupGzipLevel is the compression level used for every compressed backup.
var BackupGzipLevel = gzip.DefaultCompression

// ErrBackupTooLarge is returned (wrapped in a *BackupInvalidError) when a
// compressed backup would expand past the caller's decompressed-size cap (a
// gzip bomb, or simply not enough room).
var ErrBackupTooLarge = errors.New("backup is too large once decompressed")

const (
	// MaxBackupExpansionRatio bounds how far a compressed backup may expand
	// relative to its compressed size. Measured compression of marbor.db is about
	// 4.5x; 50x leaves a wide margin for unusually repetitive data while still
	// refusing a gzip bomb (deflate can reach about 1000x).
	MaxBackupExpansionRatio = 50

	// MaxGzipMembers caps how many gzip members a backup may hold. A marbor
	// backup has two (the data and the fingerprint tail); a file made by the
	// gzip tool has one. A stream of millions of empty members costs CPU while
	// producing nothing, so anything beyond this is refused.
	MaxGzipMembers = 16
)

// BackupInvalidError is a problem with a candidate backup file. Reason is a
// fixed, human-readable sentence that never contains a filesystem path or an
// OS error text, so it is safe to show to the operator; Err carries the full
// detail for the server log.
type BackupInvalidError struct {
	Reason string
	Err    error
}

func (e *BackupInvalidError) Error() string {
	if e.Err != nil {
		return e.Reason + ": " + e.Err.Error()
	}
	return e.Reason
}

func (e *BackupInvalidError) Unwrap() error { return e.Err }

func invalidBackup(reason string, err error) error {
	return &BackupInvalidError{Reason: reason, Err: err}
}

// NoRoomError means a volume does not have the space an operation needs. Its
// message holds only sizes, so it is safe to show to the operator.
type NoRoomError struct {
	Free   uint64
	Needed uint64
}

func (e *NoRoomError) Error() string {
	return fmt.Sprintf("not enough free disk space: %d MiB free, at least %d MiB spare space is required", e.Free>>20, e.Needed>>20)
}

// DecompressPlan describes what limits a decompression.
type DecompressPlan struct {
	FreeBytes       uint64 // free space on the destination volume
	MarginBytes     uint64 // space that must stay free on that volume afterwards
	CompressedBytes int64  // size of the compressed file; <= 0 when unknown
	LiveDBBytes     int64  // size of the live database; lower bound for the ratio cap
	HardCap         int64  // absolute ceiling
}

// DecompressLimit returns how many decompressed bytes may be written: never
// more than the hard cap, never more than MaxBackupExpansionRatio times the
// compressed size (but at least the live database size, so a restore of a
// highly compressible database is not refused), and never so many that less
// than MarginBytes would stay free. When the volume has no room beyond the
// margin it returns a *NoRoomError rather than a zero limit.
func DecompressLimit(p DecompressPlan) (int64, error) {
	limit := p.HardCap
	if limit <= 0 {
		limit = 1 << 40
	}
	if p.CompressedBytes > 0 {
		ratioCap := p.CompressedBytes
		if ratioCap > limit/MaxBackupExpansionRatio {
			ratioCap = limit
		} else {
			ratioCap *= MaxBackupExpansionRatio
		}
		if p.LiveDBBytes > ratioCap {
			ratioCap = p.LiveDBBytes
		}
		if ratioCap < limit {
			limit = ratioCap
		}
	}
	if p.FreeBytes <= p.MarginBytes {
		return 0, &NoRoomError{Free: p.FreeBytes, Needed: p.MarginBytes}
	}
	if avail := p.FreeBytes - p.MarginBytes; avail < uint64(limit) {
		limit = int64(avail)
	}
	return limit, nil
}

const (
	// fingerprintCommentPrefix introduces the SHA-256 fingerprint carried in the
	// comment of the small trailing gzip member appended to every compressed
	// backup. A gzip stream may hold several members; decoders concatenate them,
	// and this one decodes to zero bytes, so `gunzip` and Go's reader both see
	// the original database unchanged.
	fingerprintCommentPrefix = "marbor-sha256:"
	fingerprintHexLen        = 64
	copyBufferSize           = 1 << 20
	sqliteHeaderLen          = 100
)

// sqliteVolatileHeaderRanges are the 4-byte header fields SQLite rewrites on
// commits without any change to the database's content: the file change
// counter (offset 24) and the version-valid-for number (offset 92). They are
// zeroed before hashing so two snapshots of identical content hash the same.
var sqliteVolatileHeaderRanges = [2]int{24, 92}

// BackupKind classifies the first bytes of a candidate backup file.
type BackupKind int

const (
	BackupKindUnknown BackupKind = iota
	BackupKindPlain
	BackupKindGzip
)

// SniffBackupKind classifies head (at least the first 16 bytes of a file) by
// magic number, never by file name.
func SniffBackupKind(head []byte) BackupKind {
	switch {
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		return BackupKindGzip
	case bytes.HasPrefix(head, []byte("SQLite format 3\x00")):
		return BackupKindPlain
	default:
		return BackupKindUnknown
	}
}

// SniffBackupFile reads the first bytes of path and classifies it.
func SniffBackupFile(path string) (BackupKind, error) {
	f, err := os.Open(path)
	if err != nil {
		return BackupKindUnknown, err
	}
	defer f.Close()
	return SniffBackupReader(f)
}

// SniffBackupReader classifies the first bytes of an open file, from its start.
func SniffBackupReader(r io.ReaderAt) (BackupKind, error) {
	head := make([]byte, 16)
	n, err := r.ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return BackupKindUnknown, err
	}
	return SniffBackupKind(head[:n]), nil
}

// CheckPlainBackupHeader is a cheap structural check of a plain SQLite backup:
// the magic string is present and the file is not shorter than the page count
// in its header says. It does not read the whole file, so it cannot prove the
// content is intact, only that it is not obviously truncated.
func CheckPlainBackupHeader(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return CheckPlainBackupHeaderFile(f)
}

// CheckPlainBackupHeaderFile is CheckPlainBackupHeader on an already open file,
// so the check and any later read refer to the same file even if the path is
// replaced in between.
func CheckPlainBackupHeaderFile(f *os.File) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	head := make([]byte, sqliteHeaderLen)
	if _, err := f.ReadAt(head, 0); err != nil {
		return invalidBackup("file is too short to be a SQLite database", err)
	}
	return checkSQLiteHeader(head, st.Size())
}

// checkSQLiteHeader validates the 100-byte SQLite header against the number of
// bytes the database holds: magic, a legal page size, and a page count that the
// size covers.
func checkSQLiteHeader(head []byte, size int64) error {
	if len(head) < sqliteHeaderLen {
		return invalidBackup("file is too short to be a SQLite database", nil)
	}
	if SniffBackupKind(head) != BackupKindPlain {
		return invalidBackup("not a SQLite database", nil)
	}
	pageSize := int64(binary.BigEndian.Uint16(head[16:18]))
	if pageSize == 1 {
		pageSize = 65536
	}
	if pageSize < 512 || pageSize&(pageSize-1) != 0 {
		return invalidBackup("SQLite header has an invalid page size", nil)
	}
	pages := int64(binary.BigEndian.Uint32(head[28:32]))
	if pages > 0 && size < pageSize*pages {
		return invalidBackup("file is shorter than its header says (truncated)", nil)
	}
	return nil
}

// fingerprintWriter hashes the bytes written to it with the volatile SQLite
// header fields zeroed.
type fingerprintWriter struct {
	h   hash.Hash
	pos int64
}

func newFingerprintWriter() *fingerprintWriter { return &fingerprintWriter{h: sha256.New()} }

func (w *fingerprintWriter) Write(p []byte) (int, error) {
	if w.pos < sqliteHeaderLen {
		q := append([]byte(nil), p...)
		for _, off := range sqliteVolatileHeaderRanges {
			for i := off; i < off+4; i++ {
				if idx := int64(i) - w.pos; idx >= 0 && idx < int64(len(q)) {
					q[idx] = 0
				}
			}
		}
		w.h.Write(q)
	} else {
		w.h.Write(p)
	}
	w.pos += int64(len(p))
	return len(p), nil
}

func (w *fingerprintWriter) sum() string { return hex.EncodeToString(w.h.Sum(nil)) }

// ctxReader fails reads once ctx is done, so a long copy can be cancelled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// FileFingerprint returns the canonical fingerprint of a plain (uncompressed)
// database file, streaming it.
func FileFingerprint(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return FileFingerprintFile(ctx, f)
}

// FileFingerprintFile is FileFingerprint on an already open file, read from its
// start regardless of the current offset.
func FileFingerprintFile(ctx context.Context, f *os.File) (string, error) {
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	fw := newFingerprintWriter()
	sr := io.NewSectionReader(f, 0, st.Size())
	if _, err := io.CopyBuffer(fw, struct{ io.Reader }{ctxReader{ctx, sr}}, make([]byte, copyBufferSize)); err != nil {
		return "", err
	}
	return fw.sum(), nil
}

// CompressBackup streams src (a plain SQLite snapshot) through gzip into dst,
// computing the canonical fingerprint on the way, then appends the fingerprint
// tail member. It returns the fingerprint and the number of uncompressed bytes
// read. dst is not closed or synced.
func CompressBackup(src io.Reader, dst io.Writer) (fingerprint string, rawBytes int64, err error) {
	zw, err := gzip.NewWriterLevel(dst, BackupGzipLevel)
	if err != nil {
		return "", 0, err
	}
	fw := newFingerprintWriter()
	rawBytes, err = io.CopyBuffer(io.MultiWriter(zw, fw), struct{ io.Reader }{src}, make([]byte, copyBufferSize))
	if err != nil {
		_ = zw.Close()
		return "", rawBytes, fmt.Errorf("compress backup: %w", err)
	}
	if err := zw.Close(); err != nil {
		return "", rawBytes, fmt.Errorf("finish gzip stream: %w", err)
	}
	fingerprint = fw.sum()
	if err := AppendFingerprintTail(dst, fingerprint); err != nil {
		return "", rawBytes, err
	}
	return fingerprint, rawBytes, nil
}

// fingerprintTailMember returns the exact bytes of the trailing gzip member
// that records fp. It is assembled by hand rather than by compress/gzip so the
// layout is fixed for good and cannot drift with a Go release:
//
//	1f 8b 08 10      magic, deflate, FCOMMENT flag
//	00 00 00 00      modification time (none)
//	00 ff            extra flags, OS unknown
//	<comment> 00     "marbor-sha256:<64 hex digits>", NUL terminated
//	03 00            deflate stream: one final empty block
//	00 00 00 00      CRC-32 of zero bytes
//	00 00 00 00      size of zero bytes
//
// A gzip decoder reads it as a member that decodes to nothing.
func fingerprintTailMember(fp string) []byte {
	b := make([]byte, 0, fingerprintTailLen)
	b = append(b, 0x1f, 0x8b, 0x08, 0x10, 0, 0, 0, 0, 0x00, 0xff)
	b = append(b, fingerprintCommentPrefix...)
	b = append(b, fp...)
	b = append(b, 0x00)
	b = append(b, 0x03, 0x00)
	b = append(b, 0, 0, 0, 0, 0, 0, 0, 0)
	return b
}

// fingerprintTailLen is the length of the tail member: 10 header bytes, the
// comment, its NUL, the 2-byte empty deflate block and the 8-byte trailer.
const fingerprintTailLen = 10 + len(fingerprintCommentPrefix) + fingerprintHexLen + 1 + 2 + 8

// AppendFingerprintTail writes the zero-length gzip member that records fp.
func AppendFingerprintTail(dst io.Writer, fp string) error {
	if len(fp) != fingerprintHexLen {
		return fmt.Errorf("fingerprint has %d characters, want %d", len(fp), fingerprintHexLen)
	}
	if _, err := dst.Write(fingerprintTailMember(fp)); err != nil {
		return fmt.Errorf("write fingerprint member: %w", err)
	}
	return nil
}

// ReadFingerprintTail returns the fingerprint recorded in the last gzip member
// of path without decompressing anything. The member must match the exact
// layout AppendFingerprintTail writes, byte for byte. ok is false for a plain
// file, a gzip without a fingerprint member, or anything that differs.
//
// The result is a hint for finding candidates only: it says what the writer
// claimed, not that the data in front of it is intact. Anything that skips or
// replaces a backup must confirm it by decompressing (FingerprintGzipBackup).
func ReadFingerprintTail(path string) (fp string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	return ReadFingerprintTailFile(f)
}

// ReadFingerprintTailFile is ReadFingerprintTail on an already open file.
func ReadFingerprintTailFile(f *os.File) (fp string, ok bool) {
	st, err := f.Stat()
	if err != nil || st.Size() < int64(fingerprintTailLen) {
		return "", false
	}
	tail := make([]byte, fingerprintTailLen)
	if _, err := f.ReadAt(tail, st.Size()-int64(fingerprintTailLen)); err != nil {
		return "", false
	}
	start := 10 + len(fingerprintCommentPrefix)
	hexPart := string(tail[start : start+fingerprintHexLen])
	if _, err := hex.DecodeString(hexPart); err != nil {
		return "", false
	}
	if !bytes.Equal(tail, fingerprintTailMember(hexPart)) {
		return "", false
	}
	return hexPart, true
}

// streamGunzip reads the whole gzip stream in src, writing the decompressed
// bytes to sink (nil to discard) and returning their canonical fingerprint and
// count. It never returns more than maxBytes: reading one byte past the cap is
// reported as ErrBackupTooLarge. Every member's checksum is verified, the
// member count is capped, and ctx cancels it.
//
// ensure, when set, is called before every write to sink with the number of
// bytes the sink will hold afterwards; an error from it stops the stream and is
// returned as it is. It lets the caller grow a disk reservation as the output
// grows instead of reserving the largest size the stream could reach.
func streamGunzip(ctx context.Context, src io.Reader, sink io.Writer, maxBytes int64, sanity bool, ensure func(total int64) error) (string, int64, error) {
	br := bufio.NewReaderSize(ctxReader{ctx, src}, copyBufferSize)
	zr, err := gzip.NewReader(br)
	if err != nil {
		return "", 0, invalidBackup("not a valid gzip backup", err)
	}
	defer zr.Close()

	limit := max(maxBytes, 0)
	fw := newFingerprintWriter()
	head := &headCapture{}
	w := io.Writer(io.MultiWriter(fw, head))
	if sink != nil {
		if ensure != nil {
			sink = &budgetWriter{w: sink, ensure: ensure}
		}
		w = io.MultiWriter(sink, fw, head)
	}
	buf := make([]byte, copyBufferSize)
	var total int64
	for members := 1; ; members++ {
		zr.Multistream(false)
		// Read one byte past what is left of the cap so exceeding it is
		// detected without ever writing more than the cap. limit-total+1 cannot
		// overflow: total <= limit <= MaxInt64 and the sum is only formed when
		// limit-total < MaxInt64.
		remaining := limit - total
		readCap := remaining
		if remaining < 1<<62 {
			readCap = remaining + 1
		}
		n, err := io.CopyBuffer(w, io.LimitReader(zr, readCap), buf)
		total += n
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", total, ctxErr
			}
			var be *budgetError
			if errors.As(err, &be) {
				return "", total, be.err
			}
			return "", total, invalidBackup("corrupt or truncated gzip stream", err)
		}
		if total > limit {
			return "", total, invalidBackup("backup is too large once decompressed", ErrBackupTooLarge)
		}
		if err := zr.Reset(br); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", total, invalidBackup("corrupt or truncated gzip stream", err)
		}
		if members >= MaxGzipMembers {
			return "", total, invalidBackup("gzip stream has too many members", nil)
		}
	}
	if sanity {
		if err := checkSQLiteHeader(head.b, total); err != nil {
			return "", total, err
		}
	}
	return fw.sum(), total, nil
}

// budgetError carries a refusal from the caller's ensure function out through
// the copy loop, which would otherwise report it as a damaged stream.
type budgetError struct{ err error }

func (e *budgetError) Error() string { return e.err.Error() }
func (e *budgetError) Unwrap() error { return e.err }

// budgetWriter asks ensure for room before each write.
type budgetWriter struct {
	w      io.Writer
	total  int64
	ensure func(total int64) error
}

func (b *budgetWriter) Write(p []byte) (int, error) {
	if err := b.ensure(b.total + int64(len(p))); err != nil {
		return 0, &budgetError{err}
	}
	n, err := b.w.Write(p)
	b.total += int64(n)
	return n, err
}

// headCapture keeps the first sqliteHeaderLen bytes written to it.
type headCapture struct{ b []byte }

func (h *headCapture) Write(p []byte) (int, error) {
	if room := sqliteHeaderLen - len(h.b); room > 0 {
		h.b = append(h.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// FingerprintGzipBackup decompresses the gzip file at path to nowhere,
// verifying every checksum, and returns the canonical fingerprint and size of
// what it holds. It writes nothing, so it needs no disk space.
func FingerprintGzipBackup(ctx context.Context, path string, maxBytes int64) (fingerprint string, n int64, err error) {
	src, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer src.Close()
	return FingerprintGzipFile(ctx, src, maxBytes)
}

// FingerprintGzipFile is FingerprintGzipBackup on an already open file, read
// from its start. It also checks that what the stream decompresses to starts
// with a sane SQLite header and is at least as long as that header claims, so a
// gzip of arbitrary bytes is not reported as a good backup.
func FingerprintGzipFile(ctx context.Context, f *os.File, maxBytes int64) (fingerprint string, n int64, err error) {
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	return streamGunzip(ctx, io.NewSectionReader(f, 0, st.Size()), nil, maxBytes, true, nil)
}

// DecompressBackup streams the gzip file at srcPath into dstPath (mode 0600,
// replacing any existing file), refusing to write more than maxBytes. The whole
// stream, including the gzip CRC trailer of every member, is read before it
// returns nil, so a truncated or corrupt file fails here, not later. On any
// error dstPath is removed. It returns the canonical fingerprint of the
// decompressed bytes and their count.
func DecompressBackup(ctx context.Context, srcPath, dstPath string, maxBytes int64, opts ...DecompressOption) (fingerprint string, n int64, err error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return "", 0, err
	}
	defer src.Close()
	return DecompressStream(ctx, src, dstPath, maxBytes, opts...)
}

// DecompressOption adjusts DecompressBackup and DecompressStream.
type DecompressOption func(*decompressOptions)

type decompressOptions struct{ ensure func(total int64) error }

// WithBudget makes the decompression ask ensure for room before each write,
// passing the number of bytes the output will hold afterwards. A non-nil error
// stops it and is returned unchanged.
func WithBudget(ensure func(total int64) error) DecompressOption {
	return func(o *decompressOptions) { o.ensure = ensure }
}

// DecompressStream is DecompressBackup for an already open source.
func DecompressStream(ctx context.Context, src io.Reader, dstPath string, maxBytes int64, opts ...DecompressOption) (fingerprint string, n int64, err error) {
	var o decompressOptions
	for _, opt := range opts {
		opt(&o)
	}
	// Remove first, then create exclusively: a symlink or file planted at the
	// path between the two steps makes the create fail instead of being followed.
	if rmErr := os.Remove(dstPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		return "", 0, fmt.Errorf("clear destination: %w", rmErr)
	}
	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, err
	}
	if runtime.GOOS != "windows" {
		if err := dst.Chmod(0o600); err != nil {
			err = errors.Join(err, dst.Close())
			if rmErr := os.Remove(dstPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("remove partial file: %w", rmErr))
			}
			return "", 0, err
		}
	}
	fingerprint, n, err = streamGunzip(ctx, src, dst, maxBytes, false, o.ensure)
	if err == nil {
		err = dst.Sync()
	}
	closeErr := dst.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		if rmErr := os.Remove(dstPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove partial file: %w", rmErr))
		}
		return "", n, err
	}
	return fingerprint, n, nil
}
