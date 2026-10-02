-- Private saved model identity requests. Never publish scan results as facts.
CREATE TABLE identity_scan_requests (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  home_id uuid NOT NULL REFERENCES homes(id) ON DELETE CASCADE,
  appliance_id uuid NOT NULL REFERENCES appliances(id) ON DELETE CASCADE,
  identity_key bytea NOT NULL CHECK (octet_length(identity_key)=32),
  category text NOT NULL CHECK (length(category) BETWEEN 1 AND 100),
  manufacturer text NOT NULL CHECK (length(manufacturer) BETWEEN 1 AND 100),
  model text NOT NULL CHECK (length(model) BETWEEN 1 AND 100),
  fuel text NOT NULL CHECK (fuel IN ('electricity','natural_gas','propane','fuel_oil')),
  variant text,
  region text,
  status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','running','review_pending','missing_source','reviewed_present','failed')),
  attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count BETWEEN 0 AND 5),
  next_attempt_at timestamptz,
  claimed_at timestamptz,
  candidate_report jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(home_id,identity_key),
  CHECK (variant IS NULL OR length(variant) BETWEEN 1 AND 100),
  CHECK (region IS NULL OR length(region) BETWEEN 1 AND 100),
  CHECK (candidate_report IS NULL OR octet_length(candidate_report::text) <= 262144)
);
CREATE INDEX identity_scan_requests_pending_idx ON identity_scan_requests(created_at,id) WHERE status='pending';
CREATE INDEX identity_scan_requests_retry_idx ON identity_scan_requests(next_attempt_at,id) WHERE status='missing_source';
