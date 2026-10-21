-- Catalog products keep their real fuel, so gas dryer and water heater identity rows are not stored as electric.
-- The values are the fuel names tariffs and appliance identities already use.
ALTER TABLE catalog_products DROP CONSTRAINT catalog_products_fuel_check;
ALTER TABLE catalog_products ADD CONSTRAINT catalog_products_fuel_check
  CHECK (fuel IN ('electricity','natural_gas','propane','fuel_oil'));
