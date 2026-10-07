package identityscan

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// HelpRecord contains only the appliance identity needed for human research.
type HelpRecord struct {
	ApplianceID        string    `json:"appliance_id"`
	Manufacturer       string    `json:"manufacturer"`
	Model              string    `json:"model"`
	Category           string    `json:"category"`
	Fuel               string    `json:"fuel"`
	NormalizedIdentity string    `json:"normalized_identity"`
	LookupStatus       string    `json:"lookup_status"`
	LookupDate         time.Time `json:"lookup_date"`
}

func normalizeIdentity(category, manufacturer, model, fuel string) string {
	parts := []string{category, manufacturer, model, fuel}
	for i, part := range parts {
		parts[i] = strings.ToLower(strings.Join(strings.Fields(part), " "))
	}
	return strings.Join(parts, "|")
}

// ExportHelp reads final KB no-evidence outcomes. A transient lookup error or
// a scheduled retry never enters the human queue.
func ExportHelp(ctx context.Context, pool *pgxpool.Pool, path string) (int, error) {
	rows, err := pool.Query(ctx, `SELECT q.appliance_id::text,q.manufacturer,q.model,q.category,q.fuel,q.status,q.updated_at
FROM identity_scan_requests q JOIN homes h ON h.id=q.home_id
WHERE h.deleted_at IS NULL AND q.status='missing_source' AND q.attempt_count=5
 AND q.next_attempt_at IS NULL AND q.candidate_report IS NOT NULL
 AND q.candidate_report->'models'->0->>'status' IN ('no_evidence','rated_input_only')
ORDER BY q.appliance_id,q.updated_at,q.id`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var records []HelpRecord
	for rows.Next() {
		var r HelpRecord
		if err := rows.Scan(&r.ApplianceID, &r.Manufacturer, &r.Model, &r.Category, &r.Fuel, &r.LookupStatus, &r.LookupDate); err != nil {
			return 0, err
		}
		r.NormalizedIdentity = normalizeIdentity(r.Category, r.Manufacturer, r.Model, r.Fuel)
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if err := writeHelpAtomic(path, records); err != nil {
		return 0, err
	}
	return len(records), nil
}

func (s Store) ExportHelp(ctx context.Context, path string) (int, error) {
	return ExportHelp(ctx, s.Pool, path)
}

func writeHelpAtomic(path string, records []HelpRecord) (err error) {
	if path == "" || filepath.Base(path) != "unresolved_device_help.jsonl" {
		return errors.New("output must be named unresolved_device_help.jsonl")
	}
	if info, statErr := os.Lstat(path); statErr == nil {
		if !info.Mode().IsRegular() {
			return errors.New("output must be a regular file")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".unresolved-device-help-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, record := range records {
		line, marshalErr := json.Marshal(record)
		if marshalErr != nil {
			return marshalErr
		}
		if _, err := fmt.Fprintln(w, string(line)); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
