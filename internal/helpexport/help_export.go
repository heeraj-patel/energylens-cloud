// Package helpexport writes the private research-queue file for unresolved devices.
package helpexport

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

// ExportHelp reads the research queue: live appliances whose stored Datawiz resolution is unresolved or
// category-typical, minus models that already carry manual research.
func ExportHelp(ctx context.Context, pool *pgxpool.Pool, path string) (int, error) {
	rows, err := pool.Query(ctx, `SELECT a.id::text,r.record->'identity'->>'manufacturer',r.record->'identity'->>'model',
 a.category,COALESCE(a.identity->>'fuel','electricity'),
 CASE WHEN r.record->>'status'='unresolved' THEN 'unresolved' ELSE 'category_typical' END,r.resolved_at
FROM datawiz_energy_resolutions r
JOIN appliances a ON left(r.appliance_ref,6)='cloud:' AND a.id::text=substr(r.appliance_ref,7) AND a.archived_at IS NULL
JOIN homes h ON h.id=a.home_id AND h.deleted_at IS NULL
WHERE (r.record->>'status'='unresolved' OR r.record->'source'->>'tier'='category_typical')
 AND COALESCE(r.record->'identity'->>'manufacturer','')<>'' AND COALESCE(r.record->'identity'->>'model','')<>''
 AND NOT EXISTS (SELECT 1 FROM datawiz_current c JOIN datawiz_observations o ON o.id=c.observation_id
  WHERE c.route='device' AND o.source->>'adapter'='manual_research'
   AND c.subject_key='device:'||trim(both '-' from regexp_replace(lower(r.record->'identity'->>'manufacturer'),'[^a-z0-9]+','-','g'))
    ||':'||regexp_replace(lower(r.record->'identity'->>'model'),'[^a-z0-9]','','g'))
ORDER BY a.id`)
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
