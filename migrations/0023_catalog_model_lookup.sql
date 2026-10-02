-- Exact, case-insensitive reviewed model lookup; no prefix/fuzzy match.
CREATE INDEX catalog_products_model_lookup_idx
  ON catalog_products(lower(brand),lower(full_model),fuel,region,id);
