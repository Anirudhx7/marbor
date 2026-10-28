package admin

// backup_space.go - free-space checks. A backup, an upload and a restore each
// write database-sized files; every one is checked against the volumes it will
// write to before it starts, and refused with a visible reason instead of
// filling a disk that marbor.db itself lives on.
//
// Several of them can run at once (a scheduled backup, an on-demand one, an
// upload), and a check against the free space alone would let each one count
// the same bytes. So every writer takes a reservation from one shared ledger
// before it writes, and every check subtracts what the others already hold.
// Any failure to measure - a volume's free space, the database size - refuses
// the operation: a check that cannot be made is not a pass.

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Anirudhx7/marbor/internal/store"
)

const (
	// backupMinMargin is the least free space that must remain on every volume a
	// backup touches. It is the floor below which marbor.db and its write-ahead
	// log are never pushed by a backup, upload or restore.
	backupMinMargin = 128 << 20

	// backupMaxVolumeMargin caps the part of the margin that scales with the
	// size of the volume (1 percent of it), so a multi-terabyte volume does not
	// demand tens of gigabytes spare.
	backupMaxVolumeMargin = 4 << 30

	// walHeadroomMin is the least write-ahead-log growth planned for.
	walHeadroomMin = 64 << 20
)

// backupMargin is the free space to keep spare on a volume: at least 128 MiB, 5
// percent of the database, or 1 percent of the volume (that last part capped at
// 4 GiB), whichever is largest. volumeBytes is 0 when the volume size is
// unknown.
func backupMargin(dbBytes, volumeBytes uint64) uint64 {
	m := max(uint64(backupMinMargin), dbBytes/20)
	return max(m, min(volumeBytes/100, uint64(backupMaxVolumeMargin)))
}

// walHeadroom estimates how much the write-ahead log can grow while a snapshot
// holds its read transaction open. Basis: with a continuous writer the log
// peaked at 171 MiB (about 7 percent of the database) during a snapshot of a
// 2.48 GiB database. 10 percent leaves headroom over that; the floor covers a
// small database with a busy writer. A log already larger than the estimate is
// planned for as it stands.
func walHeadroom(dbBytes, walBytes uint64) uint64 {
	return max(walBytes, uint64(walHeadroomMin), dbBytes/10)
}

// The compressed copy is planned at the full snapshot size. Measured
// compression of marbor.db is 4.5x or better, but data that does not compress
// (already-compressed payloads stored in the database) makes the output about
// as large as the snapshot, and planning less could let the write run into the
// safety margin. Planning the worst case keeps the margin intact.

// ---- reservation ledger ---------------------------------------------------------

// holdTTL is the longest a reservation counts for. Every writer releases its
// hold when it finishes, however it ends; the expiry only covers a write that
// hangs for good (a dead network mount) so that it cannot keep space reserved
// until the next restart. It is far longer than any transfer may last. Tests
// replace it.
var holdTTL = 6 * time.Hour

// spaceHold is bytes promised on one volume until released. bytes and expires
// are guarded by the ledger's lock.
type spaceHold struct {
	l       *spaceLedger
	dir     string
	bytes   uint64
	expires time.Time
}

func (h *spaceHold) release() {
	if h == nil {
		return
	}
	h.l.mu.Lock()
	delete(h.l.holds, h)
	h.l.mu.Unlock()
}

// spaceLedger is every outstanding promise of space, across all writers.
type spaceLedger struct {
	mu    sync.Mutex
	holds map[*spaceHold]struct{}

	warnMu   sync.Mutex
	warnedAt map[string]time.Time // when "could not compare volumes" was last logged for a pair
}

// warnOncePer is how often a failed volume comparison is logged for one pair of
// directories.
const warnOncePer = 10 * time.Minute

func (l *spaceLedger) warnCompare(a, b string, err error) {
	key := a + "\x00" + b
	l.warnMu.Lock()
	defer l.warnMu.Unlock()
	if l.warnedAt == nil {
		l.warnedAt = map[string]time.Time{}
	}
	if time.Since(l.warnedAt[key]) < warnOncePer {
		return
	}
	l.warnedAt[key] = time.Now()
	log.Printf("admin: could not compare volumes, assuming they are the same: %v", err)
}

// addLocked records a hold. The caller holds l.mu.
func (l *spaceLedger) addLocked(dir string, bytes uint64) *spaceHold {
	if l.holds == nil {
		l.holds = map[*spaceHold]struct{}{}
	}
	h := &spaceHold{l: l, dir: dir, bytes: bytes, expires: time.Now().Add(holdTTL)}
	l.holds[h] = struct{}{}
	return h
}

// volumesLike reports, for every directory that currently has a hold, whether it
// shares dir's volume. It stats paths, so it runs before the ledger lock is
// taken, never under it. A directory that gained a hold after this call is
// missing from the result and is treated as the same volume by
// reservedOnLocked, the safe direction. Two paths that cannot be compared are
// assumed to share a volume too.
func (l *spaceLedger) volumesLike(dir string) map[string]bool {
	l.mu.Lock()
	dirs := make(map[string]struct{}, len(l.holds))
	for h := range l.holds {
		dirs[h.dir] = struct{}{}
	}
	l.mu.Unlock()
	same := make(map[string]bool, len(dirs))
	for d := range dirs {
		ok, err := backupSameVolume(dir, d)
		if err != nil {
			l.warnCompare(dir, d, err)
			ok = true
		}
		same[d] = ok
	}
	return same
}

// reservedOnLocked adds up the holds on the volume described by same (from
// volumesLike), leaving out the ones in exclude (a writer's own holds when it
// re-checks). The caller holds l.mu and does no filesystem work under it.
func (l *spaceLedger) reservedOnLocked(same map[string]bool, exclude []*spaceHold) uint64 {
	var total uint64
	now := time.Now()
outer:
	for h := range l.holds {
		if now.After(h.expires) {
			log.Printf("admin: a space reservation of %s on a backup directory expired after %s without being released; ignoring it", formatBytes(h.bytes), holdTTL)
			delete(l.holds, h)
			continue
		}
		for _, x := range exclude {
			if x == h {
				continue outer
			}
		}
		if v, known := same[h.dir]; known && !v {
			continue
		}
		total += h.bytes
	}
	return total
}

// spaceTicket is the set of holds one backup keeps for as long as it runs.
type spaceTicket struct{ holds []*spaceHold }

func (t *spaceTicket) release() {
	if t == nil {
		return
	}
	for _, h := range t.holds {
		h.release()
	}
}

// ---- volume measurements ----------------------------------------------------------

type roomInfo struct{ free, margin uint64 }

// roomOn measures dir's volume. what completes "so ..." in the error shown to the
// operator. Reading the database size is part of the measurement: the margin
// depends on it, and a failure to read it refuses rather than guesses.
func (s *Server) roomOn(dir, what string) (roomInfo, error) {
	free, err := backupFreeBytes(dir)
	if err != nil {
		return roomInfo{}, newBackupFailure("could not check free disk space, so "+what, err)
	}
	total, terr := backupTotalBytes(dir)
	if terr != nil {
		log.Printf("admin: could not read the volume size for the free-space margin: %v", terr)
		total = 0
	}
	liveDB, _, ferr := s.st.Footprint()
	if ferr != nil {
		return roomInfo{}, newBackupFailure("could not read the database size, so free disk space cannot be checked; "+what, ferr)
	}
	return roomInfo{free: free, margin: backupMargin(uint64(max(liveDB, 0)), total)}, nil
}

// usable is what may still be written on the volume: free space minus the
// safety margin minus what other writers have already reserved.
func (r roomInfo) usable(reserved uint64) uint64 {
	if r.free <= r.margin+reserved {
		return 0
	}
	return r.free - r.margin - reserved
}

// reserveRoom promises bytes on dir's volume. With exact it refuses (returning
// the sizes in a *backupRefusedError built by the caller from the returned
// roomInfo) when less is usable; otherwise it grants as much as is usable, up to
// want. The check and the reservation are one step under the ledger lock.
func (s *Server) reserveRoom(dir string, want uint64, exact bool, what string) (hold *spaceHold, granted uint64, room roomInfo, usable uint64, err error) {
	room, err = s.roomOn(dir, what)
	if err != nil {
		return nil, 0, room, 0, err
	}
	l := &s.backupSpace
	same := l.volumesLike(dir)
	l.mu.Lock()
	defer l.mu.Unlock()
	usable = room.usable(l.reservedOnLocked(same, nil))
	granted = min(want, usable)
	if exact && usable < want {
		return nil, 0, room, usable, errNoRoom
	}
	if granted == 0 {
		return nil, 0, room, usable, errNoRoom
	}
	return l.addLocked(dir, granted), granted, room, usable, nil
}

// errNoRoom marks a reservation that could not be granted; the caller turns it
// into the refusal text that fits its operation.
var errNoRoom = errors.New("not enough free disk space")

type spaceNeed struct {
	dir   string
	label string // fixed description shown to the operator, never a path
	bytes uint64
}

type spaceGroup struct {
	dir    string
	labels []string
	bytes  uint64
}

// groupByVolume adds up the needs that land on one volume. When two paths
// cannot be compared they are assumed to share a volume (the safe direction).
func (l *spaceLedger) groupByVolume(needs []spaceNeed) []spaceGroup {
	var groups []spaceGroup
outer:
	for _, n := range needs {
		for i := range groups {
			same, err := backupSameVolume(groups[i].dir, n.dir)
			if err != nil {
				l.warnCompare(groups[i].dir, n.dir, err)
				same = true
			}
			if same {
				groups[i].bytes += n.bytes
				groups[i].labels = append(groups[i].labels, n.label)
				continue outer
			}
		}
		groups = append(groups, spaceGroup{dir: n.dir, labels: []string{n.label}, bytes: n.bytes})
	}
	return groups
}

// backupPreflight refuses a run that would not fit, and on success reserves what
// the run will use until the returned ticket is released. Before the snapshot
// exists only two things are known: the volume holding marbor.db needs room for
// the write-ahead log, which grows for as long as the snapshot's read
// transaction is open, and scratchDir needs room for the snapshot, which is the
// size of the database. The compressed copy is checked once the snapshot
// exists and its real size is known (see checkOutputRoom); the worst case, a
// copy as large as the snapshot, is reserved here so concurrent backups account
// for it. Basis for the plan: the
// peak on one volume measured 1.13 times the database size.
//
// Needs on one volume add up, and each volume must keep backupMargin spare
// afterwards, beyond what other running writers have reserved.
func (s *Server) backupPreflight(scratchDir, outDir string) (*spaceTicket, error) {
	dbSigned, walSigned, err := s.st.Footprint()
	if err != nil {
		return nil, newBackupFailure("could not read the database size, so free disk space cannot be checked; the backup was not started", err)
	}
	db, wal := uint64(max(dbSigned, 0)), uint64(max(walSigned, 0))

	var needs []spaceNeed
	if dbDir := s.dirOfDB(); dbDir != "" {
		needs = append(needs, spaceNeed{dbDir, "the database", walHeadroom(db, wal)})
	}
	needs = append(needs, spaceNeed{scratchDir, "the snapshot", db})

	// Every measurement that touches the filesystem is made here, before the
	// ledger lock; under the lock only the arithmetic and the hold are done.
	type measured struct {
		g      spaceGroup
		free   uint64
		margin uint64
		same   map[string]bool
	}
	l := &s.backupSpace
	var groups []measured
	for _, g := range l.groupByVolume(needs) {
		free, err := backupFreeBytes(g.dir)
		if err != nil {
			return nil, newBackupFailure("could not check free disk space, so the backup was not started", err)
		}
		total, terr := backupTotalBytes(g.dir)
		if terr != nil {
			log.Printf("admin: could not read the volume size for the backup margin: %v", terr)
			total = 0
		}
		groups = append(groups, measured{g: g, free: free, margin: backupMargin(db, total), same: l.volumesLike(g.dir)})
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range groups {
		others := l.reservedOnLocked(m.same, nil)
		need := m.g.bytes + m.margin + others
		if m.free < need {
			if others > 0 && m.free >= m.g.bytes+m.margin {
				// It would fit if the other running backup, upload or restore were not
				// holding space: wait for it instead of reporting a failure.
				return nil, fmt.Errorf("%w: another backup operation is using the disk", errBackupDeferred)
			}
			return nil, &backupRefusedError{msg: fmt.Sprintf(
				"backup refused: not enough free disk space on the volume holding %s: need about %s, have %s (database %s + log %s, plus a safety margin)",
				strings.Join(m.g.labels, " and "), formatBytes(need), formatBytes(m.free), formatBytes(db), formatBytes(wal))}
		}
	}
	t := &spaceTicket{}
	if dbDir := s.dirOfDB(); dbDir != "" {
		t.holds = append(t.holds, l.addLocked(dbDir, walHeadroom(db, wal)))
	}
	t.holds = append(t.holds, l.addLocked(scratchDir, db))
	if outDir != "" {
		t.holds = append(t.holds, l.addLocked(outDir, db))
	}
	return t, nil
}

// checkOutputRoom is the second half of the preflight, made once the snapshot
// exists: outDir must be able to take its compressed copy (planned at the
// snapshot's full size, the worst case for data that does not compress) and
// keep the margin, beyond what other writers reserved. The run's own
// reservations in ticket do not count against it.
func (s *Server) checkOutputRoom(outDir string, snapBytes int64, ticket *spaceTicket) error {
	room, err := s.roomOn(outDir, "the backup was not written")
	if err != nil {
		return err
	}
	need := uint64(max(snapBytes, 0))
	l := &s.backupSpace
	same := l.volumesLike(outDir)
	l.mu.Lock()
	var own []*spaceHold
	if ticket != nil {
		own = ticket.holds
	}
	usable := room.usable(l.reservedOnLocked(same, own))
	l.mu.Unlock()
	if usable < need {
		return &backupRefusedError{msg: fmt.Sprintf(
			"backup refused: not enough free disk space for the compressed copy: need about %s plus a safety margin, have %s usable",
			formatBytes(need), formatBytes(usable))}
	}
	return nil
}

// growingHold is a reservation that follows a file as it grows. A restore or an
// upload cannot know in advance how large its output will be, and promising the
// largest size it could reach (up to the whole free space) would make every other
// backup operation wait or be refused for as long as it runs. It reserves a
// plausible size first and asks for more, in steps, as the bytes arrive; a
// request the volume cannot grant ends the operation with a refusal.
type growingHold struct {
	s     *Server
	dir   string
	hold  *spaceHold
	limit uint64 // never reserve more than this
	what  string // completes "the disk filled up while ..." in the refusal
}

// growStep is how far ahead a growing reservation reaches.
const growStep = 64 << 20

// ensure makes sure the hold covers a file that will be total bytes long once the
// current write lands. Free space already shows what was written, so what is
// checked is only the part still to come, beyond the safety margin and what other
// writers hold.
func (g *growingHold) ensure(total int64) error {
	need := uint64(max(total, 0))
	if need > g.limit {
		return nil // the size cap, not the disk, ends it
	}
	l := &g.s.backupSpace
	l.mu.Lock()
	held := g.hold.bytes
	// Every call is a sign the write is alive: keep the hold from expiring under
	// a long transfer that already has all the room it needs.
	g.hold.expires = time.Now().Add(holdTTL)
	l.mu.Unlock()
	if need <= held {
		return nil
	}
	want := min(g.limit, need+max(uint64(growStep), need/4))
	room, err := g.s.roomOn(g.dir, g.what)
	if err != nil {
		return err
	}
	same := l.volumesLike(g.dir)
	l.mu.Lock()
	defer l.mu.Unlock()
	usable := room.usable(l.reservedOnLocked(same, []*spaceHold{g.hold}))
	if usable < (want-need)+copyBufferBytes {
		// Fall back to the least that covers this write before giving up.
		want = need
		if usable < copyBufferBytes {
			return &backupRefusedError{msg: fmt.Sprintf("the disk filled up while %s: %s free, and at least %s must stay spare",
				g.what, formatBytes(room.free), formatBytes(room.margin))}
		}
	}
	g.hold.bytes = max(g.hold.bytes, want)
	g.hold.expires = time.Now().Add(holdTTL)
	return nil
}

func (g *growingHold) release() {
	if g != nil {
		g.hold.release()
	}
}

// copyBufferBytes is the largest single write the copy loops make.
const copyBufferBytes = 1 << 20

// newGrowingHold reserves initial bytes on dir's volume (refusing when even that
// is not usable) and returns a hold that grows up to limit.
func (s *Server) newGrowingHold(dir string, initial, limit uint64, what string) (*growingHold, error) {
	hold, _, room, usable, err := s.reserveRoom(dir, min(initial, limit), true, what)
	if errors.Is(err, errNoRoom) {
		return nil, &backupRefusedError{msg: fmt.Sprintf("not enough free disk space: %s free, %s usable after the safety margin, %s needed",
			formatBytes(room.free), formatBytes(usable), formatBytes(initial))}
	}
	if err != nil {
		return nil, err
	}
	return &growingHold{s: s, dir: dir, hold: hold, limit: limit, what: what}, nil
}

// decompressLimit is how many bytes a compressed file of compressedSize bytes
// may expand to when unpacked under dir, and a reservation that starts at a
// plausible size (the live database, or four times the compressed size, at
// least 64 MiB) and grows with the output (the caller passes its ensure to the
// decompression and releases it when the unpacked file is gone or accounted for
// by the volume's free space). The limit is the smallest of the hard ceiling, 50
// times the compressed size (but at least the live database size), and the free
// space on dir's volume minus the safety margin and minus what other writers
// have reserved. When the free space cannot be read, or leaves nothing beyond
// the margin, it returns an error the operator can be shown; it never falls back
// to a guess.
func (s *Server) decompressLimit(dir string, compressedSize int64) (int64, *growingHold, error) {
	room, err := s.roomOn(dir, "the backup file was not unpacked")
	if err != nil {
		return 0, nil, err
	}
	liveDB, _, ferr := s.st.Footprint()
	if ferr != nil {
		return 0, nil, newBackupFailure("could not read the database size, so the backup file was not unpacked", ferr)
	}
	l := &s.backupSpace
	same := l.volumesLike(dir)
	l.mu.Lock()
	reserved := l.reservedOnLocked(same, nil)
	l.mu.Unlock()
	free := room.free
	if free > reserved {
		free -= reserved
	} else {
		free = 0
	}
	limit, err := store.DecompressLimit(store.DecompressPlan{
		FreeBytes:       free,
		MarginBytes:     room.margin,
		CompressedBytes: compressedSize,
		LiveDBBytes:     liveDB,
		HardCap:         maxDecompressedBackupBytes,
	})
	var noRoom *store.NoRoomError
	if errors.As(err, &noRoom) {
		return 0, nil, &backupRefusedError{msg: fmt.Sprintf(
			"not enough free disk space to unpack this backup: %s free on the volume, and at least %s must stay spare",
			formatBytes(noRoom.Free), formatBytes(noRoom.Needed))}
	}
	if err != nil {
		return 0, nil, err
	}
	initial := max(uint64(max(liveDB, 0)), uint64(max(compressedSize, 0))*4, 64<<20)
	hold, herr := s.newGrowingHold(dir, initial, uint64(max(limit, 0)), "the backup was being unpacked")
	if herr != nil {
		return 0, nil, herr
	}
	return limit, hold, nil
}
