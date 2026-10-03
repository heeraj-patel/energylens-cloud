package questions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("home or appliance not found")
var ErrVersion = errors.New("appliance identity revision changed")
var ErrInvalid = errors.New("invalid question answer")
var ErrKeyReuse = errors.New("idempotency key reused with different answers")
var ErrDisclosureRequired = errors.New("history disclosure acceptance required")

type AnswerCreate struct {
	QuestionID       string          `json:"question_id"`
	QuestionVersion  int64           `json:"question_version"`
	ApplianceID      string          `json:"appliance_id"`
	IdentityRevision int64           `json:"identity_revision"`
	Status           string          `json:"status"`
	Value            json.RawMessage `json:"value"`
	ConfirmedAt      *string         `json:"confirmed_at"`
}
type Answer struct {
	AnswerCreate
	ID         string    `json:"id"`
	HomeID     string    `json:"home_id"`
	Revision   int64     `json:"revision"`
	RecordedAt time.Time `json:"recorded_at"`
}
type Store interface {
	List(context.Context, string, string, string, *string) ([]Question, error)
	CurrentAnswers(context.Context, string, string, string) ([]Answer, error)
	Submit(context.Context, string, string, []AnswerCreate, string, []byte) ([]Answer, error)
}
type PGStore struct{ Pool *pgxpool.Pool }

// CurrentAnswers returns only answers for the appliance's current revision.
// Prior answers remain immutable evidence, but cannot silently prefill a
// changed appliance identity.
func (s PGStore) CurrentAnswers(ctx context.Context, owner, home, appliance string) ([]Answer, error) {
	var version int64
	err := s.Pool.QueryRow(ctx, `SELECT a.version FROM appliances a JOIN homes h ON h.id=a.home_id WHERE a.id=$1 AND a.home_id=$2 AND a.archived_at IS NULL AND h.owner_user_id=$3 AND h.deleted_at IS NULL`, appliance, home, owner).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT ON (question_id) id,question_id,question_version,revision,status,answer_value,confirmed_at,recorded_at FROM question_answers WHERE home_id=$1 AND appliance_id=$2 AND appliance_version=$3 ORDER BY question_id,revision DESC`, home, appliance, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Answer{}
	for rows.Next() {
		a := Answer{AnswerCreate: AnswerCreate{ApplianceID: appliance, IdentityRevision: version}, HomeID: home}
		var value []byte
		var confirmed *time.Time
		if err := rows.Scan(&a.ID, &a.QuestionID, &a.QuestionVersion, &a.Revision, &a.Status, &value, &confirmed, &a.RecordedAt); err != nil {
			return nil, err
		}
		a.Value = json.RawMessage(value)
		if len(a.Value) == 0 {
			a.Value = json.RawMessage("null")
		}
		if confirmed != nil {
			formatted := confirmed.UTC().Format(time.RFC3339)
			a.ConfirmedAt = &formatted
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s PGStore) List(ctx context.Context, owner, home, purpose string, appliance *string) ([]Question, error) {
	var id string
	err := s.Pool.QueryRow(ctx, `SELECT id FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL`, home, owner).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT id,category,version FROM appliances WHERE home_id=$1 AND archived_at IS NULL AND ($2::uuid IS NULL OR id=$2) ORDER BY created_at,id`, home, appliance)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := []Question{}
	type applianceCandidate struct {
		id, category string
		version      int64
	}
	var cycleCandidates []applianceCandidate
	found := false
	for rows.Next() {
		var id, category string
		var version int64
		if err = rows.Scan(&id, &category, &version); err != nil {
			return nil, err
		}
		found = true
		if q, ok := questionFor(category, id, version, purpose); ok {
			candidates = append(candidates, q)
		}
		if purpose == "onboarding" {
			cycleCandidates = append(cycleCandidates, applianceCandidate{id, category, version})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if appliance != nil && !found {
		return nil, ErrNotFound
	}
	for _, item := range cycleCandidates {
		q, err := reviewedCycleQuestion(ctx, s.Pool, home, item.id, item.category, item.version)
		if err != nil {
			return nil, err
		}
		if q != nil {
			candidates = append(candidates, *q)
		}
	}
	out := []Question{}
	for _, q := range candidates {
		// Context flows advance past answers for this exact appliance identity
		// and question definition. Unknown is a saved answer too.
		if purpose != "onboarding" {
			var answered bool
			err = s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM question_answers WHERE home_id=$1 AND appliance_id=$2 AND appliance_version=$3 AND question_id=$4 AND question_version=$5)`, home, q.ApplianceID, q.IdentityRevision, q.ID, q.Version).Scan(&answered)
			if err != nil {
				return nil, err
			}
			if answered {
				continue
			}
		}
		out = append(out, q)
	}
	return out, nil
}

func validAnswer(in AnswerCreate, q Question, now time.Time) bool {
	if in.QuestionID != q.ID || in.QuestionVersion != q.Version || in.IdentityRevision != q.IdentityRevision || in.ApplianceID != q.ApplianceID {
		return false
	}
	if in.Status == "unknown" {
		return bytes.Equal(bytes.TrimSpace(in.Value), []byte("null")) && in.ConfirmedAt == nil
	}
	if in.Status != "confirmed" || in.ConfirmedAt == nil {
		return false
	}
	confirmed, err := time.Parse(time.RFC3339, *in.ConfirmedAt)
	if err != nil || confirmed.After(now.Add(5*time.Minute)) {
		return false
	}
	var raw json.Number
	d := json.NewDecoder(bytes.NewReader(in.Value))
	d.UseNumber()
	if err = d.Decode(&raw); err != nil {
		return false
	}
	if q.AnswerKind == "cycles_per_week" {
		v, err := strconv.ParseInt(raw.String(), 10, 64)
		return err == nil && v >= 0 && v <= 100
	}
	if q.AnswerKind == "hours_per_week" {
		v, err := strconv.ParseFloat(raw.String(), 64)
		return err == nil && v >= 0 && v <= 168
	}
	return false
}

func (s PGStore) Submit(ctx context.Context, owner, home string, inputs []AnswerCreate, key string, hash []byte) ([]Answer, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL FOR UPDATE`, home, owner).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var accepted bool
	err = tx.QueryRow(ctx, `SELECT history_disclosure_accepted_at IS NOT NULL FROM account_preferences WHERE user_id=$1`, owner).Scan(&accepted)
	if err != nil {
		return nil, err
	}
	if !accepted {
		return nil, ErrDisclosureRequired
	}
	var oldHash, oldBody []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,response_json FROM question_answer_idempotency WHERE owner_user_id=$1 AND home_id=$2 AND key=$3`, owner, home, key).Scan(&oldHash, &oldBody)
	if err == nil {
		if !bytes.Equal(oldHash, hash) {
			return nil, ErrKeyReuse
		}
		var old []Answer
		if err = json.Unmarshal(oldBody, &old); err != nil {
			return nil, err
		}
		return old, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]Answer, 0, len(inputs))
	now := time.Now().UTC()
	for _, in := range inputs {
		if _, err = uuid.Parse(in.ApplianceID); err != nil {
			return nil, ErrInvalid
		}
		dup := in.ApplianceID + "/" + in.QuestionID
		if seen[dup] {
			return nil, ErrInvalid
		}
		seen[dup] = true
		var category string
		var version int64
		err = tx.QueryRow(ctx, `SELECT category,version FROM appliances WHERE id=$1 AND home_id=$2 AND archived_at IS NULL FOR UPDATE`, in.ApplianceID, home).Scan(&category, &version)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if version != in.IdentityRevision {
			return nil, ErrVersion
		}
		q, ok := questionByID(category, in.ApplianceID, version, in.QuestionID)
		if !ok {
			cycle, cycleErr := reviewedCycleQuestion(ctx, tx, home, in.ApplianceID, category, version)
			if cycleErr != nil {
				return nil, cycleErr
			}
			if cycle != nil && cycle.ID == in.QuestionID {
				q, ok = *cycle, true
			}
		}
		if !ok || !validAnswer(in, q, now) {
			return nil, ErrInvalid
		}
		var revision int64
		if err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(revision),0)+1 FROM question_answers WHERE home_id=$1 AND appliance_id=$2 AND question_id=$3`, home, in.ApplianceID, in.QuestionID).Scan(&revision); err != nil {
			return nil, err
		}
		var a Answer
		a.AnswerCreate = in
		a.HomeID = home
		a.Revision = revision
		var value any
		if in.Status == "confirmed" {
			value = in.Value
		}
		err = tx.QueryRow(ctx, `INSERT INTO question_answers(home_id,appliance_id,appliance_version,question_id,question_version,revision,status,answer_value,confirmed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9) RETURNING id,recorded_at`, home, in.ApplianceID, in.IdentityRevision, in.QuestionID, in.QuestionVersion, revision, in.Status, value, in.ConfirmedAt).Scan(&a.ID, &a.RecordedAt)
		if err != nil {
			return nil, fmt.Errorf("insert answer: %w", err)
		}
		out = append(out, a)
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO question_answer_idempotency(owner_user_id,home_id,key,request_hash,response_json) VALUES($1,$2,$3,$4,$5)`, owner, home, key, hash, body)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}
