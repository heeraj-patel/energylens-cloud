package datawizrequests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Producer exports persisted Cloud inputs as route-specific, atomic JSON files.
// The Datawiz CLI processes the files separately; Cloud never invokes it.
type Producer struct {
	DB  *pgxpool.Pool
	Dir string
}

type candidate struct {
	route, kind, id string
	version         int64
	payload         any
}

var usZIP = regexp.MustCompile(`^[0-9]{5}$`)

func (p Producer) candidates(ctx context.Context) ([]candidate, error) {
	rows, err := p.DB.Query(ctx, `SELECT a.id::text,a.version,a.identity->>'brand',a.identity->>'full_model',a.category
FROM appliances a JOIN homes h ON h.id=a.home_id
WHERE a.archived_at IS NULL AND h.deleted_at IS NULL AND
  NULLIF(btrim(a.identity->>'brand'),'') IS NOT NULL AND
  ((NULLIF(btrim(a.identity->>'full_model'),'') IS NOT NULL AND NOT EXISTS(SELECT 1 FROM datawiz_request_emissions e WHERE e.route='device' AND e.source_kind='appliance' AND e.source_id=a.id AND e.source_version=a.version))
   OR (NULLIF(btrim(a.category),'') IS NOT NULL AND NOT EXISTS(SELECT 1 FROM datawiz_request_emissions e WHERE e.route='savings' AND e.source_kind='appliance' AND e.source_id=a.id AND e.source_version=a.version)))
ORDER BY a.updated_at,a.id LIMIT 100`)
	if err != nil {
		return nil, err
	}
	var out []candidate
	for rows.Next() {
		var id, category string
		var version int64
		var brand, model *string
		if err = rows.Scan(&id, &version, &brand, &model, &category); err != nil {
			break
		}
		if brand == nil || strings.TrimSpace(*brand) == "" {
			continue
		}
		manufacturer := strings.TrimSpace(*brand)
		if model != nil && strings.TrimSpace(*model) != "" {
			out = append(out, candidate{"device", "appliance", id, version, map[string]string{"manufacturer": manufacturer, "model": strings.TrimSpace(*model)}})
		}
		if strings.TrimSpace(category) != "" {
			out = append(out, candidate{"savings", "appliance", id, version, map[string]any{"devices": []map[string]string{{"manufacturer": manufacturer, "device_type": strings.TrimSpace(category)}}}})
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = p.DB.Query(ctx, `SELECT l.id::text,l.revision,l.country_code,l.postal_code
FROM home_location_revisions l JOIN homes h ON h.id=l.home_id
WHERE h.deleted_at IS NULL AND l.revision=(SELECT max(revision) FROM home_location_revisions WHERE home_id=l.home_id)
AND NOT EXISTS(SELECT 1 FROM datawiz_request_emissions e WHERE e.route='zip' AND e.source_kind='location' AND e.source_id=l.id AND e.source_version=l.revision)
ORDER BY l.recorded_at,l.id LIMIT 100`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, country, postal string
		var version int64
		if err = rows.Scan(&id, &version, &country, &postal); err != nil {
			break
		}
		postal = strings.TrimSpace(postal)
		if country == "US" && usZIP.MatchString(postal) {
			out = append(out, candidate{"zip", "location", id, version, map[string]string{"zip": postal}})
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, contextName := range []string{"night", "away", "peak_usage", "habits", "appliance_type", "upgrades", "maintenance"} {
		id := uuid.NewSHA1(uuid.NameSpaceURL, []byte("energylens:datawiz:efficiency:"+contextName)).String()
		out = append(out, candidate{"efficiency", "context", id, 1, map[string]string{"context": contextName}})
	}
	return out, nil
}

func writeFile(dir string, c candidate) error {
	data, err := json.Marshal(c.payload)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, c.route)
	if err = os.MkdirAll(path, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(path, ".request-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(append(data, '\n')); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(path, uuid.NewString()+".json"))
}

// RunOnce finds new saved appliance identities and current US ZIPs. A new UUID
// names every emitted file. Repeated worker scans are bounded and deduplicated.
func (p Producer) RunOnce(ctx context.Context) (int, error) {
	if p.DB == nil || p.Dir == "" {
		return 0, errors.New("Datawiz producer needs DB and requests directory")
	}
	candidates, err := p.candidates(ctx)
	if err != nil {
		return 0, fmt.Errorf("query Datawiz inputs: %w", err)
	}
	count := 0
	for _, c := range candidates {
		var exists bool
		err = p.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM datawiz_request_emissions WHERE route=$1 AND source_kind=$2 AND source_id=$3 AND source_version=$4)`, c.route, c.kind, c.id, c.version).Scan(&exists)
		if err != nil {
			return count, err
		}
		if exists {
			continue
		}
		if err = writeFile(p.Dir, c); err != nil {
			return count, err
		}
		_, err = p.DB.Exec(ctx, `INSERT INTO datawiz_request_emissions(route,source_kind,source_id,source_version) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, c.route, c.kind, c.id, c.version)
		if err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}
