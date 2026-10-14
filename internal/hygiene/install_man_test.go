package hygiene

// install_man_test.go guards install.sh's man-page step. That step runs on an
// end user's machine, as root, from `curl | sh`, and must never abort the
// install nor place anything uninstall.sh would not recognise. The real
// functions are cut out of install.sh (so this tests the shipped text, not a
// copy), pointed at a local release server, and run under sh.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const (
	installBaseURL = "https://github.com/${REPO}/releases/latest/download"
	installManDir  = "/usr/local/share/man/man1"
	manArchive     = "marbor-man-pages.tar.gz"
)

// installManFunctions returns the man-page functions from install.sh with the
// release URL and man directory rewritten for the test.
func installManFunctions(t *testing.T, baseURL, manDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatalf("reading install.sh: %v", err)
	}
	src := string(data)
	start := strings.Index(src, "fetch_quiet() {")
	end := strings.Index(src, "\nif [ \"$ROLE\" != \"agent\" ]; then")
	if start < 0 || end < start {
		t.Fatal("install.sh no longer has the fetch_quiet..install_man_pages block this test extracts")
	}
	block := src[start:end]
	for _, want := range []string{installBaseURL, installManDir} {
		if !strings.Contains(block, want) {
			t.Fatalf("install.sh man block no longer contains %q", want)
		}
	}
	block = strings.ReplaceAll(block, installBaseURL, baseURL)
	return strings.ReplaceAll(block, installManDir, filepath.ToSlash(manDir))
}

type tarEntry struct {
	name, body, link string
}

func buildArchive(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if e.link != "" {
			h = &tar.Header{Name: e.name, Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: e.link}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.link == "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sumsFor(name string, data []byte) []byte {
	h := sha256.Sum256(data)
	return []byte(strings.Repeat("a", 64) + "  marbor-linux-amd64\n" + hex.EncodeToString(h[:]) + "  " + name + "\n")
}

// runInstallMan serves assets as a release, runs install_man_pages with
// markerBefore already in the man dir as the uninstall marker (when set), and
// returns the script output, the *.1 files installed, and the marker lines.
func runInstallMan(t *testing.T, assets map[string][]byte, markerBefore string) (out string, pages, marker []string) {
	t.Helper()
	for _, tool := range []string{"sh", "curl", "tar", "sha256sum", "awk", "tr", "grep", "sort"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available: %v", tool, err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := assets[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	manDir := filepath.Join(t.TempDir(), "man1")
	if markerBefore != "" {
		if err := os.MkdirAll(manDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(manDir, ".marbor-installed-manifest"), []byte(markerBefore), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := "REPO=x/y\n" + installManFunctions(t, srv.URL, manDir) + "\ninstall_man_pages\n"
	b, err := exec.Command("sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("install_man_pages must never fail the install, got %v:\n%s", err, b)
	}
	files, _ := filepath.Glob(filepath.Join(manDir, "*.1"))
	for _, f := range files {
		pages = append(pages, filepath.Base(f))
	}
	sort.Strings(pages)
	if m, err := os.ReadFile(filepath.Join(manDir, ".marbor-installed-manifest")); err == nil {
		marker = strings.Fields(string(m))
	}
	return string(b), pages, marker
}

func goodArchiveEntries() []tarEntry {
	return []tarEntry{
		{name: "man1/marbor.1", body: ".TH MARBOR 1\n"},
		{name: "man1/marbor-nodes.1", body: ".TH NODES 1\n"},
	}
}

func equalStrings(a, b []string) bool {
	return strings.Join(a, "\x00") == strings.Join(b, "\x00")
}

func TestInstallMan_VerifiedArchiveInstalls(t *testing.T) {
	arc := buildArchive(t, goodArchiveEntries())
	out, pages, marker := runInstallMan(t, map[string][]byte{manArchive: arc, "checksums.txt": sumsFor(manArchive, arc)}, "")
	want := []string{"marbor-nodes.1", "marbor.1"}
	if !equalStrings(pages, want) || !equalStrings(marker, want) {
		t.Fatalf("pages=%v marker=%v want both %v\n%s", pages, marker, want, out)
	}
}

func TestInstallMan_RefusesUnverifiedArchive(t *testing.T) {
	arc := buildArchive(t, goodArchiveEntries())
	cases := map[string]map[string][]byte{
		"tampered":         {manArchive: append(append([]byte{}, arc...), 'x'), "checksums.txt": sumsFor(manArchive, arc)},
		"no entry":         {manArchive: arc, "checksums.txt": sumsFor("other.tar.gz", arc)},
		"no checksums.txt": {manArchive: arc},
	}
	for name, assets := range cases {
		t.Run(name, func(t *testing.T) {
			out, pages, marker := runInstallMan(t, assets, "")
			if len(pages) != 0 || len(marker) != 0 {
				t.Fatalf("installed from an unverified archive: pages=%v marker=%v", pages, marker)
			}
			if !strings.Contains(out, "could not verify") {
				t.Fatalf("expected a visible skip message, got %q", out)
			}
		})
	}
}

func TestInstallMan_CorruptArchiveInstallsNothing(t *testing.T) {
	arc := []byte("not a gzip stream")
	out, pages, _ := runInstallMan(t, map[string][]byte{manArchive: arc, "checksums.txt": sumsFor(manArchive, arc)}, "")
	if len(pages) != 0 || !strings.Contains(out, "could not unpack") {
		t.Fatalf("pages=%v out=%q", pages, out)
	}
}

func TestInstallMan_DropsHostileEntries(t *testing.T) {
	entries := append(goodArchiveEntries(),
		tarEntry{name: "man1/marbor evil.1", body: "x"},
		tarEntry{name: "man1/other.1", body: "x"},
		tarEntry{name: "top-level.txt", body: "x"},
	)
	if runtime.GOOS != "windows" {
		// Windows tar cannot create symlinks and refuses the whole archive,
		// which is safe but would hide the filtering this case is about.
		entries = append(entries, tarEntry{name: "man1/marbor-link.1", link: "/etc/hostname"})
	}
	arc := buildArchive(t, entries)
	out, pages, marker := runInstallMan(t, map[string][]byte{manArchive: arc, "checksums.txt": sumsFor(manArchive, arc)}, "")
	want := []string{"marbor-nodes.1", "marbor.1"}
	if !equalStrings(pages, want) || !equalStrings(marker, want) {
		t.Fatalf("pages=%v marker=%v want %v\n%s", pages, marker, want, out)
	}
	safe := regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]*$`)
	for _, m := range marker {
		if !safe.MatchString(m) {
			t.Fatalf("marker entry %q would be rejected by uninstall.sh", m)
		}
	}
}

func TestInstallMan_MarkerKeepsPagesFromEarlierInstall(t *testing.T) {
	arc := buildArchive(t, goodArchiveEntries())
	_, _, marker := runInstallMan(t, map[string][]byte{manArchive: arc, "checksums.txt": sumsFor(manArchive, arc)}, "marbor-retired.1\nmarbor.1\n")
	want := []string{"marbor-nodes.1", "marbor-retired.1", "marbor.1"}
	if !equalStrings(marker, want) {
		t.Fatalf("marker=%v want %v", marker, want)
	}
}

func TestInstallMan_NothingPublishedIsAVisibleNoOp(t *testing.T) {
	out, pages, marker := runInstallMan(t, map[string][]byte{}, "")
	if len(pages) != 0 || len(marker) != 0 || !strings.Contains(out, "Skipped man pages") {
		t.Fatalf("pages=%v marker=%v out=%q", pages, marker, out)
	}
}
