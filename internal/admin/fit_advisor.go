package admin

// Reasons a node/model pair has no quantization pick. Stable API strings.
const (
	reasonNoVariants          = "no_variants"
	reasonIncompatibleRuntime = "incompatible_runtime"
	reasonVRAMUnknown         = "vram_unknown"
	reasonDiskInsufficient    = "disk_insufficient"
	reasonTooLarge            = "too_large"
)

// quantRecommendation is the read-only answer to "which offered quantization
// should I pull on this node". Sizes are the same estimates the per-variant
// fit verdicts already use; nothing here is measured. When Picked is false,
// Reason says why and the other fields are empty.
type quantRecommendation struct {
	Picked       bool   `json:"picked"`
	Tag          string `json:"tag"`
	Quantization string `json:"quantization"`
	VRAMEstMB    int64  `json:"vram_est_mb"`
	SizeMB       int64  `json:"size_mb"`
	Fit          string `json:"fit"`
	// Tight is true when the pick only fits in the yellow band (above 85% of
	// the node's total VRAM): it fits, but with little headroom.
	Tight      bool   `json:"tight"`
	Reason     string `json:"reason,omitempty"`
	ClosestTag string `json:"closest_tag,omitempty"` // smallest variant, set with too_large
}

// quantCandidate is one offered quantization with its per-node verdicts.
type quantCandidate struct {
	Tag          string
	Quantization string
	VRAMEstMB    int64
	SizeMB       int64
	Fit          string // green / yellow / red / unknown / incompatible
	DiskFit      string // ok / insufficient / unknown
	Recommended  bool   // the catalog's own recommended variant
}

// usable reports whether a candidate can be picked: it fits total VRAM (green
// or yellow), its disk check does not fail (unknown disk counts as ok), and
// its size is actually known (a zero estimate is "unknown", never a fit).
func (c quantCandidate) usable() bool {
	return (c.Fit == "green" || c.Fit == "yellow") && c.DiskFit != "insufficient" && c.VRAMEstMB > 0
}

// pickBy returns the best usable candidate with the given fit: the catalog's
// recommended variant if it has that fit, else the largest VRAM estimate
// (first in list order on a tie).
func pickBy(cands []quantCandidate, fit string) (quantCandidate, bool) {
	var best quantCandidate
	found := false
	for _, c := range cands {
		if !c.usable() || c.Fit != fit {
			continue
		}
		if c.Recommended {
			return c, true
		}
		if !found || c.VRAMEstMB > best.VRAMEstMB {
			best, found = c, true
		}
	}
	return best, found
}

// recommendQuant picks one quantization from the offered variants. Order:
// recommended variant if green, else largest green, else recommended variant
// if yellow, else largest yellow (flagged Tight). Pure: no I/O, no routing
// state.
func recommendQuant(cands []quantCandidate) quantRecommendation {
	if len(cands) == 0 {
		return quantRecommendation{Reason: reasonNoVariants}
	}
	for _, fit := range []string{"green", "yellow"} {
		if c, ok := pickBy(cands, fit); ok {
			return quantRecommendation{
				Picked: true, Tag: c.Tag, Quantization: c.Quantization,
				VRAMEstMB: c.VRAMEstMB, SizeMB: c.SizeMB, Fit: c.Fit, Tight: fit == "yellow",
			}
		}
	}
	return noPick(cands)
}

// noPick explains why nothing was picked.
func noPick(cands []quantCandidate) quantRecommendation {
	allIncompatible, anyRed := true, false
	var closest quantCandidate
	haveClosest := false
	for _, c := range cands {
		if c.Fit != "incompatible" {
			allIncompatible = false
		}
		if c.Fit == "red" {
			anyRed = true
			if c.VRAMEstMB > 0 && (!haveClosest || c.VRAMEstMB < closest.VRAMEstMB) {
				closest, haveClosest = c, true
			}
		}
	}
	switch {
	case allIncompatible:
		return quantRecommendation{Reason: reasonIncompatibleRuntime}
	case diskBlocksAll(cands):
		return quantRecommendation{Reason: reasonDiskInsufficient}
	case anyRed:
		return quantRecommendation{Reason: reasonTooLarge, ClosestTag: closest.Tag}
	default:
		return quantRecommendation{Reason: reasonVRAMUnknown}
	}
}

// diskBlocksAll is true when every VRAM-fitting candidate fails its disk check.
func diskBlocksAll(cands []quantCandidate) bool {
	seen := false
	for _, c := range cands {
		if (c.Fit == "green" || c.Fit == "yellow") && c.VRAMEstMB > 0 {
			seen = true
			if c.DiskFit != "insufficient" {
				return false
			}
		}
	}
	return seen
}
