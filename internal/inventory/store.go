package inventory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"energylens.local/cloud/internal/catalog"
	"energylens.local/cloud/internal/energycategories"
	"energylens.local/cloud/internal/householdenergy"
	"energylens.local/cloud/internal/programs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("appliance or home not found")
var ErrVersion = errors.New("appliance version conflict")
var ErrKeyReuse = errors.New("appliance idempotency key reused")
var ErrCatalogMismatch = errors.New("catalog product not found")
var ErrAttachmentConflict = errors.New("active attachment cannot safely close")
var ErrNearbyOriginInvalid = errors.New("nearby origin is not owned by this home")

type NearbyOriginConflict struct{ ApplianceID string }

func (e NearbyOriginConflict) Error() string {
	return "nearby device is already linked to an appliance"
}

type NearbyOrigin struct {
	HubID            string `json:"hub_id"`
	OwnershipEpoch   int64  `json:"ownership_epoch"`
	ProtocolDeviceID string `json:"protocol_device_id"`
}

type Identity struct {
	Brand            string     `json:"brand"`
	FullModel        string     `json:"full_model"`
	Variant          *string    `json:"variant,omitempty"`
	Region           *string    `json:"region,omitempty"`
	Fuel             string     `json:"fuel"`
	MatchQuality     string     `json:"match_quality"`
	CatalogProductID *string    `json:"catalog_product_id,omitempty"`
	FCCID            *string    `json:"fcc_id,omitempty"`
	ICID             *string    `json:"ic_id,omitempty"`
	SerialNumber     *string    `json:"serial_number,omitempty"`
	UPC              *string    `json:"upc,omitempty"`
	BuildYear        *int       `json:"build_year,omitempty"`
	BulbLabel        *BulbLabel `json:"bulb_label,omitempty"`
}
type BulbLabel struct {
	LoadKind           string   `json:"load_kind"`
	BulbCount          *int     `json:"bulb_count,omitempty"`
	CombinedInputWatts *float64 `json:"combined_input_watts,omitempty"`
}
type Appliance struct {
	ID                  string        `json:"id"`
	DemoDeviceID        string        `json:"demo_device_id,omitempty"`
	DemoFixtureRevision string        `json:"-"`
	HomeID              string        `json:"home_id"`
	Name                string        `json:"name"`
	Category            string        `json:"category"`
	Room                *string       `json:"room,omitempty"`
	Identity            Identity      `json:"identity"`
	Version             int64         `json:"version"`
	CreatedAt           time.Time     `json:"created_at,omitempty"`
	UpdatedAt           time.Time     `json:"updated_at,omitempty"`
	ArchivedAt          *time.Time    `json:"archived_at"`
	NearbyOrigin        *NearbyOrigin `json:"nearby_origin,omitempty"`
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

type CreateInput struct {
	Name         string        `json:"name"`
	Category     string        `json:"category"`
	Room         *string       `json:"room,omitempty"`
	Identity     Identity      `json:"identity"`
	NearbyOrigin *NearbyOrigin `json:"nearby_origin,omitempty"`
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

// AnswerApplies reports whether a saved usage answer still applies to an appliance
// of the given raw and canonical category; Patch carries no answer without it.
type PGStore struct {
	Pool          *pgxpool.Pool
	AnswerApplies func(category, canonical, questionID string) bool
}

const selectAppliance = `SELECT a.id,a.home_id,a.name,a.category,a.room,a.identity,a.version,a.created_at,a.updated_at,a.archived_at,a.nearby_hub_id,a.nearby_ownership_epoch,a.nearby_protocol_device_id FROM appliances a`

type row interface{ Scan(...any) error }

func scanAppliance(r row) (Appliance, error) {
	var a Appliance
	var identity []byte
	var hubID, protocolDeviceID *string
	var epoch *int64
	err := r.Scan(&a.ID, &a.HomeID, &a.Name, &a.Category, &a.Room, &identity, &a.Version, &a.CreatedAt, &a.UpdatedAt, &a.ArchivedAt, &hubID, &epoch, &protocolDeviceID)
	if err != nil {
		return a, err
	}
	if hubID != nil && epoch != nil && protocolDeviceID != nil {
		a.NearbyOrigin = &NearbyOrigin{HubID: *hubID, OwnershipEpoch: *epoch, ProtocolDeviceID: *protocolDeviceID}
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
	err = tx.QueryRow(ctx, `SELECT h.demo_cost_snapshot
 FROM homes h WHERE h.id=$1 AND h.owner_user_id=$2 AND h.deleted_at IS NULL`, home, owner).Scan(&rawDemo)
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
	}
	if err = rows.Err(); err != nil {
		return nil, "", err
	}
	rows.Close()
	for _, a := range all {
		if a.ArchivedAt == nil {
			out = append(out, a)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, "", err
	}
	if len(rawDemo) > 0 && string(rawDemo) != "null" {
		snapshot, err := householdenergy.DecodeDemoCostSnapshot(bytes.NewReader(rawDemo))
		if err != nil {
			return nil, "", err
		}
		demoRows, err := DemoAppliances(snapshot, home)
		if err != nil {
			return nil, "", err
		}
		out = append(out, demoRows...)
		all = append(all, demoRows...)
		fingerprint := sha256.Sum256(rawDemo)
		revision := inventoryRevision("persisted+demo:"+hex.EncodeToString(fingerprint[:]), home, append(all, demoRows...))
		return out, revision, nil
	}
	revision := inventoryRevision("persisted", home, all)
	return out, revision, nil
}

// DemoAppliances derives the home-scoped public inventory IDs for named demo leaves.
// Reports use the same derivation when validating a selection.
func DemoAppliances(snapshot householdenergy.DemoCostSnapshot, home string) ([]Appliance, error) {
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

func demoAppliances(snapshot householdenergy.DemoCostSnapshot, home string) ([]Appliance, error) {
	return DemoAppliances(snapshot, home)
}
func (s PGStore) Get(ctx context.Context, owner, home, id string) (Appliance, error) {
	a, err := scanAppliance(s.Pool.QueryRow(ctx, selectAppliance+` JOIN homes h ON h.id=a.home_id WHERE a.id=$1 AND a.home_id=$2 AND h.owner_user_id=$3 AND h.deleted_at IS NULL AND a.archived_at IS NULL`, id, home, owner))
	if errors.Is(err, pgx.ErrNoRows) {
		return Appliance{}, ErrNotFound
	}
	return a, err
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
	var found string
	err := tx.QueryRow(ctx, `SELECT id FROM catalog_products WHERE id=$1 AND fuel=$2`, *id, a.Identity.Fuel).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCatalogMismatch
	}
	if err != nil {
		return err
	}
	return nil
}

// linkCatalog sets the catalog link for an identity that has none: the single
// exact product, else the single documented family product, else a prefix-trim
// family product. Only products of the identity's own fuel are considered.
func linkCatalog(ctx context.Context, tx pgx.Tx, a *Appliance) error {
	i := &a.Identity
	if i.CatalogProductID != nil || strings.TrimSpace(i.Brand) == "" || strings.TrimSpace(i.FullModel) == "" {
		return nil
	}
	category, err := energycategories.Ensure(ctx, tx, a.Category)
	if err != nil {
		return err
	}
	link, found, err := catalog.FindLink(ctx, tx, catalog.Search{Brand: i.Brand, Model: i.FullModel, Region: i.Region, Variant: i.Variant, Fuel: i.Fuel, Category: category})
	if err != nil || !found {
		return err
	}
	i.CatalogProductID, i.MatchQuality = &link.ProductID, link.MatchQuality
	return nil
}

// linkIdentity validates a picked catalog product, then links an unlinked
// identity. Create and Patch share it so an edited model is a new device entry.
func linkIdentity(ctx context.Context, tx pgx.Tx, a *Appliance) error {
	if err := checkCatalog(ctx, tx, *a); err != nil {
		return err
	}
	return linkCatalog(ctx, tx, a)
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
func (s PGStore) Create(ctx context.Context, owner, home string, in CreateInput, key string, hash []byte) (Appliance, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Appliance{}, err
	}
	defer tx.Rollback(ctx)
	if in.NearbyOrigin != nil {
		// Ownership mutations lock the user first. Attachment writes lock the
		// home before the hub; use the same order below.
		var userID string
		err = tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, owner).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return Appliance{}, ErrNearbyOriginInvalid
		}
		if err != nil {
			return Appliance{}, err
		}
	}
	if err = s.ownedHome(ctx, tx, owner, home); err != nil {
		return Appliance{}, err
	}
	if in.NearbyOrigin != nil {
		var currentHome string
		var currentEpoch int64
		err = tx.QueryRow(ctx, `SELECT home_id,ownership_epoch FROM hubs WHERE id=$1 AND home_id=$2 AND revoked_at IS NULL FOR UPDATE`, in.NearbyOrigin.HubID, home).Scan(&currentHome, &currentEpoch)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && (currentHome != home || currentEpoch != in.NearbyOrigin.OwnershipEpoch) {
			return Appliance{}, ErrNearbyOriginInvalid
		}
		if err != nil {
			return Appliance{}, err
		}
	}
	if a, found, e := replay(ctx, tx, owner, home, "create_appliance", key, hash); found || e != nil {
		return a, e
	}
	if in.NearbyOrigin != nil {
		var existing string
		err = tx.QueryRow(ctx, `SELECT id FROM appliances WHERE home_id=$1 AND nearby_hub_id=$2 AND nearby_ownership_epoch=$3 AND nearby_protocol_device_id=$4 AND archived_at IS NULL`, home, in.NearbyOrigin.HubID, in.NearbyOrigin.OwnershipEpoch, in.NearbyOrigin.ProtocolDeviceID).Scan(&existing)
		if err == nil {
			return Appliance{}, NearbyOriginConflict{ApplianceID: existing}
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Appliance{}, err
		}
	}
	a := Appliance{HomeID: home, Name: in.Name, Category: in.Category, Room: in.Room, Identity: in.Identity, Version: 1, NearbyOrigin: in.NearbyOrigin}
	if _, err = energycategories.Ensure(ctx, tx, a.Category); err != nil {
		return Appliance{}, err
	}
	if err = linkIdentity(ctx, tx, &a); err != nil {
		return Appliance{}, err
	}
	identity, _ := json.Marshal(a.Identity)
	var nearbyHub any
	var nearbyEpoch any
	var nearbyDevice any
	if in.NearbyOrigin != nil {
		nearbyHub, nearbyEpoch, nearbyDevice = in.NearbyOrigin.HubID, in.NearbyOrigin.OwnershipEpoch, in.NearbyOrigin.ProtocolDeviceID
	}
	err = tx.QueryRow(ctx, `INSERT INTO appliances(home_id,name,category,room,catalog_product_id,identity,nearby_hub_id,nearby_ownership_epoch,nearby_protocol_device_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id,created_at,updated_at`, home, a.Name, a.Category, a.Room, a.Identity.CatalogProductID, identity, nearbyHub, nearbyEpoch, nearbyDevice).Scan(&a.ID, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return Appliance{}, err
	}
	// The owned home is locked above. Set the milestone only after a real row
	// exists, in the same transaction as the appliance and its replay record.
	if _, err = tx.Exec(ctx, `UPDATE homes SET first_device_added_at=$2
		WHERE id=$1 AND first_device_added_at IS NULL`, home, a.CreatedAt); err != nil {
		return Appliance{}, err
	}
	if err = snapshot(ctx, tx, a); err != nil {
		return Appliance{}, err
	}
	if err = saveReplay(ctx, tx, owner, home, "create_appliance", key, hash, a); err != nil {
		return Appliance{}, err
	}
	queueProgramRefresh(ctx, tx, owner, home, a.ID, "create:"+key)
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
	previousCategory, previousFuel := a.Category, a.Identity.Fuel
	if in.Name != nil {
		a.Name = *in.Name
	}
	if in.Category != nil {
		a.Category = *in.Category
	}
	canonical, err := energycategories.Ensure(ctx, tx, a.Category)
	if err != nil {
		return Appliance{}, err
	}
	if in.Identity != nil {
		previous := a.Identity
		a.Identity = *in.Identity
		modelEdited := a.Identity.Brand != previous.Brand || a.Identity.FullModel != previous.FullModel
		if modelEdited && sameOptional(a.Identity.CatalogProductID, previous.CatalogProductID) {
			a.Identity.CatalogProductID = nil
		}
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
	if err = linkIdentity(ctx, tx, &a); err != nil {
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
	if err = carryAnswers(ctx, tx, a, func(question string) bool {
		return s.AnswerApplies != nil && s.AnswerApplies(a.Category, canonical, question)
	}); err != nil {
		return Appliance{}, err
	}
	if err = saveReplay(ctx, tx, owner, home, op, key, hash, a); err != nil {
		return Appliance{}, err
	}
	if a.Category != previousCategory || a.Identity.Fuel != previousFuel {
		queueProgramRefresh(ctx, tx, owner, home, a.ID, "patch:"+key)
	}
	return a, tx.Commit(ctx)
}

// queueProgramRefresh queues program discovery for the appliance's ZIP and
// category. It is best effort in a savepoint, so a missing location or queue
// error never fails the appliance write.
func queueProgramRefresh(ctx context.Context, tx pgx.Tx, owner, home, appliance, seed string) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		slog.Warn("program refresh savepoint failed", "error", err)
		return
	}
	key := uuid.NewSHA1(uuid.NameSpaceOID, []byte("program-refresh:"+seed)).String()
	if _, err = programs.EnqueueInTx(ctx, sp, owner, home, appliance, key); err != nil {
		slog.Warn("program refresh enqueue failed", "error", err)
		_ = sp.Rollback(ctx)
		return
	}
	_ = sp.Commit(ctx)
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
	a, err = scanAppliance(tx.QueryRow(ctx, `UPDATE appliances a SET archived_at=$3,version=version+1,updated_at=$3 WHERE a.id=$1 AND a.home_id=$2 AND a.archived_at IS NULL RETURNING a.id,a.home_id,a.name,a.category,a.room,a.identity,a.version,a.created_at,a.updated_at,a.archived_at,a.nearby_hub_id,a.nearby_ownership_epoch,a.nearby_protocol_device_id`, id, home, closedAt))
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
	if i.Brand != "" && !validText(i.Brand, 120) || i.FullModel != "" && !validText(i.FullModel, 240) {
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
	if i.MatchQuality != "unresolved" && (i.Brand == "" || i.FullModel == "") {
		return false
	}
	if i.CatalogProductID != nil {
		if !validUUID(*i.CatalogProductID) {
			return false
		}
	}
	for _, v := range []*string{i.FCCID, i.ICID, i.SerialNumber, i.UPC} {
		if v != nil && !validText(*v, 120) {
			return false
		}
	}
	if i.BuildYear != nil && (*i.BuildYear < 1900 || *i.BuildYear > 2100) {
		return false
	}
	if label := i.BulbLabel; label != nil {
		if label.LoadKind != "replaceable_bulb" && label.LoadKind != "integrated_lamp" {
			return false
		}
		if label.LoadKind == "integrated_lamp" {
			if label.BulbCount != nil || label.CombinedInputWatts != nil {
				return false
			}
		} else {
			if label.BulbCount == nil || *label.BulbCount < 1 || *label.BulbCount > 8 || label.CombinedInputWatts == nil || *label.CombinedInputWatts < 0.1 || *label.CombinedInputWatts > 2000 {
				return false
			}
		}
	}
	return true
}
func ValidCreate(in CreateInput) bool {
	return validText(in.Name, 120) && validText(in.Category, 120) && (in.Room == nil || validText(*in.Room, 120)) && ValidIdentity(in.Identity) && (in.NearbyOrigin == nil || validUUID(in.NearbyOrigin.HubID) && in.NearbyOrigin.OwnershipEpoch > 0 && validText(in.NearbyOrigin.ProtocolDeviceID, 256))
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

// RelinkResult counts the outcome of one RelinkCatalog pass.
type RelinkResult struct{ LinkedExact, LinkedFamily, Unresolved int }

// RelinkCatalog links active appliances of any fuel that have no catalog link,
// writing each change like a Patch: version bump and snapshot.
func (s PGStore) RelinkCatalog(ctx context.Context) (RelinkResult, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id FROM appliances WHERE catalog_product_id IS NULL AND archived_at IS NULL AND btrim(identity->>'brand')<>'' AND btrim(identity->>'full_model')<>'' ORDER BY created_at,id`)
	if err != nil {
		return RelinkResult{}, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return RelinkResult{}, err
	}
	var out RelinkResult
	for _, id := range ids {
		quality, err := s.relinkOne(ctx, id)
		if err != nil {
			return out, err
		}
		switch quality {
		case "confirmed_exact":
			out.LinkedExact++
		case "documented_variant_family":
			out.LinkedFamily++
		default:
			out.Unresolved++
		}
	}
	return out, nil
}

func (s PGStore) relinkOne(ctx context.Context, id string) (string, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	a, err := scanAppliance(tx.QueryRow(ctx, selectAppliance+` WHERE a.id=$1 AND a.archived_at IS NULL AND a.catalog_product_id IS NULL FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if err = linkCatalog(ctx, tx, &a); err != nil {
		return "", err
	}
	if a.Identity.CatalogProductID == nil {
		return "", nil
	}
	identity, _ := json.Marshal(a.Identity)
	err = tx.QueryRow(ctx, `UPDATE appliances SET catalog_product_id=$2,identity=$3,version=version+1,updated_at=now() WHERE id=$1 RETURNING version,updated_at`, id, a.Identity.CatalogProductID, identity).Scan(&a.Version, &a.UpdatedAt)
	if err != nil {
		return "", err
	}
	if err = snapshot(ctx, tx, a); err != nil {
		return "", err
	}
	// Brand, model and category are unchanged, so every answer carries over.
	if err = carryAnswers(ctx, tx, a, nil); err != nil {
		return "", err
	}
	return a.Identity.MatchQuality, tx.Commit(ctx)
}

// carryAnswers copies each question's latest answer from the previous appliance
// version to the current one. A nil keep carries every question.
func carryAnswers(ctx context.Context, tx pgx.Tx, a Appliance, keep func(questionID string) bool) error {
	rows, err := tx.Query(ctx, `SELECT DISTINCT question_id FROM question_answers WHERE home_id=$1 AND appliance_id=$2 AND appliance_version=$3`, a.HomeID, a.ID, a.Version-1)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		if keep == nil || keep(id) {
			ids = append(ids, id)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil || len(ids) == 0 {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO question_answers(home_id,appliance_id,appliance_version,question_id,question_version,revision,status,answer_value,confirmed_at)
	 SELECT p.home_id,p.appliance_id,$3::bigint,p.question_id,p.question_version,
	  (SELECT max(m.revision) FROM question_answers m WHERE m.home_id=p.home_id AND m.appliance_id=p.appliance_id AND m.question_id=p.question_id)+1,
	  p.status,p.answer_value,p.confirmed_at
	 FROM (SELECT DISTINCT ON (question_id) * FROM question_answers WHERE home_id=$1 AND appliance_id=$2 AND appliance_version=$4 AND question_id=ANY($5) ORDER BY question_id,revision DESC) p`, a.HomeID, a.ID, a.Version, a.Version-1, ids)
	return err
}
