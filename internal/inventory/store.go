package inventory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"energylens.local/cloud/internal/householdenergy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("appliance or home not found")
var ErrVersion = errors.New("appliance version conflict")
var ErrKeyReuse = errors.New("appliance idempotency key reused")
var ErrCatalogMismatch = errors.New("catalog product does not match confirmed exact identity")
var ErrDisclosureRequired = errors.New("history disclosure acceptance required")
var ErrAttachmentConflict = errors.New("active attachment cannot safely close")

type Identity struct {
	Brand            string  `json:"brand"`
	FullModel        string  `json:"full_model"`
	Variant          *string `json:"variant,omitempty"`
	Region           *string `json:"region,omitempty"`
	Fuel             string  `json:"fuel"`
	MatchQuality     string  `json:"match_quality"`
	CatalogProductID *string `json:"catalog_product_id,omitempty"`
}
type Appliance struct {
	ID                  string         `json:"id"`
	DemoDeviceID        string         `json:"demo_device_id,omitempty"`
	DemoFixtureRevision string         `json:"-"`
	HomeID              string         `json:"home_id"`
	Name                string         `json:"name"`
	Category            string         `json:"category"`
	Room                *string        `json:"room,omitempty"`
	Identity            Identity       `json:"identity"`
	Version             int64          `json:"version"`
	CreatedAt           time.Time      `json:"created_at,omitempty"`
	UpdatedAt           time.Time      `json:"updated_at,omitempty"`
	ArchivedAt          *time.Time     `json:"archived_at"`
	KbWasteResult       *KbWasteResult `json:"kb_waste_result,omitempty"`
}

// Synthetic inventory has no creation or update event. Omit Go's zero-time
// encoding instead of presenting year 0001 as a real appliance timestamp.
func (a Appliance) MarshalJSON() ([]byte, error) {
	type applianceAlias Appliance
	if a.DemoDeviceID == "" {
		return json.Marshal(applianceAlias(a))
	}
	return json.Marshal(struct {
		ID           string     `json:"id"`
		DemoDeviceID string     `json:"demo_device_id"`
		HomeID       string     `json:"home_id"`
		Name         string     `json:"name"`
		Category     string     `json:"category"`
		Identity     Identity   `json:"identity"`
		Version      int64      `json:"version"`
		ArchivedAt   *time.Time `json:"archived_at"`
	}{a.ID, a.DemoDeviceID, a.HomeID, a.Name, a.Category, a.Identity, a.Version, a.ArchivedAt})
}

// KbWasteResult is emitted only for an explicit terminal waste-specific KB finding.
type KbWasteResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}
type CreateInput struct {
	Name     string   `json:"name"`
	Category string   `json:"category"`
	Room     *string  `json:"room,omitempty"`
	Identity Identity `json:"identity"`
}
type PatchInput struct {
	Name     *string         `json:"name,omitempty"`
	Category *string         `json:"category,omitempty"`
	Room     json.RawMessage `json:"room,omitempty"`
	Identity *Identity       `json:"identity,omitempty"`
}
type Store interface {
	List(context.Context, string, string) ([]Appliance, error)
	ListWithRevision(context.Context, string, string) ([]Appliance, string, error)
	Get(context.Context, string, string, string) (Appliance, error)
	Create(context.Context, string, string, CreateInput, string, []byte) (Appliance, error)
	Patch(context.Context, string, string, string, PatchInput, int64, string, []byte) (Appliance, error)
	Archive(context.Context, string, string, string, int64, string, []byte) (Appliance, error)
}
type PGStore struct{ Pool *pgxpool.Pool }

const selectAppliance = `SELECT a.id,a.home_id,a.name,a.category,a.room,a.identity,a.version,a.created_at,a.updated_at,a.archived_at FROM appliances a`

type row interface{ Scan(...any) error }

func scanAppliance(r row) (Appliance, error) {
	var a Appliance
	var identity []byte
	err := r.Scan(&a.ID, &a.HomeID, &a.Name, &a.Category, &a.Room, &identity, &a.Version, &a.CreatedAt, &a.UpdatedAt, &a.ArchivedAt)
	if err != nil {
		return a, err
	}
	err = json.Unmarshal(identity, &a.Identity)
	return a, err
}
func (s PGStore) ownedHome(ctx context.Context, tx pgx.Tx, owner, home string) error {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL FOR UPDATE`, home, owner).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var accepted bool
	err = tx.QueryRow(ctx, `SELECT history_disclosure_accepted_at IS NOT NULL FROM account_preferences WHERE user_id=$1`, owner).Scan(&accepted)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDisclosureRequired
	}
	if err != nil {
		return err
	}
	if !accepted {
		return ErrDisclosureRequired
	}
	return nil
}
func (s PGStore) List(ctx context.Context, owner, home string) ([]Appliance, error) {
	out, _, err := s.ListWithRevision(ctx, owner, home)
	return out, err
}

// ListWithRevision hashes the complete persisted inventory, including archived
// rows, from one database snapshot. Archiving the last active row therefore
// cannot return the revision of a never-used empty home.
func (s PGStore) ListWithRevision(ctx context.Context, owner, home string) ([]Appliance, string, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx)
	var rawDemo []byte
	var hasEnergyHistory bool
	err = tx.QueryRow(ctx, `SELECT h.demo_cost_snapshot,
 EXISTS(SELECT 1 FROM ingest_events e WHERE e.home_id=h.id AND e.event_kind='energy_interval')
 FROM homes h WHERE h.id=$1 AND h.owner_user_id=$2 AND h.deleted_at IS NULL`, home, owner).Scan(&rawDemo, &hasEnergyHistory)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	rows, err := tx.Query(ctx, selectAppliance+` JOIN homes h ON h.id=a.home_id WHERE h.id=$1 AND h.owner_user_id=$2 AND h.deleted_at IS NULL ORDER BY a.created_at,a.id`, home, owner)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []Appliance{}
	all := []Appliance{}
	for rows.Next() {
		a, e := scanAppliance(rows)
		if e != nil {
			return nil, "", e
		}
		all = append(all, a)
		if a.ArchivedAt == nil {
			out = append(out, a)
		}
	}
	if err = rows.Err(); err != nil {
		return nil, "", err
	}
	rows.Close()
	if err = tx.Commit(ctx); err != nil {
		return nil, "", err
	}
	if err := s.attachKbWasteResults(ctx, owner, home, out); err != nil {
		return nil, "", err
	}
	if len(all) == 0 && !hasEnergyHistory && len(rawDemo) > 0 && string(rawDemo) != "null" {
		snapshot, err := householdenergy.DecodeDemoCostSnapshot(bytes.NewReader(rawDemo))
		if err != nil {
			return nil, "", err
		}
		demoRows, err := demoAppliances(snapshot, home)
		if err != nil {
			return nil, "", err
		}
		out = append(out, demoRows...)
		all = append(all, demoRows...)
		fingerprint := sha256.Sum256(rawDemo)
		revision := inventoryRevision("persisted+demo:"+hex.EncodeToString(fingerprint[:]), home, all)
		return out, revision, nil
	}
	revision := inventoryRevision("persisted", home, all)
	return out, revision, nil
}

func demoAppliances(snapshot householdenergy.DemoCostSnapshot, home string) ([]Appliance, error) {
	rows := make([]Appliance, 0, 10)
	for _, device := range snapshot.Devices {
		if device.Name == nil || strings.TrimSpace(*device.Name) == "" {
			continue
		}
		id := uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://energylens.local/demo/"+home+"/"+device.ID)).String()
		rows = append(rows, Appliance{
			ID:                  id,
			DemoDeviceID:        device.ID,
			DemoFixtureRevision: snapshot.FixtureRevision,
			HomeID:              home,
			Name:                *device.Name,
			Identity:            Identity{MatchQuality: "unresolved"},
			Version:             1,
		})
	}
	if len(rows) != 10 {
		return nil, errors.New("demo cost snapshot must have ten named devices")
	}
	return rows, nil
}
func (s PGStore) Get(ctx context.Context, owner, home, id string) (Appliance, error) {
	a, err := scanAppliance(s.Pool.QueryRow(ctx, selectAppliance+` JOIN homes h ON h.id=a.home_id WHERE a.id=$1 AND a.home_id=$2 AND h.owner_user_id=$3 AND h.deleted_at IS NULL AND a.archived_at IS NULL`, id, home, owner))
	if errors.Is(err, pgx.ErrNoRows) {
		return Appliance{}, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	items := []Appliance{a}
	if err = s.attachKbWasteResults(ctx, owner, home, items); err != nil {
		return Appliance{}, err
	}
	return items[0], nil
}

// Candidate reports currently describe annual energy, so their status alone
// never establishes a waste finding. This optional field requires an explicit
// waste-specific result in the report, a completed scan, and the current identity.
func (s PGStore) attachKbWasteResults(ctx context.Context, owner, home string, appliances []Appliance) error {
	if len(appliances) == 0 {
		return nil
	}
	byID := make(map[string]*Appliance, len(appliances))
	for i := range appliances {
		byID[appliances[i].ID] = &appliances[i]
	}
	rows, err := s.Pool.Query(ctx, `SELECT q.id,q.appliance_id,q.identity_key FROM identity_scan_requests q
JOIN homes h ON h.id=q.home_id
WHERE q.home_id=$1 AND h.owner_user_id=$2 AND h.deleted_at IS NULL
AND q.status IN ('missing_source','reviewed_present') AND q.next_attempt_at IS NULL
AND (q.status='reviewed_present' OR q.attempt_count=5)
AND q.candidate_report->'models'->0->'waste_evidence'->>'status'='not_found'
AND q.candidate_report->'models'->0->'waste_evidence'->'checked_sources'='["epa","configured_site_search"]'::jsonb
AND q.candidate_report->'models'->0->'waste_evidence'->'search'->>'status'='no_search_leads'`, home, owner)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, applianceID string
		var key []byte
		if err := rows.Scan(&id, &applianceID, &key); err != nil {
			return err
		}
		if a := byID[applianceID]; a != nil && a.Identity.MatchQuality == "confirmed_exact" &&
			a.Identity.CatalogProductID == nil && bytes.Equal(key, scanKey(*a)) {
			a.KbWasteResult = &KbWasteResult{ID: id, Status: "waste_evidence_unavailable_after_kb"}
		}
	}
	return rows.Err()
}
func replay(ctx context.Context, tx pgx.Tx, owner, home, op, key string, hash []byte) (Appliance, bool, error) {
	var previous, body []byte
	err := tx.QueryRow(ctx, `SELECT request_hash,response_json FROM appliance_idempotency WHERE owner_user_id=$1 AND home_id=$2 AND operation=$3 AND key=$4`, owner, home, op, key).Scan(&previous, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return Appliance{}, false, nil
	}
	if err != nil {
		return Appliance{}, false, err
	}
	if !bytes.Equal(previous, hash) {
		return Appliance{}, true, ErrKeyReuse
	}
	var a Appliance
	err = json.Unmarshal(body, &a)
	return a, true, err
}
func saveReplay(ctx context.Context, tx pgx.Tx, owner, home, op, key string, hash []byte, a Appliance) error {
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO appliance_idempotency(owner_user_id,home_id,operation,key,request_hash,response_json) VALUES($1,$2,$3,$4,$5,$6)`, owner, home, op, key, hash, body)
	return err
}
func checkCatalog(ctx context.Context, tx pgx.Tx, a Appliance) error {
	id := a.Identity.CatalogProductID
	if id == nil {
		return nil
	}
	if a.Identity.MatchQuality != "confirmed_exact" {
		return ErrCatalogMismatch
	}
	var category, brand, model, fuel, quality string
	var variant, region *string
	err := tx.QueryRow(ctx, `SELECT category,brand,full_model,variant,region,fuel,match_quality FROM catalog_products WHERE id=$1`, *id).Scan(&category, &brand, &model, &variant, &region, &fuel, &quality)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCatalogMismatch
	}
	if err != nil {
		return err
	}
	if category != a.Category || brand != a.Identity.Brand || model != a.Identity.FullModel || fuel != a.Identity.Fuel || quality != "confirmed_exact" || !sameOptional(variant, a.Identity.Variant) || !sameOptional(region, a.Identity.Region) {
		return ErrCatalogMismatch
	}
	return nil
}
func sameOptional(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
func snapshot(ctx context.Context, tx pgx.Tx, a Appliance) error {
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO appliance_revisions(appliance_id,home_id,version,snapshot) VALUES($1,$2,$3,$4)`, a.ID, a.HomeID, a.Version, body)
	return err
}
func scanKey(a Appliance) []byte {
	parts := []string{a.Category, a.Identity.Brand, a.Identity.FullModel, a.Identity.Fuel}
	for _, value := range []*string{a.Identity.Variant, a.Identity.Region} {
		if value == nil {
			parts = append(parts, "")
		} else {
			parts = append(parts, *value)
		}
	}
	canonical := strings.ToLower(strings.Join(parts, "\x00"))
	hash := sha256.Sum256([]byte(canonical))
	return hash[:]
}

// enqueueScan is part of the appliance write transaction. This private request
// stays home-scoped because a user-entered model might contain sensitive text.
func enqueueScan(ctx context.Context, tx pgx.Tx, a Appliance) error {
	if a.Identity.CatalogProductID != nil || a.Identity.MatchQuality != "confirmed_exact" || len(a.Category) > 100 || len(a.Identity.Brand) > 100 || len(a.Identity.FullModel) > 100 || strings.EqualFold(a.Identity.Brand, "Unknown") || strings.EqualFold(a.Identity.FullModel, "Unknown") {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO identity_scan_requests(home_id,appliance_id,identity_key,category,manufacturer,model,fuel,variant,region)
SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9 WHERE NOT EXISTS (
 SELECT 1 FROM catalog_products p WHERE lower(p.category)=lower($4) AND lower(p.brand)=lower($5) AND lower(p.full_model)=lower($6) AND p.fuel=$7 AND p.match_quality='confirmed_exact'
 AND (p.variant IS NULL OR lower(p.variant)=lower($8)) AND (p.region IS NULL OR lower(p.region)=lower($9))
) ON CONFLICT(home_id,identity_key) DO NOTHING`, a.HomeID, a.ID, scanKey(a), a.Category, a.Identity.Brand, a.Identity.FullModel, a.Identity.Fuel, a.Identity.Variant, a.Identity.Region)
	return err
}
func (s PGStore) Create(ctx context.Context, owner, home string, in CreateInput, key string, hash []byte) (Appliance, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Appliance{}, err
	}
	defer tx.Rollback(ctx)
	if err = s.ownedHome(ctx, tx, owner, home); err != nil {
		return Appliance{}, err
	}
	if a, found, e := replay(ctx, tx, owner, home, "create_appliance", key, hash); found || e != nil {
		return a, e
	}
	a := Appliance{HomeID: home, Name: in.Name, Category: in.Category, Room: in.Room, Identity: in.Identity, Version: 1}
	if err = checkCatalog(ctx, tx, a); err != nil {
		return Appliance{}, err
	}
	identity, _ := json.Marshal(a.Identity)
	err = tx.QueryRow(ctx, `INSERT INTO appliances(home_id,name,category,room,catalog_product_id,identity) VALUES($1,$2,$3,$4,$5,$6) RETURNING id,created_at,updated_at`, home, a.Name, a.Category, a.Room, a.Identity.CatalogProductID, identity).Scan(&a.ID, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return Appliance{}, err
	}
	if err = snapshot(ctx, tx, a); err != nil {
		return Appliance{}, err
	}
	if err = enqueueScan(ctx, tx, a); err != nil {
		return Appliance{}, err
	}
	if err = saveReplay(ctx, tx, owner, home, "create_appliance", key, hash, a); err != nil {
		return Appliance{}, err
	}
	return a, tx.Commit(ctx)
}
func (s PGStore) Patch(ctx context.Context, owner, home, id string, in PatchInput, version int64, key string, hash []byte) (Appliance, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Appliance{}, err
	}
	defer tx.Rollback(ctx)
	if err = s.ownedHome(ctx, tx, owner, home); err != nil {
		return Appliance{}, err
	}
	op := "patch_appliance:" + id
	if a, found, e := replay(ctx, tx, owner, home, op, key, hash); found || e != nil {
		return a, e
	}
	a, err := scanAppliance(tx.QueryRow(ctx, selectAppliance+` WHERE a.id=$1 AND a.home_id=$2 AND a.archived_at IS NULL FOR UPDATE`, id, home))
	if errors.Is(err, pgx.ErrNoRows) {
		return Appliance{}, ErrNotFound
	}
	if err != nil {
		return Appliance{}, err
	}
	if a.Version != version {
		return Appliance{}, ErrVersion
	}
	if in.Name != nil {
		a.Name = *in.Name
	}
	if in.Category != nil {
		a.Category = *in.Category
	}
	if in.Identity != nil {
		a.Identity = *in.Identity
	}
	if in.Room != nil {
		if bytes.Equal(bytes.TrimSpace(in.Room), []byte("null")) {
			a.Room = nil
		} else {
			var room string
			if err = json.Unmarshal(in.Room, &room); err != nil {
				return Appliance{}, err
			}
			a.Room = &room
		}
	}
	if err = checkCatalog(ctx, tx, a); err != nil {
		return Appliance{}, err
	}
	identity, _ := json.Marshal(a.Identity)
	err = tx.QueryRow(ctx, `UPDATE appliances SET name=$3,category=$4,room=$5,catalog_product_id=$6,identity=$7,version=version+1,updated_at=now() WHERE id=$1 AND home_id=$2 RETURNING version,updated_at`, id, home, a.Name, a.Category, a.Room, a.Identity.CatalogProductID, identity).Scan(&a.Version, &a.UpdatedAt)
	if err != nil {
		return Appliance{}, err
	}
	if err = snapshot(ctx, tx, a); err != nil {
		return Appliance{}, err
	}
	if (in.Identity != nil || in.Category != nil) && a.Identity.CatalogProductID == nil {
		if err = enqueueScan(ctx, tx, a); err != nil {
			return Appliance{}, err
		}
	}
	if err = saveReplay(ctx, tx, owner, home, op, key, hash, a); err != nil {
		return Appliance{}, err
	}
	return a, tx.Commit(ctx)
}

// Archive closes active attachments at the database clock before hiding the
// appliance from current inventory. Historical rows and snapshots are retained.
func (s PGStore) Archive(ctx context.Context, owner, home, id string, version int64, key string, hash []byte) (Appliance, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Appliance{}, err
	}
	defer tx.Rollback(ctx)
	// All owner attachment mutations take this home lock first.
	if err = s.ownedHome(ctx, tx, owner, home); err != nil {
		return Appliance{}, err
	}
	op := "archive_appliance:" + id
	if a, found, e := replay(ctx, tx, owner, home, op, key, hash); found || e != nil {
		return a, e
	}
	a, err := scanAppliance(tx.QueryRow(ctx, selectAppliance+` WHERE a.id=$1 AND a.home_id=$2 AND a.archived_at IS NULL FOR UPDATE`, id, home))
	if errors.Is(err, pgx.ErrNoRows) {
		return Appliance{}, ErrNotFound
	}
	if err != nil {
		return Appliance{}, err
	}
	if a.Version != version {
		return Appliance{}, ErrVersion
	}
	var closedAt time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&closedAt); err != nil {
		return Appliance{}, err
	}
	rows, err := tx.Query(ctx, `SELECT id,started_at FROM attachments WHERE home_id=$1 AND appliance_id=$2 AND ended_at IS NULL ORDER BY id FOR UPDATE`, home, id)
	if err != nil {
		return Appliance{}, err
	}
	type openAttachment struct {
		id    string
		start time.Time
	}
	var open []openAttachment
	for rows.Next() {
		var item openAttachment
		if err = rows.Scan(&item.id, &item.start); err != nil {
			rows.Close()
			return Appliance{}, err
		}
		open = append(open, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Appliance{}, err
	}
	for _, item := range open {
		if !closedAt.After(item.start) {
			return Appliance{}, ErrAttachmentConflict
		}
		var futureEvidence bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM energy_observations WHERE attachment_revision_id=$1 AND end_at>$2)`, item.id, closedAt).Scan(&futureEvidence)
		if err != nil {
			return Appliance{}, err
		}
		if futureEvidence {
			return Appliance{}, ErrAttachmentConflict
		}
		var sensorID string
		err = tx.QueryRow(ctx, `UPDATE attachments SET ended_at=$2,version=version+1,updated_at=$2 WHERE id=$1 AND ended_at IS NULL RETURNING sensor_id`, item.id, closedAt).Scan(&sensorID)
		if err != nil {
			return Appliance{}, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO attachment_revisions(attachment_id,version,home_id,snapshot) SELECT a.id,a.version,a.home_id,to_jsonb(a) FROM attachments a WHERE a.id=$1`, item.id)
		if err != nil {
			return Appliance{}, err
		}
		_, err = tx.Exec(ctx, `UPDATE sensor_endpoints SET version=version+1 WHERE id=$1 AND home_id=$2`, sensorID, home)
		if err != nil {
			return Appliance{}, err
		}
	}
	a, err = scanAppliance(tx.QueryRow(ctx, `UPDATE appliances a SET archived_at=$3,version=version+1,updated_at=$3 WHERE a.id=$1 AND a.home_id=$2 AND a.archived_at IS NULL RETURNING a.id,a.home_id,a.name,a.category,a.room,a.identity,a.version,a.created_at,a.updated_at,a.archived_at`, id, home, closedAt))
	if err != nil {
		return Appliance{}, err
	}
	if err = snapshot(ctx, tx, a); err != nil {
		return Appliance{}, err
	}
	if err = saveReplay(ctx, tx, owner, home, op, key, hash, a); err != nil {
		return Appliance{}, err
	}
	return a, tx.Commit(ctx)
}
func validText(s string, max int) bool { return s != "" && s == strings.TrimSpace(s) && len(s) <= max }
func ValidIdentity(i Identity) bool {
	if !validText(i.Brand, 120) || !validText(i.FullModel, 240) {
		return false
	}
	if i.Variant != nil && !validText(*i.Variant, 120) || i.Region != nil && !validText(*i.Region, 120) {
		return false
	}
	switch i.Fuel {
	case "electricity", "natural_gas", "propane", "fuel_oil":
	default:
		return false
	}
	switch i.MatchQuality {
	case "confirmed_exact", "documented_variant_family", "unresolved":
	default:
		return false
	}
	if i.CatalogProductID != nil {
		if i.MatchQuality != "confirmed_exact" || !validUUID(*i.CatalogProductID) {
			return false
		}
	}
	return true
}
func ValidCreate(in CreateInput) bool {
	return validText(in.Name, 120) && validText(in.Category, 120) && (in.Room == nil || validText(*in.Room, 120)) && ValidIdentity(in.Identity)
}
func ValidPatch(in PatchInput) bool {
	if in.Name == nil && in.Category == nil && in.Room == nil && in.Identity == nil {
		return false
	}
	if in.Name != nil && !validText(*in.Name, 120) || in.Category != nil && !validText(*in.Category, 120) || in.Identity != nil && !ValidIdentity(*in.Identity) {
		return false
	}
	if in.Room != nil && !bytes.Equal(bytes.TrimSpace(in.Room), []byte("null")) {
		var room string
		if json.Unmarshal(in.Room, &room) != nil || !validText(room, 120) {
			return false
		}
	}
	return true
}
func validUUID(s string) bool { _, err := uuid.Parse(s); return err == nil && s == strings.ToLower(s) }
