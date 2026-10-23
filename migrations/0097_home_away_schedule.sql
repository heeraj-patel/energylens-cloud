-- Away schedule on home preferences: NULL means not set, so efficiency uses the
-- 09:00-16:00 home-local default. schema_version 2 marks rows written with it.
ALTER TABLE home_preferences
  ADD COLUMN away_schedule jsonb CHECK (away_schedule IS NULL OR CASE WHEN jsonb_typeof(away_schedule)='array' THEN jsonb_array_length(away_schedule) BETWEEN 1 AND 14 ELSE false END),
  DROP CONSTRAINT home_preferences_schema_version_check,
  ADD CONSTRAINT home_preferences_schema_version_check CHECK (schema_version IN (1,2));

ALTER TABLE home_preference_revisions
  ADD COLUMN away_schedule jsonb CHECK (away_schedule IS NULL OR CASE WHEN jsonb_typeof(away_schedule)='array' THEN jsonb_array_length(away_schedule) BETWEEN 1 AND 14 ELSE false END),
  DROP CONSTRAINT home_preference_revisions_schema_version_check,
  ADD CONSTRAINT home_preference_revisions_schema_version_check CHECK (schema_version IN (1,2));
