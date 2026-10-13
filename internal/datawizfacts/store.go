package datawizfacts

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Fact is a current approved Datawiz observation with its original provenance.
// These values are research evidence; callers must apply domain-specific
// household matching before using them as an actual bill or device estimate.
type Fact struct {
	ID          string          `json:"id"`
	Route       string          `json:"route"`
	SubjectKey  string          `json:"subject_key"`
	Field       string          `json:"field"`
	RecordKey   string          `json:"record_key"`
	Value       json.RawMessage `json:"value"`
	Source      json.RawMessage `json:"source"`
	CollectedAt string          `json:"collected_at"`
}

type Store struct{ DB *pgxpool.Pool }

func (s Store) Current(ctx context.Context, route, subjectKey string) ([]Fact, error) {
	if s.DB == nil {
		return nil, errors.New("Datawiz facts database is unavailable")
	}
	return Current(ctx, s.DB, route, subjectKey)
}

type Querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// Current also accepts a transaction, so a consumer can include observations
// in the same read snapshot as its other Cloud state.
func Current(ctx context.Context, db Querier, route, subjectKey string) ([]Fact, error) {
	rows, err := db.Query(ctx, `SELECT o.id::text,c.route,c.subject_key,c.field,c.record_key,o.value,o.source,to_char(o.collected_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"')
FROM datawiz_current c JOIN datawiz_observations o ON o.id=c.observation_id
WHERE c.route=$1 AND c.subject_key=$2 AND o.approval_status='approved'
ORDER BY c.field,c.record_key`, route, subjectKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	facts := []Fact{}
	for rows.Next() {
		var f Fact
		if err = rows.Scan(&f.ID, &f.Route, &f.SubjectKey, &f.Field, &f.RecordKey, &f.Value, &f.Source, &f.CollectedAt); err != nil {
			return nil, err
		}
		facts = append(facts, f)
	}
	return facts, rows.Err()
}

var nonAlphanumeric = regexp.MustCompile(`[^A-Z0-9]`)
var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func DeviceSubject(manufacturer, model string) string {
	return "device:" + strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(strings.TrimSpace(manufacturer)), "-"), "-") + ":" + strings.ToLower(nonAlphanumeric.ReplaceAllString(strings.ToUpper(model), ""))
}

func SavingsSubject(manufacturer, category string) string {
	slug := func(s string) string {
		return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-"), "-")
	}
	return "savings:" + slug(manufacturer) + ":" + slug(category)
}

// Device returns current approved research for the requested normalized model.
// Publication already decided which observations are current and approved; a
// manual source can provide a rating without a separate identity observation.
func Device(ctx context.Context, db Querier, manufacturer, model string) ([]Fact, error) {
	return Current(ctx, db, "device", DeviceSubject(manufacturer, model))
}
