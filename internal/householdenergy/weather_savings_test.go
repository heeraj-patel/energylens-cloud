package householdenergy

import (
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
)

// weatherMeteredDevice seeds a home meter and a metered plug device using 1 kWh
// during the peak hour yesterday; periods adds a +0.20 peak row to the tariff.
func (h *gasHarness) weatherMeteredDevice(category, currency string, periods bool) string {
	h.t.Helper()
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		h.t.Fatal(err)
	}
	now := h.svc.Now()
	since := now.Add(-5 * 24 * time.Hour)
	hub, meter, plug := uuid.NewString(), uuid.NewString(), uuid.NewString()
	meterApp, deviceApp := uuid.NewString(), uuid.NewString()
	meterAtt, plugAtt, revision := uuid.NewString(), uuid.NewString(), uuid.NewString()
	h.exec(`INSERT INTO hubs(id,local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES($1,$2,$3,'test',$4,1)`, hub, "elhub_"+hub[:8], make([]byte, 32), h.home)
	h.exec(`INSERT INTO sensor_endpoints(id,home_id,hub_id,ownership_epoch,local_sensor_id,kind) VALUES($1,$2,$3,1,'main-meter','utility_meter')`, meter, h.home, hub)
	h.exec(`INSERT INTO sensor_endpoints(id,home_id,hub_id,ownership_epoch,local_sensor_id,kind,parent_sensor_id) VALUES($1,$2,$3,1,'device-plug','plug',$4)`, plug, h.home, hub, meter)
	for _, a := range [][]string{{meterApp, "Home", "other"}, {deviceApp, "Device", category}} {
		h.exec(`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,$3,$4,'{}')`, a[0], h.home, a[1], a[2])
		h.exec(`INSERT INTO appliance_revisions(appliance_id,home_id,version,snapshot) VALUES($1,$2,1,'{}')`, a[0], h.home)
	}
	h.exec(`INSERT INTO datawiz_energy_resolutions(appliance_ref,request_sha256,record,resolved_at) VALUES($1,$2,'{"appliance_version":1,"values":{"standby_watts":"1"},"source":{"tier":"model"}}',now())`, "cloud:"+deviceApp, make([]byte, 32))
	h.exec(`INSERT INTO attachments(id,home_id,sensor_id,appliance_id,identity_revision,started_at) VALUES($1,$2,$3,$4,1,$5),($6,$2,$7,$8,1,$5)`, meterAtt, h.home, meter, meterApp, since, plugAtt, plug, deviceApp)
	h.exec(`INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,1,'electricity',0.10,'kWh',$2,'user_tariff','bill',$3,false,$4)`, h.home, currency, since, since.Add(-24*time.Hour))
	if periods {
		if err = h.pool.QueryRow(h.ctx, `SELECT id::text FROM tariff_revisions WHERE home_id=$1`, h.home).Scan(&revision); err != nil {
			h.t.Fatal(err)
		}
		h.exec(`INSERT INTO tariff_revision_periods(tariff_revision_id,ordinal,name,adder,days,start_local,end_local,months) VALUES($1,1,'peak',0.20,$2,'16:00','21:00',$3::smallint[])`, revision, everyDay, allMonths)
	}
	observe := func(sensor, att string, wh int) {
		batch, event := uuid.NewString(), uuid.NewString()
		from := time.Date(2026, 10, 20, 17, 0, 0, 0, chicago)
		h.exec(`INSERT INTO ingest_batches(hub_id,ownership_epoch,batch_id,home_id,request_hash) VALUES($1,1,$2,$3,$4)`, hub, batch, h.home, make([]byte, 32))
		h.exec(`INSERT INTO ingest_events(hub_id,ownership_epoch,event_id,revision,home_id,sensor_id,event_kind,payload_hash,first_batch_id) VALUES($1,1,$2,1,$3,$4,'energy_interval',$5,$6)`, hub, event, h.home, sensor, make([]byte, 32), batch)
		h.exec(`INSERT INTO energy_observations(hub_id,ownership_epoch,event_id,revision,kind,start_at,end_at,energy_wh,covered_seconds,attachment_revision_id,fuel) VALUES($1,1,$2,1,'energy_interval',$3,$4,$5,3600,$6,'electricity')`, hub, event, from, from.Add(time.Hour), wh, att)
	}
	observe(meter, meterAtt, 2000)
	observe(plug, plugAtt, 1000)
	return deviceApp
}

func TestWeatherDeviceSavingsMatchesMeasuredPeakShift(t *testing.T) {
	h := newGasHarness(t, nil)
	pump := h.weatherMeteredDevice("pool_pump", "USD", true)
	unmetered := h.appliance("pool_pump", "electricity", "Unwired", nil)
	got, err := h.svc.WeatherDeviceSavings(h.ctx, h.owner, h.home)
	if err != nil {
		t.Fatal(err)
	}
	// 1 kWh moves from the +0.20 adder to the day's lowest adder.
	device := got.Devices[pump]
	if !device.Metered || device.MonthlyShiftSaving == nil || device.MonthlyShiftSaving.Cmp(big.NewRat(1, 5)) != 0 || got.ShiftCurrency != "USD" {
		t.Fatalf("pump %+v currency %q", device, got.ShiftCurrency)
	}
	eff, err := h.svc.TrailingEfficiency(h.ctx, h.owner, h.home)
	if err != nil || eff == nil {
		t.Fatalf("trailing efficiency %+v, %v", eff, err)
	}
	for _, d := range eff.Devices {
		if d.ApplianceID == pump && (d.Peak == nil || d.Peak.Amount != "0.200000") {
			t.Fatalf("peak %+v", d.Peak)
		}
	}
	if d := got.Devices[unmetered]; d.Metered || d.MonthlyShiftSaving != nil {
		t.Fatalf("unmetered %+v", d)
	}
	c, err := h.svc.compute(h.ctx, h.owner, h.home, "day", "trailing_30_days")
	if err != nil {
		t.Fatal(err)
	}
	cost := c.totals.Cost.(money)
	want, _ := new(big.Rat).SetString(cost.Amount)
	if got.ElectricMonthly == nil || got.ElectricMonthly.Cmp(want) != 0 || got.ElectricCurrency != "USD" || want.Sign() <= 0 {
		t.Fatalf("electric monthly %v want %v currency %q", got.ElectricMonthly, want, got.ElectricCurrency)
	}
	if got.GasMonthly != nil {
		t.Fatalf("gas monthly %v for a home without gas", got.GasMonthly)
	}
}

func TestWeatherDeviceSavingsFlatTariffHasNoShiftSaving(t *testing.T) {
	h := newGasHarness(t, nil)
	pump := h.weatherMeteredDevice("pool_pump", "USD", false)
	got, err := h.svc.WeatherDeviceSavings(h.ctx, h.owner, h.home)
	if err != nil {
		t.Fatal(err)
	}
	if d := got.Devices[pump]; !d.Metered || d.MonthlyShiftSaving != nil {
		t.Fatalf("flat tariff %+v", d)
	}
	if got.ElectricMonthly == nil || got.ElectricMonthly.Sign() <= 0 {
		t.Fatalf("electric monthly %v", got.ElectricMonthly)
	}
}

func TestWeatherDeviceSavingsCanadianTariffIsNotUSD(t *testing.T) {
	h := newGasHarness(t, nil)
	pump := h.weatherMeteredDevice("pool_pump", "CAD", true)
	got, err := h.svc.WeatherDeviceSavings(h.ctx, h.owner, h.home)
	if err != nil {
		t.Fatal(err)
	}
	if got.ShiftCurrency != "CAD" || got.ElectricCurrency != "CAD" || got.Devices[pump].MonthlyShiftSaving == nil {
		t.Fatalf("CAD savings %+v", got)
	}
}

func TestWeatherDeviceSavingsGasCostMatchesTheDashboard(t *testing.T) {
	h := newGasHarness(t, fixedGasRate("static", 1.481))
	p := h.product("dryer", "natural_gas", "DVG45T6000V", map[[2]string]float64{{"combined_energy_factor", "lb/kWh"}: 3.3}, dryerCEF)
	h.appliance("dryer", "natural_gas", "DVG45T6000V", &p)
	got, err := h.svc.WeatherDeviceSavings(h.ctx, h.owner, h.home)
	if err != nil {
		t.Fatal(err)
	}
	// 22.698 therms a year at $1.481 over 30 days.
	if got.GasMonthly == nil || got.GasCurrency != "USD" {
		t.Fatalf("gas monthly %v %q", got.GasMonthly, got.GasCurrency)
	}
	gas, _ := got.GasMonthly.Float64()
	near(t, "gas monthly", gas, 22.698/365*30*1.481, 0.01)
}
