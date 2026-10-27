// Package diskspace answers the questions the backup and restore paths need
// answered before they write a large file: how many bytes can still be written
// under a path, how big that volume is, and whether two paths live on the same
// volume (so their space requirements must be added together rather than
// checked independently).
//
// The free figure is what the calling user can still write (POSIX f_bavail,
// Windows "available to caller"). It does not account for anything the
// operating system does not report as unavailable to that user: a per-user or
// per-directory quota on a network or project-quota filesystem, thin-provisioned
// storage that is over-committed, or files another process is about to write.
// Treat it as an optimistic upper bound and keep a safety margin.
package diskspace

// Usage is the capacity of the volume holding a path.
type Usage struct {
	Free  uint64 // bytes the calling user can still write
	Total uint64 // size of the volume
}

// Stat returns the free and total bytes of the volume holding path.
func Stat(path string) (Usage, error) { return stat(path) }

// Free returns the bytes available to the calling user on the volume holding
// path.
func Free(path string) (uint64, error) {
	u, err := stat(path)
	if err != nil {
		return 0, err
	}
	return u.Free, nil
}
