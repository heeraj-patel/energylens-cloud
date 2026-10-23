package account

import (
	"embed"
	"encoding/csv"
	"io"
	"sync"
)

//go:embed data/us_zcta_timezones_2026.csv
var zipTimezoneData embed.FS

var zipTimezoneIndex = sync.OnceValues(func() (map[string]string, error) {
	file, err := zipTimezoneData.Open("data/us_zcta_timezones_2026.csv")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := csv.NewReader(file)
	header, err := reader.Read()
	if err != nil || len(header) != 2 || header[0] != "zip" || header[1] != "timezone" {
		return nil, io.ErrUnexpectedEOF
	}
	index := make(map[string]string, 33000)
	for {
		row, err := reader.Read()
		if err == io.EOF {
			return index, nil
		}
		if err != nil || len(row) != 2 || len(row[0]) != 5 || row[1] == "" {
			return nil, io.ErrUnexpectedEOF
		}
		index[row[0]] = row[1]
	}
})

// resolveUSZIPTimeZone returns only a supported, bounded ZCTA assignment.
// ZCTAs approximate USPS ZIPs; missing or ambiguous entries are unresolved.
func resolveUSZIPTimeZone(postal string) (string, bool) {
	if !validUSZIPPostal(postal) {
		return "", false
	}
	if len(postal) == 10 && postal[5] == '-' {
		postal = postal[:5]
	}
	index, err := zipTimezoneIndex()
	if err != nil {
		return "", false
	}
	zone, ok := index[postal]
	return zone, ok
}

func validUSZIPPostal(postal string) bool {
	if len(postal) != 5 && (len(postal) != 10 || postal[5] != '-') {
		return false
	}
	for i := range postal {
		if i == 5 {
			continue
		}
		if postal[i] < '0' || postal[i] > '9' {
			return false
		}
	}
	return true
}
