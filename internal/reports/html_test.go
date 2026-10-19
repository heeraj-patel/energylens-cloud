package reports

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRenderApprovedHTMLIsEscapedDeterministicAndDigestBound(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	p := Preview{ID: "report-id", HomeID: "private-home-id", SchemaVersion: "manual-preview/v1", CreatedAt: now, ExpiresAt: now.Add(time.Hour), Scope: "selected_appliances", Assumptions: []string{`Use <img src=x onerror=alert(1)> only as an estimate.`}, Items: []Item{{ApplianceID: `device<script>alert(1)</script>`, Category: `fridge<b>`, Explanation: `Estimated & <script>alert(1)</script>`, Estimate: &Estimate{Kind: "estimated_operating_cost", EnergyKWh: "12.5", CostUSD: "2.00", PeriodStart: "2026-10-01", PeriodEnd: "2026-11-01", AsOf: now, SourceRefs: []SourceRef{{ID: `source"><a href="https://evil.test">`, Kind: "catalog_fact", Revision: 1}}}}}}
	var err error
	p.SnapshotSHA256, err = snapshotDigest(p)
	if err != nil {
		t.Fatal(err)
	}
	first, err := RenderApprovedHTML(p, p.SnapshotSHA256)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RenderApprovedHTML(p, p.SnapshotSHA256)
	if err != nil || first != second {
		t.Fatalf("render changed: %v", err)
	}
	for _, fragment := range []string{"<!doctype html>", "lang=\"en\"", "<main>", "<h1>", "<ol>", "<li>", "This is an estimate, not measured use or a savings claim.", "Forwarded copies cannot be recalled", p.SnapshotSHA256, "&lt;script&gt;", "&lt;img"} {
		if !strings.Contains(first.HTML, fragment) {
			t.Fatalf("missing report content %q", fragment)
		}
	}
	for _, forbidden := range []string{"<script", "<img", "<a ", "private-home-id", "tracking", "Estimate evidence as of", "catalog_fact", "source&#34;&gt;"} {
		if strings.Contains(first.HTML, forbidden) {
			t.Fatalf("unsafe or private report content %q", forbidden)
		}
	}
	if first.Subject != "EnergyLens report" || first.SnapshotSHA256 != p.SnapshotSHA256 {
		t.Fatalf("report envelope: %+v", first)
	}
	withoutEstimate := p
	withoutEstimate.Items = []Item{{ApplianceID: "device", Category: "other", Explanation: "No current monthly estimate is available for this appliance."}}
	withoutEstimate.SnapshotSHA256, err = snapshotDigest(withoutEstimate)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := RenderApprovedHTML(withoutEstimate, withoutEstimate.SnapshotSHA256)
	if err != nil || !strings.Contains(missing.HTML, "No current monthly estimate is available.</p>") {
		t.Fatalf("approved empty estimate copy missing: %v", err)
	}
	changed := p
	changed.Assumptions = []string{"changed after approval"}
	if _, err := RenderApprovedHTML(changed, p.SnapshotSHA256); !errors.Is(err, ErrSnapshotMismatch) {
		t.Fatalf("changed snapshot rendered: %v", err)
	}
	changed = p
	changed.Stale = true
	if _, err := RenderApprovedHTML(changed, p.SnapshotSHA256); !errors.Is(err, ErrStalePreview) {
		t.Fatalf("stale snapshot rendered: %v", err)
	}
	if _, err := RenderApprovedHTML(p, strings.Repeat("0", 64)); !errors.Is(err, ErrSnapshotMismatch) {
		t.Fatalf("unapproved digest rendered: %v", err)
	}
}
