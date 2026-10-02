package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"time"

	"energylens.local/cloud/internal/tariff"
	"github.com/jackc/pgx/v5/pgxpool"
)

func importOfficialTariff(ctx context.Context, pool *pgxpool.Pool, fs *flag.FlagSet, args []string) (tariff.Tariff, error) {
	artifactPath := fs.String("artifact", "", "reviewed one-home official tariff JSON")
	snapshotPath := fs.String("source-snapshot", "", "local official source snapshot")
	actor := fs.String("imported-by", "", "administrator identifier")
	if err := fs.Parse(args); err != nil {
		return tariff.Tariff{}, err
	}
	if fs.NArg() != 0 || *artifactPath == "" || *snapshotPath == "" || strings.TrimSpace(*actor) == "" {
		return tariff.Tariff{}, errors.New("artifact, source-snapshot and imported-by required")
	}
	artifact, err := readBoundedTariffFile(*artifactPath, 16<<10)
	if err != nil {
		return tariff.Tariff{}, err
	}
	snapshot, err := readBoundedTariffFile(*snapshotPath, 2<<20)
	if err != nil {
		return tariff.Tariff{}, err
	}
	d := json.NewDecoder(bytes.NewReader(artifact))
	d.DisallowUnknownFields()
	var in tariff.OfficialImport
	if err = d.Decode(&in); err != nil {
		return tariff.Tariff{}, err
	}
	var extra any
	if err = d.Decode(&extra); !errors.Is(err, io.EOF) {
		return tariff.Tariff{}, errors.New("one tariff JSON object required")
	}
	return tariff.ImportOfficial(ctx, pool, in, snapshot, *actor, time.Now().UTC())
}

func readBoundedTariffFile(path string, maximum int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maximum+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || int64(len(data)) > maximum {
		return nil, errors.New("tariff artifact or source snapshot is empty or too large")
	}
	return data, nil
}
