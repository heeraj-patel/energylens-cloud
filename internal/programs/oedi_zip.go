package programs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"energylens.local/cloud/internal/datawizfacts"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const oediDatasetURL = "https://data.openei.org/submissions/8563"

var oediZIPPattern = regexp.MustCompile(`^[0-9]{5}$`)
var oediIDPattern = regexp.MustCompile(`^[0-9]+$`)
var oediStatePattern = regexp.MustCompile(`^[A-Z]{2}$`)
var oediHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type OEDIZIPResolver struct {
	DB                      *pgxpool.Pool
	IOUPath, NonIOUPath     string
	IOUSHA256, NonIOUSHA256 string // operator-recorded full-file digests
	RetrievedAt             time.Time
}

type oediFile struct {
	name, path, digest, kind string
	maxBytes                 int64
}

type oediCandidate struct {
	id, state, name, ownership string
	services                   map[string]bool
}

func (r OEDIZIPResolver) ResolveZIP(ctx context.Context, country, zip string) ([]Scope, error) {
	result, err := r.ResolveZIPDetailed(ctx, country, zip)
	return result.Scopes, err
}

// ResolveZIPDetailed reads only operator-supplied full CSVs. OEDI assigns
// likely ZIPs; every returned scope is an unverified electric-service lead.
func (r OEDIZIPResolver) ResolveZIPDetailed(ctx context.Context, country, zip string) (ZIPResolution, error) {
	if country != "US" || !oediZIPPattern.MatchString(zip) {
		return ZIPResolution{}, errors.New("OEDI resolver requires US five-digit ZIP")
	}
	if r.DB != nil {
		facts, err := (datawizfacts.Store{DB: r.DB}).Current(ctx, "zip", "zip:"+zip)
		if err != nil {
			return ZIPResolution{}, err
		}
		if len(facts) > 0 {
			resolution := datawizZIPResolution(facts)
			if len(resolution.Scopes) > 0 {
				return resolution, nil
			}
		}
	}
	if r.RetrievedAt.IsZero() || r.RetrievedAt.Location() != time.UTC || r.RetrievedAt.After(time.Now().UTC()) {
		return ZIPResolution{}, errors.New("OEDI retrieval time must be a past UTC timestamp")
	}
	files := []oediFile{
		{"iou_zipcodes_2024.csv", r.IOUPath, r.IOUSHA256, "iou", 7_000_000},
		{"non_iou_zipcodes_2024.csv", r.NonIOUPath, r.NonIOUSHA256, "non_iou", 4_000_000},
	}
	candidates := map[string]*oediCandidate{}
	for _, file := range files {
		if err := readOEDICSV(ctx, file, zip, candidates); err != nil {
			return ZIPResolution{}, err
		}
	}
	keys := make([]string, 0, len(candidates))
	for key := range candidates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	scopes := make([]Scope, 0, len(keys))
	for _, key := range keys {
		candidate := candidates[key]
		scopes = append(scopes, Scope{Kind: "utility", Key: "eia:" + key, Name: candidate.name,
			ProviderRole: "electric_service_candidate"})
	}
	return ZIPResolution{
		Scopes: scopes,
		Provenance: map[string]any{
			"dataset_url":         oediDatasetURL,
			"source_data_year":    2024,
			"source_published_on": "2025-11-10",
			"retrieved_at":        r.RetrievedAt.Format(time.RFC3339Nano),
			"source_sha256":       map[string]string{files[0].name: files[0].digest, files[1].name: files[1].digest},
			"source_urls": map[string]string{
				files[0].name: "https://data.openei.org/files/8563/" + files[0].name,
				files[1].name: "https://data.openei.org/files/8563/" + files[1].name,
			},
			"candidate_count": len(scopes),
		},
		Limitations: []string{"electricity_only", "gas_unresolved", "provider_unconfirmed", "other_jurisdictions_unresolved", "likely_zip_assignments_not_address_coverage"},
	}, nil
}

func datawizZIPResolution(facts []datawizfacts.Fact) ZIPResolution {
	result := ZIPResolution{Scopes: []Scope{}, Provenance: map[string]any{"datawiz_current": true}, Limitations: []string{"provider_unconfirmed", "zcta_intersection_not_address_coverage"}}
	seen := map[string]bool{}
	for _, fact := range facts {
		var value struct {
			Name          string `json:"name"`
			GeographyType string `json:"geography_type"`
			State         string `json:"state"`
			ProviderID    string `json:"provider_id"`
			ServiceType   string `json:"service_type"`
		}
		if json.Unmarshal(fact.Value, &value) != nil || value.Name == "" {
			continue
		}
		scope := Scope{Name: value.Name}
		switch fact.Field {
		case "jurisdiction":
			switch value.GeographyType {
			case "county":
				scope.Kind = "county"
			case "place":
				scope.Kind = "city"
			default:
				continue
			}
			scope.Key = fact.RecordKey
		case "provider_candidate":
			scope.Kind = "utility"
			if value.ProviderID != "" {
				scope.Key = "eia:" + value.ProviderID
				scope.ProviderRole = "electric_service_candidate"
			} else {
				scope.Key = fact.RecordKey
				scope.ProviderRole = "gas_distributor"
			}
		default:
			continue
		}
		key := scope.Kind + "\x00" + scope.Key + "\x00" + scope.ProviderRole
		if !seen[key] {
			seen[key] = true
			result.Scopes = append(result.Scopes, scope)
		}
	}
	return result
}

func readOEDICSV(ctx context.Context, file oediFile, zip string, candidates map[string]*oediCandidate) error {
	if !oediHashPattern.MatchString(file.digest) || filepath.Base(file.path) != file.name {
		return fmt.Errorf("%s requires exact filename and SHA-256", file.kind)
	}
	info, err := os.Lstat(file.path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > file.maxBytes {
		return fmt.Errorf("%s CSV is linked, empty, or oversized", file.kind)
	}
	stream, err := os.Open(file.path)
	if err != nil {
		return err
	}
	defer stream.Close()
	body, err := io.ReadAll(io.LimitReader(stream, file.maxBytes+1))
	if err != nil {
		return err
	}
	if len(body) == 0 || int64(len(body)) > file.maxBytes || !utf8.Valid(body) {
		return fmt.Errorf("%s CSV exceeds byte limit or is not UTF-8", file.kind)
	}
	actual := sha256.Sum256(body)
	if hex.EncodeToString(actual[:]) != file.digest {
		return fmt.Errorf("%s CSV SHA-256 mismatch", file.kind)
	}
	reader := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(body, []byte{0xef, 0xbb, 0xbf})))
	reader.FieldsPerRecord = 9
	header, err := reader.Read()
	if err != nil || strings.Join(header, ",") != "zip,eiaid,utility_name,state,service_type,ownership,comm_rate,ind_rate,res_rate" {
		return fmt.Errorf("%s CSV columns differ from 2024 source", file.kind)
	}
	for rowCount := 0; ; {
		if err := ctx.Err(); err != nil {
			return err
		}
		row, readErr := reader.Read()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("%s CSV malformed: %w", file.kind, readErr)
		}
		rowCount++
		if rowCount > 100_000 {
			return fmt.Errorf("%s CSV exceeds row limit", file.kind)
		}
		if row[0] != zip {
			continue
		}
		if !oediZIPPattern.MatchString(row[0]) || !oediIDPattern.MatchString(row[1]) || !oediStatePattern.MatchString(row[3]) ||
			len(strings.TrimSpace(row[2])) < 1 || len(row[2]) > 160 || containsControl(row[2]) ||
			(row[4] != "Bundled" && row[4] != "Delivery" && row[4] != "Energy") ||
			len(strings.TrimSpace(row[5])) < 1 || len(row[5]) > 80 ||
			((file.kind == "iou") != (row[5] == "Investor Owned")) {
			return fmt.Errorf("%s CSV matched row is invalid", file.kind)
		}
		key := row[1] + ":" + row[3]
		if existing := candidates[key]; existing != nil {
			if existing.name != strings.TrimSpace(row[2]) || existing.ownership != strings.TrimSpace(row[5]) {
				return errors.New("OEDI EIA identity conflicts across rows")
			}
			existing.services[row[4]] = true
		} else {
			candidates[key] = &oediCandidate{id: row[1], state: row[3], name: strings.TrimSpace(row[2]), ownership: strings.TrimSpace(row[5]), services: map[string]bool{row[4]: true}}
			if len(candidates) > 25 {
				return errors.New("OEDI ZIP exceeds candidate bound")
			}
		}
	}
	return nil
}

func containsControl(value string) bool {
	for _, char := range value {
		if char < 32 || char == 127 {
			return true
		}
	}
	return false
}
