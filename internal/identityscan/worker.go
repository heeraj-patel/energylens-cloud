// Package identityscan keeps newly confirmed model research private and unreviewed.
package identityscan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Request struct {
	ID, Category, Manufacturer, Model, Fuel string
	Variant, Region                         *string
	Attempts                                int
}
type Scanner interface {
	Scan(context.Context, Request) (json.RawMessage, error)
}
type Store struct{ Pool *pgxpool.Pool }

// Claim takes a fresh request, or a timed-out claim. Missing-source retries
// are selected only by an explicit nightly pass.
func (s Store) Claim(ctx context.Context, nightly bool, now time.Time) (*Request, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var r Request
	err = tx.QueryRow(ctx, `WITH chosen AS (
 SELECT q.id FROM identity_scan_requests q JOIN homes h ON h.id=q.home_id
 WHERE h.deleted_at IS NULL AND q.attempt_count<5 AND (
   q.status='pending' OR (q.status='running' AND q.claimed_at<$1::timestamptz-interval '15 minutes')
   OR (q.status='failed' AND q.next_attempt_at<=$1)
   OR ($2 AND q.status='missing_source' AND q.next_attempt_at<=$1))
 ORDER BY q.created_at,q.id LIMIT 1 FOR UPDATE OF q SKIP LOCKED)
 UPDATE identity_scan_requests q SET status='running',claimed_at=$1,attempt_count=attempt_count+1,updated_at=$1
 FROM chosen WHERE q.id=chosen.id
 RETURNING q.id,q.category,q.manufacturer,q.model,q.fuel,q.variant,q.region,q.attempt_count`, now, nightly).Scan(&r.ID, &r.Category, &r.Manufacturer, &r.Model, &r.Fuel, &r.Variant, &r.Region, &r.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	return &r, tx.Commit(ctx)
}

func status(report json.RawMessage, r Request) (string, error) {
	var result struct {
		SchemaVersion int `json:"schema_version"`
		ModelCount    int `json:"model_count"`
		Models        []struct {
			Category     string `json:"category"`
			Manufacturer string `json:"manufacturer"`
			Model        string `json:"model"`
			Status       string `json:"status"`
			EPACheck     struct {
				Status string `json:"status"`
			} `json:"epa_check"`
			IdentityDiscovery struct {
				Status string `json:"status"`
			} `json:"identity_discovery"`
			SearchDiscovery struct {
				Status string `json:"status"`
			} `json:"search_discovery"`
		} `json:"models"`
	}
	if len(report) > 262144 || json.Unmarshal(report, &result) != nil || result.SchemaVersion != 1 || result.ModelCount != 1 || len(result.Models) != 1 {
		return "", errors.New("invalid candidate report")
	}
	model := result.Models[0]
	if model.Category != r.Category || model.Manufacturer != r.Manufacturer || model.Model != r.Model {
		return "", errors.New("candidate report identity mismatch")
	}
	if model.EPACheck.Status == "lookup_error" || model.IdentityDiscovery.Status == "lookup_error" || model.SearchDiscovery.Status == "lookup_error" {
		return "", errors.New("candidate source lookup failed")
	}
	switch model.Status {
	case "annual_manifest_present":
		return "reviewed_present", nil
	case "epa_review_candidate", "identity_review_candidate", "search_review_lead":
		return "review_pending", nil
	case "no_evidence", "rated_input_only":
		return "missing_source", nil
	default:
		return "", fmt.Errorf("unsupported candidate report status %q", model.Status)
	}
}

func (s Store) Finish(ctx context.Context, r Request, report json.RawMessage, scanErr error, now time.Time) error {
	state := "failed"
	var next *time.Time
	if scanErr == nil {
		var err error
		state, err = status(report, r)
		if err != nil {
			scanErr = err
		}
	}
	if scanErr != nil {
		report = nil
		state = "failed"
	}
	if state == "missing_source" && r.Attempts < 5 {
		when := now.Add(24 * time.Hour)
		next = &when
	} else if state == "failed" && r.Attempts < 5 {
		when := now.Add(time.Duration(15<<min(r.Attempts-1, 3)) * time.Minute)
		next = &when
	}
	_, err := s.Pool.Exec(ctx, `UPDATE identity_scan_requests SET status=$2,candidate_report=$3,next_attempt_at=$4,claimed_at=NULL,updated_at=$5 WHERE id=$1 AND status='running' AND attempt_count=$6`, r.ID, state, report, next, now, r.Attempts)
	return err
}

func (s Store) Run(ctx context.Context, scanner Scanner, nightly bool, limit int, now time.Time) (int, error) {
	if limit < 1 || limit > 10 {
		return 0, errors.New("scan batch must be 1..10")
	}
	completed := 0
	// The same exact model can be saved in several homes. Reuse one public
	// source lookup within this bounded pass, while retaining private rows.
	seen := map[string]json.RawMessage{}
	for completed < limit {
		r, err := s.Claim(ctx, nightly, now)
		if err != nil {
			return completed, err
		}
		if r == nil {
			return completed, nil
		}
		key := r.Category + "\x00" + r.Manufacturer + "\x00" + r.Model + "\x00" + r.Fuel
		if r.Variant != nil {
			key += "\x00" + *r.Variant
		}
		if r.Region != nil {
			key += "\x00" + *r.Region
		}
		report, cached := seen[key]
		var scanErr error
		if !cached {
			report, scanErr = scanner.Scan(ctx, *r)
			if scanErr == nil {
				seen[key] = report
			}
		}
		if err = s.Finish(ctx, *r, report, scanErr, now); err != nil {
			return completed, err
		}
		completed++
	}
	return completed, nil
}
