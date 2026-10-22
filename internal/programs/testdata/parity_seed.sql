-- Shared by the Go runner and Datawiz parity checks; the OEDI CSVs in this folder supply ZIP 92503.
DO $$
DECLARE
  u uuid; h uuid; a uuid; j uuid; n integer := 0; spec record;
BEGIN
  INSERT INTO users DEFAULT VALUES RETURNING id INTO u;
  INSERT INTO homes(owner_user_id,name,timezone) VALUES (u,'Home','America/Chicago') RETURNING id INTO h;
  INSERT INTO appliances(home_id,name,category,identity) VALUES (h,'Washer','washer','{}') RETURNING id INTO a;
  FOR spec IN SELECT * FROM (VALUES
    ('parity-utility-gas','utility','utility','gas_distributor','["92503","78732"]','[]',true,interval '1 hour'),
    ('parity-state','government','state','','["92503"]','["washer"]',true,interval '1 hour'),
    ('parity-maker','manufacturer','manufacturer','','["92503","90210"]','["washer","dryer"]',true,interval '1 hour'),
    ('parity-fail','utility','utility','electric_distributor','["92503"]','["washer"]',true,interval '1 hour'),
    ('parity-disabled','government','county','','["92503"]','[]',false,interval '1 hour'),
    ('parity-expired','government','city','','["92503"]','["dryer"]',true,interval '-1 hour')
  ) AS t(key,kind,scope,role,postals,cats,enabled,expires) LOOP
    INSERT INTO program_sources(source_key,source_kind,authority_name,canonical_url,allowed_hosts,access_method,terms_url,terms_reviewed_at,terms_review_expires_at,terms_reviewed_by,parser_version,canonicalization_version,geography_coverage,category_coverage,refresh_interval,max_requests_per_run,max_response_bytes,enabled)
    VALUES (spec.key,spec.kind,spec.key,'https://official.example/terms',ARRAY['official.example'],'html','https://official.example/terms',now()-interval '1 day',now()+spec.expires,'reviewer','v1','v1',
      jsonb_build_object('country_code','US','postal_codes',spec.postals::jsonb,'scope_kind',spec.scope,'provider_role',spec.role),spec.cats::jsonb,interval '7 days',1,100000,spec.enabled);
  END LOOP;
  FOR spec IN SELECT * FROM (VALUES ('US','92503','washer'),('US','92503','dryer'),('US','90210','washer'),('US','78732','washer'),('CA','M5V2T6','washer')) AS t(country,postal,category) LOOP
    n := n + 1;
    INSERT INTO program_discovery_refresh_jobs(country_code,postal_code,category,created_at) VALUES (spec.country,spec.postal,spec.category,now()-interval '1 hour'+n*interval '1 second') RETURNING id INTO j;
    INSERT INTO program_discovery_refresh_requests(owner_user_id,home_id,appliance_id,location_revision,country_code,postal_code,category,idempotency_key,job_id)
    VALUES (u,h,a,1,spec.country,spec.postal,spec.category,gen_random_uuid(),j);
  END LOOP;
  INSERT INTO program_discovery_refresh_jobs(country_code,postal_code,category) VALUES ('US','10001','washer');
  INSERT INTO program_discovery_refresh_jobs(country_code,postal_code,category,state,started_at) VALUES ('US','60601','washer','running',now()-interval '10 minutes');
  INSERT INTO program_discovery_refresh_jobs(country_code,postal_code,category,state,finished_at) VALUES ('US','60602','washer','failed',now()-interval '40 days');
  FOR spec IN SELECT * FROM (VALUES
    ('jurisdiction','county:travis','{"name":"Travis County","geography_type":"county"}'),
    ('jurisdiction','place:austin','{"name":"Austin","geography_type":"place"}'),
    ('jurisdiction','state:tx','{"name":"Texas","geography_type":"state"}'),
    ('provider_candidate','eia:1234','{"name":"Grid One","provider_id":"1234","service_type":"electric"}'),
    ('provider_candidate','gas:atmos','{"name":"Atmos Gas","service_type":"gas"}'),
    ('provider_candidate','gas:nameless','{"name":""}')
  ) AS t(field,record_key,value) LOOP
    INSERT INTO datawiz_runs(id,route,request,mode,status) VALUES (gen_random_uuid(),'zip','{}','realtime','complete') RETURNING id INTO j;
    INSERT INTO datawiz_observations(id,run_id,route,subject_key,field,record_key,value,source,approval_status,collected_at)
    VALUES (j,j,'zip','zip:78732',spec.field,spec.record_key,spec.value::jsonb,'{"adapter":"test"}','approved',now());
    INSERT INTO datawiz_current(route,subject_key,field,record_key,observation_id) VALUES ('zip','zip:78732',spec.field,spec.record_key,j);
  END LOOP;
END $$;
