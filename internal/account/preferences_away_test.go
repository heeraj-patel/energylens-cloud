package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type awayStore struct {
	preferenceRouteStore
	got HomePreferencesPatch
}

func (s *awayStore) PatchHomePreferences(_ context.Context, _, _ string, in HomePreferencesPatch, _ int64, _ string, _ []byte) (HomePreferences, error) {
	s.calls++
	s.got = in
	return HomePreferences{Version: 2, SchemaVersion: 2, Goal: "unspecified", ComfortPriority: "unknown", AwaySchedule: in.AwaySchedule.Blocks}, nil
}

func awayPatch(t *testing.T, body string) (*awayStore, *httptest.ResponseRecorder) {
	t.Helper()
	store := &awayStore{preferenceRouteStore: preferenceRouteStore{home: uuid.NewString()}}
	router := chi.NewRouter()
	router.Route("/v1", func(r chi.Router) {
		(Service{Store: routeStore{}, Verifier: fixedVerifier{}, Preferences: store}).Routes(r)
	})
	req := httptest.NewRequest(http.MethodPatch, "/v1/homes/"+store.home+"/preferences", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer owner")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", uuid.NewString())
	req.Header.Set("If-Match", `"1"`)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return store, w
}

func TestHomePreferencesPatchAwaySchedule(t *testing.T) {
	store, w := awayPatch(t, `{"away_schedule":[{"days":["mon","tue"],"start_local":"22:00","end_local":"06:00"},{"days":["sat"],"start_local":"00:00","end_local":"24:00"}]}`)
	if w.Code != 200 || w.Header().Get("ETag") != `"2"` {
		t.Fatalf("away patch = %d %s", w.Code, w.Body.String())
	}
	if !store.got.AwaySchedule.Set || len(store.got.AwaySchedule.Blocks) != 2 || store.got.AwaySchedule.Blocks[0].StartLocal != "22:00" {
		t.Fatalf("patch reached store as %+v", store.got)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out["schema_version"] == nil || string(out["schema_version"]) != "2" || out["away_schedule"] == nil {
		t.Fatalf("response = %s", w.Body.String())
	}
}

func TestHomePreferencesPatchAwayScheduleNullClears(t *testing.T) {
	store, w := awayPatch(t, `{"away_schedule":null}`)
	if w.Code != 200 || !store.got.AwaySchedule.Set || store.got.AwaySchedule.Blocks != nil {
		t.Fatalf("null patch = %d set=%v blocks=%v", w.Code, store.got.AwaySchedule.Set, store.got.AwaySchedule.Blocks)
	}
	if !strings.Contains(w.Body.String(), `"away_schedule":null`) {
		t.Fatalf("cleared schedule not null in response: %s", w.Body.String())
	}
}

func TestHomePreferencesPatchAwayScheduleRejectsInvalid(t *testing.T) {
	block := func(days, start, end string) string {
		return `{"away_schedule":[{"days":` + days + `,"start_local":"` + start + `","end_local":"` + end + `"}]}`
	}
	many := strings.Repeat(`{"days":["mon"],"start_local":"09:00","end_local":"10:00"},`, 14) + `{"days":["mon"],"start_local":"09:00","end_local":"10:00"}`
	for name, body := range map[string]string{
		"empty list":     `{"away_schedule":[]}`,
		"equal times":    block(`["mon"]`, "09:00", "09:00"),
		"no days":        block(`[]`, "09:00", "16:00"),
		"duplicate day":  block(`["mon","mon"]`, "09:00", "16:00"),
		"bad day":        block(`["monday"]`, "09:00", "16:00"),
		"bad start":      block(`["mon"]`, "9:00", "16:00"),
		"24:00 start":    block(`["mon"]`, "24:00", "16:00"),
		"bad minutes":    block(`["mon"]`, "09:60", "16:00"),
		"unknown field":  `{"away_schedule":[{"days":["mon"],"start_local":"09:00","end_local":"16:00","x":1}]}`,
		"not an array":   `{"away_schedule":"09:00-16:00"}`,
		"too many":       `{"away_schedule":[` + many + `]}`,
		"null goal":      `{"goal":null,"away_schedule":null}`,
		"unknown top":    `{"away_schedule":null,"other":1}`,
		"missing fields": `{"away_schedule":[{"days":["mon"]}]}`,
	} {
		store, w := awayPatch(t, body)
		if w.Code != 400 || store.calls != 0 {
			t.Errorf("%s: code %d calls %d body %s", name, w.Code, store.calls, w.Body.String())
		}
	}
}

func TestHomePreferencesJSONSchemaVersion(t *testing.T) {
	v1, _ := json.Marshal(HomePreferences{Version: 1, SchemaVersion: 1, Goal: "unspecified", ComfortPriority: "unknown"})
	if strings.Contains(string(v1), "away_schedule") {
		t.Fatalf("schema_version 1 must omit away_schedule: %s", v1)
	}
	v2, _ := json.Marshal(HomePreferences{Version: 1, SchemaVersion: 2, Goal: "unspecified", ComfortPriority: "unknown"})
	if !strings.Contains(string(v2), `"away_schedule":null`) {
		t.Fatalf("schema_version 2 must carry away_schedule: %s", v2)
	}
}
