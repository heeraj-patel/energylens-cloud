package programs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// storeSourceSnapshot records fetched bytes as an immutable, unreviewed
// artifact. Only ImportReviewed can create a customer-visible revision.
func storeSourceSnapshot(ctx context.Context, db *pgxpool.Pool, source RegisteredSource, fetched FetchedSource) error {
	if source.ID.String() == "00000000-0000-0000-0000-000000000000" || source.ParserVersion == "" ||
		len(fetched.Content) == 0 || int64(len(fetched.Content)) > source.MaxResponseBytes ||
		len(fetched.Content) > 4<<20 || strings.TrimSpace(fetched.ContentType) == "" {
		return errors.New("invalid source snapshot")
	}
	hash := sha256.Sum256(fetched.Content)
	digest := hex.EncodeToString(hash[:])
	tx, err := db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var snapshotID string
	err = tx.QueryRow(ctx, `INSERT INTO program_source_snapshots
		(source_id,source_url,content_sha256,content_type,content_bytes,fetched_at,parser_version,snapshot_ref)
		VALUES($1,$2,$3,$4,$5,now(),$6,$7) RETURNING id::text`, source.ID, source.CanonicalURL,
		digest, fetched.ContentType, len(fetched.Content), source.ParserVersion, "fetch:sha256:"+digest).Scan(&snapshotID)
	if err != nil {
		return fmt.Errorf("store source snapshot: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO program_source_snapshot_content(snapshot_id,content) VALUES($1,$2)`, snapshotID, fetched.Content); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
