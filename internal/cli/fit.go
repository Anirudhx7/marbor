package cli

// fit.go - `marbor fit`: for each node, which curated catalog models (or one
// Hugging Face repo) fit its total VRAM and disk, and which quantization to
// pull. Read-only. It decodes only the recommendation fields it prints from
// GET /admin/models/catalog and GET /admin/models/repo.

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
)

// fitPick mirrors the server's `recommendation` object (fields used here).
type fitPick struct {
	Picked       bool   `json:"picked"`
	Tag          string `json:"tag"`
	Quantization string `json:"quantization"`
	VRAMEstMB    int64  `json:"vram_est_mb"`
	Fit          string `json:"fit"`
	Tight        bool   `json:"tight"`
	Reason       string `json:"reason,omitempty"`
	ClosestTag   string `json:"closest_tag,omitempty"`
}

// fitRow is one printed/JSON row: a node and a model (or repo) with its pick.
type fitRow struct {
	Node           string  `json:"node"`
	Model          string  `json:"model"`
	Downloaded     bool    `json:"downloaded,omitempty"`
	Recommendation fitPick `json:"recommendation"`
}

type fitCatalogResp struct {
	Nodes []struct {
		Name   string `json:"name"`
		Models []struct {
			Name           string  `json:"name"`
			Downloaded     bool    `json:"downloaded"`
			Recommendation fitPick `json:"recommendation"`
		} `json:"models"`
	} `json:"nodes"`
}

// fitPickCell renders the PICK column; "-" when there is no pick.
func fitPickCell(p fitPick) string {
	if !p.Picked {
		return "-"
	}
	if p.Quantization == "" {
		return p.Tag
	}
	return p.Tag + " (" + p.Quantization + ")"
}

// fitNote explains a missing pick, or flags a tight one.
func fitNote(r fitRow) string {
	p := r.Recommendation
	var note string
	switch {
	case p.Picked && p.Tight:
		note = "tight fit: uses most of the VRAM"
	case p.Picked:
		note = ""
	case p.Reason == "too_large" && p.ClosestTag != "":
		note = "too large for this node; closest: " + p.ClosestTag
	case p.Reason == "too_large":
		note = "too large for this node"
	case p.Reason == "vram_unknown":
		note = "VRAM or model size unknown"
	case p.Reason == "disk_insufficient":
		note = "not enough free disk"
	case p.Reason == "incompatible_runtime":
		note = "other format for this node's runtime"
	case p.Reason == "no_variants":
		note = "no quantizations offered"
	default:
		note = "-"
	}
	if r.Downloaded {
		if note != "" {
			note += "; "
		}
		note += "a version is downloaded"
	}
	if note == "" {
		return "-"
	}
	return note
}

func fitFitCell(p fitPick) string {
	switch {
	case !p.Picked:
		return "-"
	case p.Tight:
		return "tight"
	default:
		return p.Fit
	}
}

func fitVRAMCell(p fitPick) string {
	if !p.Picked || p.VRAMEstMB <= 0 {
		return "-"
	}
	return "~" + fmtMB(p.VRAMEstMB)
}

// printFitRows writes the table (or JSON when --json is set).
func printFitRows(flags *globalFlags, rows []fitRow, header string, stdout, stderr io.Writer) int {
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, rows); handled {
		return code
	}
	tw := newTabWriter(stdout)
	fmt.Fprintf(tw, "NODE\t%s\tPICK\tEST VRAM\tFIT\tNOTE\n", header)
	for _, r := range rows {
		p := r.Recommendation
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Node, r.Model, fitPickCell(p), fitVRAMCell(p), fitFitCell(p), fitNote(r))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	return ExitOK
}

// runFit implements `marbor fit [owner/name] [--node --ctx]`.
func runFit(ctx *RunCtx) int {
	repoID, node := "", ctx.String("node")
	if len(ctx.Args) > 0 {
		repoID = ctx.Args[0]
	}
	if ctx.IsSet("ctx") && (repoID == "" || ctx.Int("ctx") <= 0) {
		if repoID == "" {
			fmt.Fprintln(ctx.Stderr, "error: --ctx only applies when an owner/name Hugging Face repo is given")
		} else {
			fmt.Fprintln(ctx.Stderr, "error: --ctx must be a positive number of tokens")
		}
		return ExitUserError
	}
	client, err := authenticatedClient(ctx.Flags)
	if err != nil {
		return reportError(err, ctx.Stderr)
	}
	raw, err := client.ModelCatalog()
	if err != nil {
		return reportError(err, ctx.Stderr)
	}
	var cat fitCatalogResp
	if err := json.Unmarshal(raw, &cat); err != nil {
		fmt.Fprintf(ctx.Stderr, "error: decode catalog: %v\n", err)
		return ExitServerError
	}
	var names []string
	for _, n := range cat.Nodes {
		names = append(names, n.Name)
	}
	if node != "" && !slices.Contains(names, node) {
		fmt.Fprintf(ctx.Stderr, "error: unknown node %q (nodes: %s)\n", node, strings.Join(names, ", "))
		return ExitUserError
	}

	var rows []fitRow
	if repoID == "" {
		for _, n := range cat.Nodes {
			if node != "" && n.Name != node {
				continue
			}
			for _, m := range n.Models {
				rows = append(rows, fitRow{Node: n.Name, Model: m.Name, Downloaded: m.Downloaded, Recommendation: m.Recommendation})
			}
		}
		return printFitRows(ctx.Flags, rows, "MODEL", ctx.Stdout, ctx.Stderr)
	}

	for _, name := range names {
		if node != "" && name != node {
			continue
		}
		raw, err := client.ModelRepo(ModelRepoOpts{ID: repoID, Node: name, CtxLen: ctx.Int("ctx")})
		if err != nil {
			return reportError(err, ctx.Stderr)
		}
		var repo struct {
			Recommendation fitPick `json:"recommendation"`
			Variants       []struct {
				Downloaded bool `json:"downloaded"`
			} `json:"variants"`
		}
		if err := json.Unmarshal(raw, &repo); err != nil {
			fmt.Fprintf(ctx.Stderr, "error: decode repo: %v\n", err)
			return ExitServerError
		}
		downloaded := false
		for _, v := range repo.Variants {
			downloaded = downloaded || v.Downloaded
		}
		rows = append(rows, fitRow{Node: name, Model: repoID, Downloaded: downloaded, Recommendation: repo.Recommendation})
	}
	return printFitRows(ctx.Flags, rows, "REPO", ctx.Stdout, ctx.Stderr)
}
