-- One OEDI utility-by-ZIP edition, one row per CSV data row; loaded by Datawiz, never seeded here.
CREATE TABLE oedi_utility_zips (
  edition_year smallint NOT NULL CHECK (edition_year BETWEEN 2011 AND 2100),
  file_kind text NOT NULL CHECK (file_kind IN ('iou','non_iou')),
  source_row integer NOT NULL CHECK (source_row > 0),
  zip text NOT NULL CHECK (zip ~ '^[0-9]{5}$'),
  eiaid text NOT NULL CHECK (eiaid ~ '^[0-9]+$'),
  utility_name text NOT NULL CHECK (length(btrim(utility_name)) BETWEEN 1 AND 160),
  state text NOT NULL CHECK (state ~ '^[A-Z]{2}$'),
  service_type text NOT NULL CHECK (service_type IN ('Bundled','Delivery','Energy')),
  ownership text NOT NULL CHECK (length(btrim(ownership)) BETWEEN 1 AND 80),
  comm_rate numeric CHECK (comm_rate >= 0),
  ind_rate numeric CHECK (ind_rate >= 0),
  res_rate numeric CHECK (res_rate >= 0),
  dataset_url text NOT NULL,
  published_on timestamptz NOT NULL,
  source_url text NOT NULL,
  source_sha256 text NOT NULL CHECK (source_sha256 ~ '^[0-9a-f]{64}$'),
  retrieved_at timestamptz NOT NULL,
  loaded_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (edition_year, file_kind, source_row),
  CHECK ((file_kind = 'iou') = (ownership = 'Investor Owned'))
);
CREATE INDEX oedi_utility_zips_zip_idx ON oedi_utility_zips(zip);
