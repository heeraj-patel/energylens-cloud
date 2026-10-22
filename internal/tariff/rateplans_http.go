package tariff

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

func (s Service) listPlans(w http.ResponseWriter, r *http.Request) {
	home, ok := homeID(w, r)
	if !ok {
		return
	}
	view, err := s.Plans.RatePlans(r.Context(), s.UserID(r), home, time.Now().UTC())
	if err != nil {
		storeError(w, err)
		return
	}
	respond(w, 200, view)
}

// readPut checks the Idempotency-Key and decodes one strict JSON object; the hash covers the operation and body.
func readPut(w http.ResponseWriter, r *http.Request, op string, into any) (string, []byte, bool) {
	key := r.Header.Get("Idempotency-Key")
	if _, err := uuid.Parse(key); err != nil {
		fail(w, 400, "invalid_request", "UUID Idempotency-Key required")
		return "", nil, false
	}
	if r.Header.Get("Content-Type") != "application/json" {
		fail(w, 415, "invalid_request", "application/json required")
		return "", nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil {
		fail(w, 400, "invalid_request", "invalid body")
		return "", nil, false
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	var extra any
	if err = d.Decode(into); err != nil || !errors.Is(d.Decode(&extra), io.EOF) {
		fail(w, 400, "invalid_request", "one JSON object required")
		return "", nil, false
	}
	hash := sha256.Sum256(append([]byte(op+"\n"), body...))
	return key, hash[:], true
}

func (s Service) putUtility(w http.ResponseWriter, r *http.Request) {
	home, ok := homeID(w, r)
	if !ok {
		return
	}
	var in struct {
		EIAID *string `json:"eiaid"`
	}
	key, hash, ok := readPut(w, r, "electric-utility", &in)
	if !ok {
		return
	}
	if in.EIAID == nil || !eiaidPattern.MatchString(*in.EIAID) {
		fail(w, 400, "invalid_request", "eiaid required")
		return
	}
	view, err := s.Plans.ChooseUtility(r.Context(), s.UserID(r), home, *in.EIAID, key, hash, time.Now().UTC())
	if err != nil {
		storeError(w, err)
		return
	}
	respond(w, 200, view)
}

func (s Service) putPlan(w http.ResponseWriter, r *http.Request) {
	home, ok := homeID(w, r)
	if !ok {
		return
	}
	var in struct {
		PlanID *string `json:"plan_id"`
	}
	key, hash, ok := readPut(w, r, "utility-rate-plan", &in)
	if !ok {
		return
	}
	if in.PlanID == nil {
		fail(w, 400, "invalid_request", "plan_id required")
		return
	}
	if _, err := uuid.Parse(*in.PlanID); err != nil {
		fail(w, 400, "invalid_request", "plan_id required")
		return
	}
	view, err := s.Plans.ChoosePlan(r.Context(), s.UserID(r), home, *in.PlanID, key, hash, time.Now().UTC())
	if err != nil {
		storeError(w, err)
		return
	}
	respond(w, 200, view)
}
