-- Mean residential natural gas price per region (state code or 'US') over the latest 12 EIA months.
-- Datawiz load-energy-rates writes it with energy_rate_cache; Cloud only reads both.
CREATE TABLE gas_rate_cache (
  region             text PRIMARY KEY CHECK (region ~ '^[A-Z]{2}$'),
  rate_usd_per_therm numeric NOT NULL CHECK (rate_usd_per_therm > 0),
  period             date NOT NULL,
  fetched_at         timestamptz NOT NULL DEFAULT now()
);
