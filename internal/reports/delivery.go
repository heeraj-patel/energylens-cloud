package reports

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrInvalidRecipients = errors.New("select 1 to 5 valid recipient addresses")
var ErrStalePreview = errors.New("report preview has changed or expired")
var ErrSnapshotMismatch = errors.New("approved snapshot digest does not match")
var ErrDeliveryRate = errors.New("manual report request limit reached")
var ErrApprovalRequired = errors.New("explicit report approval required")

var emailParts = regexp.MustCompile(`^[A-Za-z0-9_+%-]+(?:\.[A-Za-z0-9_+%-]+)*@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+$`)

func normalizeRecipients(raw []string) ([]string, error) {
	if len(raw) < 1 || len(raw) > 5 {
		return nil, ErrInvalidRecipients
	}
	out := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, address := range raw {
		if len(address) > 254 || !emailParts.MatchString(address) {
			return nil, ErrInvalidRecipients
		}
		parts := strings.Split(address, "@")
		if len(parts[0]) > 64 {
			return nil, ErrInvalidRecipients
		}
		for _, label := range strings.Split(parts[1], ".") {
			if len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
				return nil, ErrInvalidRecipients
			}
		}
		normalized := strings.ToLower(address)
		if seen[normalized] {
			return nil, ErrInvalidRecipients
		}
		seen[normalized] = true
		out = append(out, normalized)
	}
	sort.Strings(out)
	return out, nil
}

type DeliveryInput struct {
	PreviewID              string
	ApprovedSnapshotSHA256 string
	Recipients             []string
	Approved               bool
	IdempotencyKey         uuid.UUID
}

type DeliveryRequest struct {
	ID         string    `json:"id"`
	PreviewID  string    `json:"preview_id"`
	Recipients []string  `json:"recipients"`
	State      string    `json:"state"`
	CreatedAt  time.Time `json:"created_at"`
}

// QueueDelivery records an explicit approval; it never invokes a provider.
// Only the exact immutable preview digest may be approved. The home row lock
// serializes idempotency and the three-requests-per-owner-per-day rate bound.
func (s Store) QueueDelivery(ctx context.Context, owner, home string, in DeliveryInput, now time.Time) (DeliveryRequest, error) {
	if !in.Approved {
		return DeliveryRequest{}, ErrApprovalRequired
	}
	previewID, err := uuid.Parse(in.PreviewID)
	if err != nil || previewID == uuid.Nil || in.PreviewID != previewID.String() || in.IdempotencyKey == uuid.Nil {
		return DeliveryRequest{}, ErrNotFound
	}
	if len(in.ApprovedSnapshotSHA256) != 64 {
		return DeliveryRequest{}, ErrSnapshotMismatch
	}
	if _, err = hex.DecodeString(in.ApprovedSnapshotSHA256); err != nil || strings.ToLower(in.ApprovedSnapshotSHA256) != in.ApprovedSnapshotSHA256 {
		return DeliveryRequest{}, ErrSnapshotMismatch
	}
	recipients, err := normalizeRecipients(in.Recipients)
	if err != nil {
		return DeliveryRequest{}, err
	}
	recipientBody, _ := json.Marshal(recipients)
	recipientHash := sha256.Sum256(recipientBody)
	requestBody, _ := json.Marshal(struct {
		PreviewID  string
		Digest     string
		Recipients []string
	}{in.PreviewID, in.ApprovedSnapshotSHA256, recipients})
	requestHash := sha256.Sum256(requestBody)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return DeliveryRequest{}, err
	}
	defer tx.Rollback(ctx)
	// The rate bound spans all of an owner's homes. Lock the owner first so
	// concurrent requests for different homes cannot each observe count 2.
	var lockedOwner string
	err = tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, owner).Scan(&lockedOwner)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeliveryRequest{}, ErrNotFound
	}
	if err != nil {
		return DeliveryRequest{}, err
	}
	var homeVersion int64
	err = tx.QueryRow(ctx, `SELECT version FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL FOR UPDATE`, home, owner).Scan(&homeVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeliveryRequest{}, ErrNotFound
	}
	if err != nil {
		return DeliveryRequest{}, err
	}
	var prior DeliveryRequest
	var priorHash []byte
	err = tx.QueryRow(ctx, `SELECT id,preview_id,recipients,state,created_at,request_hash FROM report_delivery_requests
		WHERE owner_user_id=$1 AND home_id=$2 AND idempotency_key=$3`, owner, home, in.IdempotencyKey).Scan(&prior.ID, &prior.PreviewID, &prior.Recipients, &prior.State, &prior.CreatedAt, &priorHash)
	if err == nil {
		if !bytes.Equal(priorHash, requestHash[:]) {
			return DeliveryRequest{}, ErrKeyReuse
		}
		return prior, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return DeliveryRequest{}, err
	}
	var previewBody []byte
	var previewVersion int64
	var expires time.Time
	err = tx.QueryRow(ctx, `SELECT payload,home_version,expires_at FROM report_previews WHERE id=$1 AND owner_user_id=$2 AND home_id=$3`, previewID, owner, home).Scan(&previewBody, &previewVersion, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeliveryRequest{}, ErrNotFound
	}
	if err != nil {
		return DeliveryRequest{}, err
	}
	var preview Preview
	if err = json.Unmarshal(previewBody, &preview); err != nil {
		return DeliveryRequest{}, err
	}
	digest, err := snapshotDigest(preview)
	if err != nil {
		return DeliveryRequest{}, err
	}
	if digest != preview.SnapshotSHA256 || digest != in.ApprovedSnapshotSHA256 {
		return DeliveryRequest{}, ErrSnapshotMismatch
	}
	var stale bool
	err = tx.QueryRow(ctx, dependencyStaleSQL, previewID, home, previewVersion).Scan(&stale)
	if err != nil {
		return DeliveryRequest{}, err
	}
	if stale || previewVersion != homeVersion || !now.Before(expires) {
		return DeliveryRequest{}, ErrStalePreview
	}
	var duplicate DeliveryRequest
	err = tx.QueryRow(ctx, `SELECT id,preview_id,recipients,state,created_at FROM report_delivery_requests WHERE owner_user_id=$1 AND preview_id=$2 AND recipient_hash=$3`, owner, previewID, recipientHash[:]).Scan(&duplicate.ID, &duplicate.PreviewID, &duplicate.Recipients, &duplicate.State, &duplicate.CreatedAt)
	if err == nil {
		return duplicate, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return DeliveryRequest{}, err
	}
	var count int
	err = tx.QueryRow(ctx, `SELECT count(*) FROM report_delivery_requests WHERE owner_user_id=$1 AND created_at>$2`, owner, now.Add(-24*time.Hour)).Scan(&count)
	if err != nil {
		return DeliveryRequest{}, err
	}
	if count >= 3 {
		return DeliveryRequest{}, ErrDeliveryRate
	}
	request := DeliveryRequest{ID: uuid.NewString(), PreviewID: in.PreviewID, Recipients: recipients, State: "pending", CreatedAt: now.UTC()}
	_, err = tx.Exec(ctx, `INSERT INTO report_delivery_requests(id,owner_user_id,home_id,preview_id,idempotency_key,request_hash,recipient_hash,recipients,approved_snapshot_sha256,state,created_at,next_attempt_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'pending',$10,$10)`, request.ID, owner, home, previewID, in.IdempotencyKey, requestHash[:], recipientHash[:], recipients, digest, request.CreatedAt)
	if err != nil {
		return DeliveryRequest{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return DeliveryRequest{}, err
	}
	return request, nil
}
