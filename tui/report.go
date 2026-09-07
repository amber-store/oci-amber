package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/importer"
)

// RenderReport renders the end-of-run report as plain text.
func RenderReport(r *importer.Report, archive string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Imported %s in %s\n\n", archive, FormatShort(r.Duration))
	for _, e := range r.Entries {
		names := make([]string, len(e.Names))
		for i, n := range e.Names {
			names[i] = n.String()
		}
		fields := []string{strings.Join(names, ", "), shortRef(e), kindOf(e)}
		if rf := rootfsOf(e); rf != "" {
			fields = append(fields, rf)
		}
		fmt.Fprintf(&b, "  %s\n", strings.Join(fields, "   "))
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "%-15s %s\n", "Blobs", blobsLine(r.Blobs))
	line := func(label string, n int64, note string) {
		fmt.Fprintf(&b, "%-15s %-11s %s bytes   %s\n", label, FormatBytes(n), FormatCount(n), note)
	}
	line("Compressed", r.Compressed, "blob and manifest bytes as they are in the archive")
	line("Uncompressed", r.Uncompressed, "after decompression; raw blobs counted as is")
	line("Added to CAS", r.Added, "appended to pack segments, manifests and rootfs tree included")
	switch ratio, ok := r.DedupRatio(); {
	case r.Blobs.Processed == 0:
		// A re-import stores no blob; what it adds is the rewritten manifest
		// metadata (meta.json and root nodes), a few hundred bytes.
		note := "everything already present"
		if r.Added > 0 {
			note += fmt.Sprintf(", %s bytes of manifest metadata rewritten", FormatCount(r.Added))
		}
		fmt.Fprintf(&b, "%-15s %s\n", "Dedup ratio", note)
	case ok:
		fmt.Fprintf(&b, "%-15s %-11s compressed bytes ÷ bytes added to CAS   (%.1f%% not written)\n", "Dedup ratio", fmt.Sprintf("%.2fx", ratio), r.NotWrittenPercent())
	default:
		fmt.Fprintf(&b, "%-15s %s\n", "Dedup ratio", "nothing added")
	}
	fmt.Fprintf(&b, "%-15s %-11s of offered bytes were already in the store\n", "Chunks reused", fmt.Sprintf("%.1f%%", r.ChunksReusedPercent()))
	return b.String()
}

// shortRef abbreviates an entry's digest as "sha256:xxxx…xxxx".
func shortRef(e importer.EntryReport) string {
	h := e.Digest.Hex()
	if len(h) >= 8 {
		return "sha256:" + h[:4] + "…" + h[len(h)-4:]
	}
	return e.Digest.String()
}

// kindOf describes an entry as "manifest" or an index's platform/attestation counts.
func kindOf(e importer.EntryReport) string {
	if !e.IsIndex {
		return "manifest"
	}
	return fmt.Sprintf("index, %s + %s", plural(e.Platforms, "platform"), plural(e.Attestations, "attestation"))
}

// rootfsOf summarizes an entry's rootfs outcome, or "" when it has none.
func rootfsOf(e importer.EntryReport) string {
	switch len(e.Rootfs) {
	case 0:
		return ""
	case 1:
		rf := e.Rootfs[0]
		if rf.Status == image.RootfsOK || rf.Status == image.RootfsPartial {
			return fmt.Sprintf("rootfs %s, %s entries", rf.Status, FormatCount(int64(rf.Entries)))
		}
		return "rootfs " + string(rf.Status)
	}
	ok := 0
	for _, rf := range e.Rootfs {
		if rf.Status == image.RootfsOK {
			ok++
		}
	}
	return fmt.Sprintf("rootfs %d/%d ok", ok, len(e.Rootfs))
}

// blobsLine summarizes how many blobs were processed, stored and already present.
func blobsLine(c importer.BlobCounts) string {
	total := c.Processed + c.Present
	if c.Processed == 0 {
		return fmt.Sprintf("%d processed, %d already present", total, c.Present)
	}
	stored := fmt.Sprintf("%d stored (%d prism, %d raw%s)", c.Prism+c.Raw, c.Prism, c.Raw, rawReasons(c.RawReasons))
	return fmt.Sprintf("%d processed: %s, %d already present", total, stored, c.Present)
}

// rawReasons renders why blobs were stored raw, e.g. ": 1 not-tar".
func rawReasons(m map[blob.RawReason]int) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if len(keys) == 1 {
			parts = append(parts, k)
		} else {
			parts = append(parts, fmt.Sprintf("%d %s", m[blob.RawReason(k)], k))
		}
	}
	return ": " + strings.Join(parts, ", ")
}

// plural renders n with word, pluralized ("1 platform", "2 platforms").
func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
