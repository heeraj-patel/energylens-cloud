package energyrating

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type stubRow struct{ scan func(...any) error }

func (r stubRow) Scan(dest ...any) error { return r.scan(dest...) }

type stubDB struct {
	query string
	args  []any
	row   pgx.Row
	exec  string
	tag   *pgconn.CommandTag
}

func (d *stubDB) QueryRow(_ context.Context, q string, args ...any) pgx.Row {
	d.query, d.args = q, args
	return d.row
}
func (d *stubDB) Exec(_ context.Context, q string, args ...any) (pgconn.CommandTag, error) {
	d.exec, d.args = q, args
	if d.tag != nil {
		return *d.tag, nil
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func TestFindReviewedRequiresReviewAndPreservesEvidence(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	annual := 318.0
	rowID := "epa-row-42"
	db := &stubDB{row: stubRow{scan: func(dest ...any) error {
		*dest[0].(*int64) = 9
		*dest[1].(*string) = "Lenovo"
		*dest[2].(*string) = "20XLS1H500"
		*dest[3].(*string) = "computer"
		*dest[4].(*string) = "epa_exact"
		*dest[5].(**float64) = &annual
		*dest[10].(*string) = "https://catalog.example/epa"
		*dest[11].(**string) = &rowID
		*dest[12].(**time.Time) = nil
		*dest[13].(*string) = "energy-v1"
		*dest[14].(*bool) = true
		*dest[15].(**time.Time) = &now
		return nil
	}}}
	got, ok, err := NewStore(db).FindReviewed(context.Background(), "Lenovo", "20XLS1H500", "computer")
	if err != nil || !ok {
		t.Fatalf("FindReviewed = (%+v, %v, %v)", got, ok, err)
	}
	if got.SourceKind != "epa_exact" || got.AnnualKWh == nil || *got.AnnualKWh != annual || got.SourceRowID == nil || *got.SourceRowID != rowID || !got.Reviewed {
		t.Fatalf("rating evidence was not preserved: %+v", got)
	}
	if !strings.Contains(db.query, "reviewed = TRUE") || !strings.Contains(db.query, "ORDER BY CASE source_kind") {
		t.Fatalf("review gate or evidence priority missing from query: %s", db.query)
	}
	if len(db.args) != 3 || db.args[1] != "20XLS1H500" {
		t.Fatalf("identity query args = %#v", db.args)
	}
}

func TestFindReviewedReturnsMissForUnreviewedOnly(t *testing.T) {
	db := &stubDB{row: stubRow{scan: func(...any) error { return pgx.ErrNoRows }}}
	got, ok, err := NewStore(db).FindReviewed(context.Background(), "Brand", "Model", "computer")
	if err != nil || ok || got != (Rating{}) {
		t.Fatalf("unreviewed candidate became readable: rating=%+v ok=%v err=%v", got, ok, err)
	}
}

func TestSaveDoesNotPromoteCandidateOrMergeProvenance(t *testing.T) {
	ref, row := "file://reviewed-manual.pdf", "page-3"
	db := &stubDB{}
	r := Candidate{Brand: "Lenovo", Model: "20XLS1H500", Category: "computer", SourceKind: "oem_family", SourceRef: ref, SourceRowID: &row, ContractVersion: "energy-v1"}
	if err := NewStore(db).SaveCandidate(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if db.args[9] != ref || db.args[10] != &row || !strings.Contains(db.exec, "VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,FALSE,NULL)") || !strings.Contains(db.exec, "ON CONFLICT (lower(brand), lower(model), source_kind)") || !strings.Contains(db.exec, "source_row_id = EXCLUDED.source_row_id") || !strings.Contains(db.exec, "WHERE energy_rating_catalog.reviewed = FALSE") {
		t.Fatalf("candidate write did not preserve provenance/review state: args=%#v query=%s", db.args, db.exec)
	}
}

func TestSaveCandidateReportsReviewedRowProtection(t *testing.T) {
	tag := pgconn.NewCommandTag("INSERT 0 0")
	db := &stubDB{tag: &tag}
	err := NewStore(db).SaveCandidate(context.Background(), Candidate{
		Brand: "Lenovo", Model: "20XLS1H500", Category: "computer",
		SourceKind: "epa_exact", SourceRef: "https://catalog.example/row",
		ContractVersion: "energy-v1",
	})
	if !errors.Is(err, ErrReviewedRatingProtected) {
		t.Fatalf("SaveCandidate error = %v, want ErrReviewedRatingProtected", err)
	}
}

func TestReviewIsSeparateExplicitTransition(t *testing.T) {
	db := &stubDB{}
	store := NewStore(db)
	if promoted, err := store.Review(context.Background(), "Lenovo", "20XLS1H500", "epa_exact", time.Time{}); err == nil || promoted {
		t.Fatalf("zero-time review = (%v, %v), want rejection", promoted, err)
	}
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.FixedZone("CDT", -5*60*60))
	promoted, err := store.Review(context.Background(), "Lenovo", "20XLS1H500", "epa_exact", at)
	if err != nil || !promoted {
		t.Fatalf("Review = (%v, %v)", promoted, err)
	}
	if !strings.Contains(db.exec, "SET reviewed = TRUE") || !strings.Contains(db.exec, "AND reviewed = FALSE") || db.args[3] != at.UTC() {
		t.Fatalf("explicit review transition query/args = %s / %#v", db.exec, db.args)
	}
}

func TestFindReviewedPropagatesStoreErrors(t *testing.T) {
	want := errors.New("database unavailable")
	db := &stubDB{row: stubRow{scan: func(...any) error { return want }}}
	_, _, err := NewStore(db).FindReviewed(context.Background(), "Brand", "Model", "computer")
	if !errors.Is(err, want) {
		t.Fatalf("FindReviewed error = %v, want %v", err, want)
	}
}
