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

	"energylens.local/cloud/internal/catalog"
	"energylens.local/cloud/internal/datawizfacts"
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

func subjectID(subject string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("energylens:datawiz:"+subject)).String()
}

func (p Producer) candidates(ctx context.Context) ([]candidate, error) {
	rows, err := p.DB.Query(ctx, `SELECT a.identity->>'brand',a.identity->>'full_model',a.category,pr.brand,pr.full_model,pr.match_quality
FROM appliances a JOIN homes h ON h.id=a.home_id LEFT JOIN catalog_products pr ON pr.id=a.catalog_product_id
WHERE a.archived_at IS NULL AND h.deleted_at IS NULL AND NULLIF(btrim(a.identity->>'brand'),'') IS NOT NULL
ORDER BY a.updated_at,a.id`)
	if err != nil {
		return nil, err
	}
	var out []candidate
	for rows.Next() {
		var brand, model, category, pBrand, pModel, pQuality *string
		if err = rows.Scan(&brand, &model, &category, &pBrand, &pModel, &pQuality); err != nil {
			break
		}
		manufacturer := strings.TrimSpace(deref(brand))
		modelText := strings.TrimSpace(deref(model))
		if modelText != "" {
			quality, _ := catalog.SendableLink(manufacturer, modelText, catalog.LinkedProduct{Brand: deref(pBrand), FullModel: deref(pModel), MatchQuality: deref(pQuality)})
			if quality != "confirmed_exact" {
				out = append(out, candidate{"device", "model", subjectID(datawizfacts.DeviceSubject(manufacturer, modelText)), 1, map[string]string{"manufacturer": manufacturer, "model": modelText}})
			}
		}
		if cat := strings.TrimSpace(deref(category)); cat != "" {
			out = append(out, candidate{"savings", "offer", subjectID(datawizfacts.SavingsSubject(manufacturer, cat)), 1, map[string]any{"devices": []map[string]string{{"manufacturer": manufacturer, "device_type": cat}}}})
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = p.DB.Query(ctx, `SELECT l.postal_code
FROM home_location_revisions l JOIN homes h ON h.id=l.home_id
WHERE h.deleted_at IS NULL AND l.country_code='US' AND l.revision=(SELECT max(revision) FROM home_location_revisions WHERE home_id=l.home_id)
ORDER BY l.recorded_at,l.id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var postal string
		if err = rows.Scan(&postal); err != nil {
			break
		}
		if postal = strings.TrimSpace(postal); usZIP.MatchString(postal) {
			out = append(out, candidate{"zip", "zip", subjectID("zip:" + postal), 1, map[string]string{"zip": postal}})
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
	count, err := p.drainQueue(ctx)
	if err != nil {
		return count, err
	}
	resolved, err := p.emitNewResolutions(ctx)
	count += resolved
	if err != nil {
		return count, err
	}
	candidates, err := p.candidates(ctx)
	if err != nil {
		return 0, fmt.Errorf("query Datawiz inputs: %w", err)
	}
	seen := map[string]bool{}
	for _, c := range candidates {
		key := c.route + "|" + c.kind + "|" + c.id
		if seen[key] {
			continue
		}
		seen[key] = true
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

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
