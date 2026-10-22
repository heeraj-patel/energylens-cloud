package reports

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func fullPDFPreview(t *testing.T) Preview {
	t.Helper()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	id := uuid.NewString()
	p := Preview{
		ID: id, HomeID: uuid.NewString(), SchemaVersion: "manual-preview/v2",
		CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour),
		Scope: "selected_appliances", HomeTotalStatus: "unavailable_incomplete_scope",
		MeasuredStatus: "partial", RecommendationsStatus: "available",
		Assumptions: []string{"Only selected appliances are included."},
		Items: []Item{{
			ApplianceID: uuid.NewString(), Category: "washer",
			Explanation: "A café washing machine's current estimate.",
			Estimate: &Estimate{
				Kind: "estimated_operating_cost", EnergyKWh: "12.5", CostUSD: "2.10",
				PeriodStart: "2026-10-01", PeriodEnd: "2026-11-01", AsOf: now,
				SourceRefs: []SourceRef{{ID: uuid.NewString(), Kind: "catalog_fact", Revision: 3}},
			},
			Measured: &MeasuredSummary{
				EnergyKWh: "4.25", PeriodStart: "2026-10-01", PeriodEnd: "2026-10-10",
				BucketCount: 9, Source: "daily_sensor_buckets", SourceSHA256: strings.Repeat("a", 64),
				SourceAsOf: now, Coverage: "partial",
			},
		}},
		Recommendations: []ReportRecommendation{{
			ApplianceID: id, RuleID: "review_usage", RuleVersion: 2, State: "available",
			Title: "Review usage", Explanation: "Check the measured period.",
			EvidenceRefs: []SourceRef{{ID: uuid.NewString(), Kind: "measured_summary", Revision: 1}},
		}},
	}
	var err error
	p.SnapshotSHA256, err = snapshotDigest(p)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRenderApprovedPDFIncludesFullV2Snapshot(t *testing.T) {
	p := fullPDFPreview(t)
	html, err := RenderApprovedHTML(p, p.SnapshotSHA256)
	if err != nil {
		t.Fatal(err)
	}
	text := reportPDFText(html.HTML)
	for _, required := range []string{
		"EnergyLens report", "Selected appliances",
		"A café washing machine's current estimate", "12.5 kWh and $2.10 USD",
		"Measured electricity: 4.25 kWh across 9 complete daily sensor buckets",
		"Coverage: partial", "Available recommendations", "Review usage",
		"measured_summary", "Scope and assumptions", "Only selected appliances are included.",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("report text missing %q", required)
		}
	}
	if strings.Contains(text, "latest_delivery") || strings.Contains(text, "recipients") ||
		strings.Contains(text, "source_sha256") || strings.Contains(text, "home_total_status") {
		t.Fatal("mutable delivery data leaked into PDF")
	}
	p.LatestDelivery = &DeliveryStatus{ID: uuid.NewString(), State: "sent"}
	pdf, err := RenderApprovedPDF(p, p.SnapshotSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) || len(pdf) < 1000 {
		t.Fatalf("invalid PDF, %d bytes", len(pdf))
	}
}

type pdfRouteStore struct {
	*routeStore
	preview Preview
}

func (s *pdfRouteStore) Get(_ context.Context, owner, home, id string, _ time.Time) (Preview, error) {
	s.read = true
	if owner != "verified-owner" || home != s.preview.HomeID || id != s.preview.ID {
		return Preview{}, ErrNotFound
	}
	return s.preview, nil
}

func TestPDFRouteRequiresOwnerFreshExactSnapshotAndDoesNotQueue(t *testing.T) {
	p := fullPDFPreview(t)
	store := &pdfRouteStore{routeStore: &routeStore{}, preview: p}
	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer valid" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	r := chi.NewRouter()
	Service{Store: store, Auth: auth, UserID: func(*http.Request) string { return "verified-owner" }}.Routes(r)
	call := func(home, token, digest string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/homes/"+home+"/reports/previews/"+p.ID+"/pdf", nil)
		if token != "" {
			req.Header.Set("Authorization", token)
		}
		if digest != "" {
			req.Header.Set("X-Report-Snapshot-SHA256", digest)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	if w := call(p.HomeID, "", p.SnapshotSHA256); w.Code != 401 || store.read {
		t.Fatalf("auth bypass: %d", w.Code)
	}
	if w := call(p.HomeID, "Bearer valid", ""); w.Code != 400 || store.read {
		t.Fatalf("digest required: %d", w.Code)
	}
	if w := call(uuid.NewString(), "Bearer valid", p.SnapshotSHA256); w.Code != 404 {
		t.Fatalf("cross-home access: %d", w.Code)
	}
	if w := call(p.HomeID, "Bearer valid", strings.Repeat("0", 64)); w.Code != 409 || !strings.Contains(w.Body.String(), "snapshot_mismatch") {
		t.Fatalf("wrong digest: %d %s", w.Code, w.Body.String())
	}
	store.preview.Stale = true
	if w := call(p.HomeID, "Bearer valid", p.SnapshotSHA256); w.Code != 409 || !strings.Contains(w.Body.String(), "preview_stale") {
		t.Fatalf("stale preview: %d %s", w.Code, w.Body.String())
	}
	store.preview.Stale = false
	w := call(p.HomeID, "Bearer valid", p.SnapshotSHA256)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/pdf" || w.Header().Get("Cache-Control") != "private, no-store" || !bytes.HasPrefix(w.Body.Bytes(), []byte("%PDF-")) {
		t.Fatalf("PDF response: %d %v", w.Code, w.Header())
	}
	if store.queued {
		t.Fatal("PDF download queued an email")
	}
}
