-- A single private research job may serve multiple saved appliances in a home.
-- Each link is tied to the appliance's current identity digest.
ALTER TABLE identity_scan_requests
  ADD COLUMN IF NOT EXISTS claim_count integer NOT NULL DEFAULT 0 CHECK (claim_count BETWEEN 0 AND 1000000);

CREATE TABLE identity_scan_subscriptions (
  appliance_id uuid PRIMARY KEY REFERENCES appliances(id) ON DELETE CASCADE,
  home_id uuid NOT NULL REFERENCES homes(id) ON DELETE CASCADE,
  request_id uuid NOT NULL REFERENCES identity_scan_requests(id) ON DELETE CASCADE,
  identity_key bytea NOT NULL CHECK (octet_length(identity_key)=32),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(request_id, appliance_id)
);
CREATE INDEX identity_scan_subscriptions_request_idx ON identity_scan_subscriptions(request_id);

-- Preserve the existing request's appliance and link all other currently saved
-- appliances in the same home whose exact identity fields match that request.
INSERT INTO identity_scan_subscriptions(appliance_id,home_id,request_id,identity_key)
SELECT a.id,a.home_id,q.id,q.identity_key
FROM identity_scan_requests q
JOIN appliances a ON a.home_id=q.home_id AND a.archived_at IS NULL
WHERE lower(a.category)=lower(q.category)
  AND lower(a.identity->>'brand')=lower(q.manufacturer)
  AND lower(a.identity->>'full_model')=lower(q.model)
  AND a.identity->>'fuel'=q.fuel
  AND lower(COALESCE(a.identity->>'variant',''))=lower(COALESCE(q.variant,''))
  AND lower(COALESCE(a.identity->>'region',''))=lower(COALESCE(q.region,''))
  AND a.identity->>'match_quality'='confirmed_exact'
ON CONFLICT(appliance_id) DO NOTHING;

-- Older request rows may predate the claim counter. Treat their attempt count
-- as already claimed work so a deployment never creates an unbounded retry.
UPDATE identity_scan_requests SET claim_count=attempt_count WHERE claim_count=0 AND attempt_count>0;
