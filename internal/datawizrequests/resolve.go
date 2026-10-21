package datawizrequests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"energylens.local/cloud/internal/catalog"
	"energylens.local/cloud/internal/householdenergy"
	"energylens.local/cloud/internal/inventory"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type resolveEntry struct {
	ApplianceRef     string               `json:"appliance_ref"`
	ApplianceVersion int64                `json:"appliance_version"`
	DeviceKind       string               `json:"device_kind"`
	Category         string               `json:"category,omitempty"`
	Manufacturer     string               `json:"manufacturer,omitempty"`
	Model            string               `json:"model,omitempty"`
	FCCID            string               `json:"fcc_id,omitempty"`
	ICID             string               `json:"ic_id,omitempty"`
	SerialNumber     string               `json:"serial_number,omitempty"`
	UPC              string               `json:"upc,omitempty"`
	Label            *inventory.BulbLabel `json:"label,omitempty"`
	CatalogProductID string               `json:"catalog_product_id,omitempty"`
	MatchQuality     string               `json:"match_quality,omitempty"`
	Usage            map[string]float64   `json:"usage,omitempty"`
	DemoDeviceID     string               `json:"demo_device_id,omitempty"`
	Name             string               `json:"name,omitempty"`
}
type resolveCandidate struct {
	entry             resolveEntry
	applianceID       string
	version           int64
	attachmentID      *string
	attachmentVersion *int64
	demo              bool
}

var queueReasons = map[string]bool{"usage_answer": true, "admin": true, "owner_refresh": true, "low_tier": true,
	"stale": true, "source_config": true, "catalog_change": true, "schema": true}

// Queue participates in the caller's transaction when given a pgx.Tx.
func Queue(ctx context.Context, db interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, applianceID, reason string) error {
	if _, err := uuid.Parse(applianceID); err != nil {
		return err
	}
	if !queueReasons[reason] {
		return errors.New("invalid resolve reason")
	}
	_, err := db.Exec(ctx, `INSERT INTO datawiz_resolve_queue(appliance_id,reason) VALUES($1,$2) ON CONFLICT(appliance_id) DO UPDATE SET reason=EXCLUDED.reason,queued_at=clock_timestamp()`, applianceID, reason)
	return err
}

func (p Producer) resolveCandidates(ctx context.Context) ([]resolveCandidate, error) {
	rows, err := p.DB.Query(ctx, `SELECT a.id::text,a.version,a.category,a.identity,
  a.catalog_product_id::text,pr.brand,pr.full_model,pr.match_quality,t.id::text,t.version,EXISTS(SELECT 1 FROM attachments live JOIN sensor_endpoints s ON s.id=live.sensor_id WHERE live.appliance_id=a.id AND live.ended_at IS NULL AND s.archived_at IS NULL AND s.capabilities @> ARRAY['power','energy']::text[])
 FROM appliances a JOIN homes h ON h.id=a.home_id
 LEFT JOIN catalog_products pr ON pr.id=a.catalog_product_id
 LEFT JOIN LATERAL (SELECT id,version,sensor_id,ended_at FROM attachments WHERE appliance_id=a.id ORDER BY started_at DESC,id LIMIT 1) t ON true
 WHERE a.archived_at IS NULL AND h.deleted_at IS NULL ORDER BY a.updated_at,a.id`)
	if err != nil {
		return nil, err
	}
	out := []resolveCandidate{}
	for rows.Next() {
		var c resolveCandidate
		var raw []byte
		var metering bool
		var linkID, pBrand, pModel, pQuality *string
		if err = rows.Scan(&c.applianceID, &c.version, &c.entry.Category, &raw, &linkID, &pBrand, &pModel, &pQuality, &c.attachmentID, &c.attachmentVersion, &metering); err != nil {
			break
		}
		var identity inventory.Identity
		if err = json.Unmarshal(raw, &identity); err != nil {
			break
		}
		c.entry.ApplianceRef = "cloud:" + c.applianceID
		c.entry.ApplianceVersion = c.version
		c.entry.DeviceKind = "real_non_metering"
		if metering {
			c.entry.DeviceKind = "real_metering"
		}
		c.entry.Manufacturer = identity.Brand
		c.entry.Model = identity.FullModel
		c.entry.Label = identity.BulbLabel
		if linkID != nil {
			if quality, ok := catalog.SendableLink(identity.Brand, identity.FullModel, identity.MatchQuality, catalog.LinkedProduct{Brand: deref(pBrand), FullModel: deref(pModel), MatchQuality: deref(pQuality)}); ok {
				c.entry.CatalogProductID, c.entry.MatchQuality = *linkID, quality
			}
		}
		if identity.FCCID != nil {
			c.entry.FCCID = *identity.FCCID
		}
		if identity.ICID != nil {
			c.entry.ICID = *identity.ICID
		}
		if identity.SerialNumber != nil {
			c.entry.SerialNumber = *identity.SerialNumber
		}
		if identity.UPC != nil {
			c.entry.UPC = *identity.UPC
		}
		out = append(out, c)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	// Synthetic inventory uses exactly the same public IDs as inventory.List.
	rows, err = p.DB.Query(ctx, `SELECT id::text,demo_cost_snapshot FROM homes WHERE deleted_at IS NULL AND demo_cost_snapshot IS NOT NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var home string
		var raw []byte
		if err = rows.Scan(&home, &raw); err != nil {
			break
		}
		snapshot, e := householdenergy.DecodeDemoCostSnapshot(bytes.NewReader(raw))
		if e != nil {
			err = e
			break
		}
		demos, e := inventory.DemoAppliances(snapshot, home)
		if e != nil {
			err = e
			break
		}
		for _, a := range demos {
			out = append(out, resolveCandidate{entry: resolveEntry{ApplianceRef: "cloud:" + a.ID, ApplianceVersion: 1, DeviceKind: "simulated", Category: a.Category, DemoDeviceID: a.DemoDeviceID, Name: a.Name}, applianceID: a.ID, version: 1, demo: true})
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	return out, err
}

func (p Producer) resolveEntryFor(ctx context.Context, id string) (resolveCandidate, bool, error) {
	all, err := p.resolveCandidates(ctx)
	if err != nil {
		return resolveCandidate{}, false, err
	}
	for _, c := range all {
		if c.applianceID == id {
			return c, true, nil
		}
	}
	return resolveCandidate{}, false, nil
}

var usageKeys = map[string]string{
	".hours_per_week":      "hours_per_week",
	".cycles_per_week":     "cycles_per_week",
	".season_share":        "season_share",
	".age_band":            "age_factor",
	".unconditioned_space": "unconditioned_space",
}

func (p Producer) addUsage(ctx context.Context, c *resolveCandidate) error {
	if c.demo {
		return nil
	}
	rows, err := p.DB.Query(ctx, `SELECT DISTINCT ON (question_id) question_id,answer_value::text FROM question_answers WHERE appliance_id=$1 AND appliance_version=$2 AND status='confirmed' ORDER BY question_id,revision DESC`, c.applianceID, c.version)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, value string
		if err = rows.Scan(&id, &value); err != nil {
			return err
		}
		var n float64
		if _, err = fmt.Sscan(value, &n); err != nil {
			continue
		}
		if strings.HasPrefix(id, "avoid.") || strings.HasPrefix(id, "faq-") || strings.Contains(id, ".reviewed_cycle.") {
			continue
		}
		for suffix, key := range usageKeys {
			if strings.HasSuffix(id, suffix) {
				if c.entry.Usage == nil {
					c.entry.Usage = map[string]float64{}
				}
				c.entry.Usage[key] = n
			}
		}
	}
	return rows.Err()
}

func (p Producer) hasMarker(ctx context.Context, kind, id string, version int64) (bool, error) {
	var found bool
	err := p.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM datawiz_request_emissions WHERE route='resolve' AND source_kind=$1 AND source_id=$2 AND source_version=$3)`, kind, id, version).Scan(&found)
	return found, err
}
func (p Producer) mark(ctx context.Context, kind, id string, version int64) error {
	_, err := p.DB.Exec(ctx, `INSERT INTO datawiz_request_emissions(route,source_kind,source_id,source_version) VALUES('resolve',$1,$2,$3) ON CONFLICT DO NOTHING`, kind, id, version)
	return err
}
func (p Producer) emitResolve(ctx context.Context, c resolveCandidate, mark bool) error {
	if err := p.addUsage(ctx, &c); err != nil {
		return err
	}
	if err := writeFile(p.Dir, candidate{route: "resolve", payload: c.entry}); err != nil {
		return err
	}
	if !mark {
		return nil
	}
	kind := "appliance"
	if c.demo {
		kind = "demo_appliance"
	}
	if err := p.mark(ctx, kind, c.applianceID, c.version); err != nil {
		return err
	}
	if c.attachmentID != nil && c.attachmentVersion != nil {
		return p.mark(ctx, "attachment", *c.attachmentID, *c.attachmentVersion)
	}
	return nil
}
func (p Producer) emitNewResolutions(ctx context.Context) (int, error) {
	items, err := p.resolveCandidates(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, c := range items {
		kind := "appliance"
		if c.demo {
			kind = "demo_appliance"
		}
		found, err := p.hasMarker(ctx, kind, c.applianceID, c.version)
		if err != nil {
			return count, err
		}
		if found && c.attachmentID != nil && c.attachmentVersion != nil {
			found, err = p.hasMarker(ctx, "attachment", *c.attachmentID, *c.attachmentVersion)
			if err != nil {
				return count, err
			}
		}
		if found {
			continue
		}
		if err = p.emitResolve(ctx, c, true); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}
func (p Producer) drainQueue(ctx context.Context) (int, error) {
	return p.drainQueueFiltered(ctx, "")
}

// DrainQueuedAppliance writes only the requested entry for the operator action.
func (p Producer) DrainQueuedAppliance(ctx context.Context, applianceID string) (int, error) {
	if _, err := uuid.Parse(applianceID); err != nil {
		return 0, err
	}
	return p.drainQueueFiltered(ctx, applianceID)
}

func (p Producer) drainQueueFiltered(ctx context.Context, applianceID string) (int, error) {
	tx, err := p.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var filter any
	if applianceID != "" {
		filter = applianceID
	}
	rows, err := tx.Query(ctx, `SELECT appliance_id::text,queued_at FROM datawiz_resolve_queue WHERE ($1::uuid IS NULL OR appliance_id=$1) ORDER BY queued_at,appliance_id LIMIT 100 FOR UPDATE SKIP LOCKED`, filter)
	if err != nil {
		return 0, err
	}
	type item struct {
		id string
		at time.Time
	}
	items := []item{}
	for rows.Next() {
		var x item
		if err = rows.Scan(&x.id, &x.at); err != nil {
			break
		}
		items = append(items, x)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, x := range items {
		c, found, e := p.resolveEntryFor(ctx, x.id)
		if e != nil {
			return count, e
		}
		if found {
			if e = p.emitResolve(ctx, c, true); e != nil {
				return count, e
			}
			count++
		} else {
			slog.Warn("dropping resolve request for unknown appliance", "appliance_id", x.id)
		}
		_, e = tx.Exec(ctx, `DELETE FROM datawiz_resolve_queue WHERE appliance_id=$1 AND queued_at<=$2`, x.id, x.at)
		if e != nil {
			return count, e
		}
	}
	return count, tx.Commit(ctx)
}

func (p Producer) Exists(ctx context.Context, id string) (bool, error) {
	_, found, err := p.resolveEntryFor(ctx, id)
	return found, err
}
