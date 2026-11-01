package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // UK local time must work on machines without a zoneinfo database

	"github.com/JamesWoolfenden/foxess-mcp/internal/foxess"
)

// API is the part of the FoxESS client the service needs.
type API interface {
	DeviceList(ctx context.Context) ([]foxess.Device, error)
	DeviceDetail(ctx context.Context, sn string) (*foxess.DeviceDetail, error)
	RealQuery(ctx context.Context, sn string, variables []string) ([]foxess.RealPoint, error)
	ReportQuery(ctx context.Context, sn, dimension string, year, month int, variables []string) ([]foxess.ReportSeries, error)
}

const (
	maxDayRange   = 366
	maxMonthRange = 36

	maxRealtimeFallback = 40
	closedTTL           = 24 * time.Hour // a finished day or month no longer changes
	openTTL             = 5 * time.Minute
	deviceTTL           = 10 * time.Minute
)

var london = func() *time.Location {
	l, err := time.LoadLocation("Europe/London")
	if err != nil {
		return time.UTC
	}
	return l
}()

// FoxESS report variable names.
const (
	vPV        = "PVEnergyTotal"
	vGen       = "generation"
	vFeedIn    = "feedin"
	vGrid      = "gridConsumption"
	vCharge    = "chargeEnergyToTal"
	vDischarge = "dischargeEnergyToTal"
	vLoads     = "loads"
)

var reportVars = []string{vPV, vGen, vFeedIn, vGrid, vCharge, vDischarge, vLoads}

var realtimeVars = []string{"pvPower", "loadsPower", "generationPower", "feedinPower", "gridConsumptionPower", "batChargePower", "batDischargePower", "SoC"}

type cacheEntry struct {
	series []foxess.ReportSeries
	expiry time.Time
}

// Service holds the tool logic. Everything it returns is read-only data.
type Service struct {
	api API
	sn  string // optional pinned device serial
	now func() time.Time

	mu      sync.Mutex
	cache   map[string]cacheEntry
	devices []foxess.Device
	devExp  time.Time
}

// NewService builds a service. pinnedSN may be empty, in which case the device
// is discovered from the account.
func NewService(api API, pinnedSN string) *Service {
	return &Service{api: api, sn: pinnedSN, now: time.Now, cache: map[string]cacheEntry{}}
}

// mask hides all but the last four characters of an identifier.
func mask(s string) string {
	if len(s) <= 4 {
		return strings.Repeat("*", len(s))
	}
	return strings.Repeat("*", len(s)-4) + s[len(s)-4:]
}

func round(v float64, places int) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0 // encoding/json refuses NaN and Inf
	}
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

func ptr(v float64) *float64 { r := round(v, 3); return &r }

// -------------------------------------------------------------- devices

func (s *Service) deviceList(ctx context.Context) ([]foxess.Device, error) {
	s.mu.Lock()
	if s.devices != nil && s.now().Before(s.devExp) {
		d := s.devices
		s.mu.Unlock()
		return d, nil
	}
	s.mu.Unlock()
	d, err := s.api.DeviceList(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.devices, s.devExp = d, s.now().Add(deviceTTL)
	s.mu.Unlock()
	return d, nil
}

// resolveDevice finds the serial to query. want may be a full serial or its
// masked form; with one device on the account it can be empty.
func (s *Service) resolveDevice(ctx context.Context, want string) (string, error) {
	if want == "" {
		want = s.sn
	}
	devs, err := s.deviceList(ctx)
	if err != nil {
		return "", err
	}
	if len(devs) == 0 {
		return "", errors.New("no devices found on this FoxESS account")
	}
	if want == "" {
		if len(devs) == 1 {
			return devs[0].SN, nil
		}
		var m []string
		for _, d := range devs {
			m = append(m, mask(d.SN))
		}
		return "", fmt.Errorf("this account has %d devices (%s); pass one as 'device', or set FOXESS_DEVICE_SN", len(devs), strings.Join(m, ", "))
	}
	for _, d := range devs {
		if d.SN == want || mask(d.SN) == want {
			return d.SN, nil
		}
	}
	return "", fmt.Errorf("device %q not found on this account", mask(want))
}

type DeviceOut struct {
	Device     string  `json:"device_masked"`
	Status     string  `json:"status"`
	Model      string  `json:"model,omitempty"`
	HasPV      bool    `json:"has_pv"`
	HasBattery bool    `json:"has_battery"`
	Batteries  int     `json:"batteries"`
	CapacityKW float64 `json:"inverter_capacity_kw,omitempty"`
}

type PlantsOut struct {
	Devices []DeviceOut `json:"devices"`
	Notes   []string    `json:"notes"`
}

func statusName(code int) string {
	switch code {
	case 1:
		return "online"
	case 2:
		return "fault"
	case 3:
		return "offline"
	}
	return fmt.Sprintf("unknown(%d)", code)
}

// GetPlants lists the inverters. Station names are left out: people often name
// a plant after their address.
func (s *Service) GetPlants(ctx context.Context) (*PlantsOut, error) {
	devs, err := s.deviceList(ctx)
	if err != nil {
		return nil, err
	}
	out := &PlantsOut{Devices: []DeviceOut{}, Notes: []string{"Serial numbers are masked, and the station name is omitted because it is often an address."}}
	for _, d := range devs {
		do := DeviceOut{Device: mask(d.SN), Status: statusName(d.Status), Model: d.DeviceType, HasPV: d.HasPV, HasBattery: d.HasBattery}
		if det, err := s.api.DeviceDetail(ctx, d.SN); err == nil {
			do.CapacityKW = round(det.Capacity, 2)
			do.Batteries = len(det.BatteryList)
		} else {
			out.Notes = append(out.Notes, fmt.Sprintf("Could not read detail for %s: %v", mask(d.SN), err))
		}
		out.Devices = append(out.Devices, do)
	}
	return out, nil
}

// ------------------------------------------------------------- realtime

type ReadingOut struct {
	Variable string `json:"variable"`
	Name     string `json:"name,omitempty"`
	Unit     string `json:"unit,omitempty"`
	Value    any    `json:"value"`
}

type RealtimeOut struct {
	Device   string       `json:"device_masked"`
	Status   string       `json:"status"` // ok, no_data
	Time     string       `json:"reading_time,omitempty"`
	Readings []ReadingOut `json:"readings"`
	Notes    []string     `json:"notes"`
}

func (s *Service) GetRealtime(ctx context.Context, device string) (*RealtimeOut, error) {
	sn, err := s.resolveDevice(ctx, device)
	if err != nil {
		return nil, err
	}
	// Ask for everything and pick out what we want: requesting variables by
	// name returned nothing on a live account, so the names can't be trusted.
	all, err := s.api.RealQuery(ctx, sn, nil)
	if err != nil {
		return nil, err
	}
	out := &RealtimeOut{Device: mask(sn), Readings: []ReadingOut{}, Notes: []string{}}
	if len(all) == 0 {
		out.Status = "no_data"
		out.Notes = append(out.Notes, "FoxESS returned no live readings. The inverter may be offline; this is missing data, not zero output.")
		return out, nil
	}
	want := map[string]bool{}
	for _, v := range realtimeVars {
		want[v] = true
	}
	var pts []foxess.RealPoint
	for _, p := range all {
		if want[p.Variable] {
			pts = append(pts, p)
		}
	}
	if len(pts) == 0 {
		// None of the expected names matched: show what FoxESS did send so the
		// names can be corrected, rather than hiding live data.
		pts = all
		if len(pts) > maxRealtimeFallback {
			pts = pts[:maxRealtimeFallback]
		}
		out.Notes = append(out.Notes, "None of the expected variable names were present, so this shows the first readings FoxESS returned.")
	}
	out.Status = "ok"
	for _, p := range pts {
		out.Readings = append(out.Readings, ReadingOut{Variable: p.Variable, Name: p.Name, Unit: p.Unit, Value: p.Value})
		if out.Time == "" {
			out.Time = p.Time
		}
	}
	return out, nil
}

// --------------------------------------------------------- energy report

// Metrics are energy totals in kWh. A nil field means FoxESS did not return
// that variable, which is different from zero.
type Metrics struct {
	PVKWh               *float64 `json:"pv_kwh"`
	ExportKWh           *float64 `json:"export_kwh"`
	ImportKWh           *float64 `json:"grid_import_kwh"`
	BatteryChargeKWh    *float64 `json:"battery_charge_kwh"`
	BatteryDischargeKWh *float64 `json:"battery_discharge_kwh"`
	LoadKWh             *float64 `json:"load_kwh"`
	InverterOutputKWh   *float64 `json:"inverter_output_kwh"`
	// Derived (see notes): every kWh exported is counted as solar, so this is
	// pv minus export, and it is left out when that would be negative.
	PVSelfConsumedKWh  *float64 `json:"pv_self_consumed_kwh,omitempty"`
	SelfSufficiencyPct *float64 `json:"self_sufficiency_pct,omitempty"`
}

func addPtr(dst **float64, src *float64) {
	if src == nil {
		return
	}
	if *dst == nil {
		z := 0.0
		*dst = &z
	}
	**dst += *src
}

func (m *Metrics) add(o Metrics) {
	addPtr(&m.PVKWh, o.PVKWh)
	addPtr(&m.ExportKWh, o.ExportKWh)
	addPtr(&m.ImportKWh, o.ImportKWh)
	addPtr(&m.BatteryChargeKWh, o.BatteryChargeKWh)
	addPtr(&m.BatteryDischargeKWh, o.BatteryDischargeKWh)
	addPtr(&m.LoadKWh, o.LoadKWh)
	addPtr(&m.InverterOutputKWh, o.InverterOutputKWh)
}

// finish rounds the base fields and fills in the derived ones.
func (m *Metrics) finish() {
	for _, p := range []**float64{&m.PVKWh, &m.ExportKWh, &m.ImportKWh, &m.BatteryChargeKWh, &m.BatteryDischargeKWh, &m.LoadKWh, &m.InverterOutputKWh} {
		if *p != nil {
			*p = ptr(**p)
		}
	}
	if m.PVKWh != nil && m.ExportKWh != nil && *m.PVKWh >= *m.ExportKWh {
		m.PVSelfConsumedKWh = ptr(*m.PVKWh - *m.ExportKWh)
	}
	if m.LoadKWh != nil && m.ImportKWh != nil && *m.LoadKWh > 0 {
		pct := (1 - *m.ImportKWh / *m.LoadKWh) * 100
		m.SelfSufficiencyPct = ptr(math.Max(0, math.Min(100, pct)))
	}
}

func (m Metrics) allZeroOrNil() bool {
	for _, p := range []*float64{m.PVKWh, m.ExportKWh, m.ImportKWh, m.BatteryChargeKWh, m.BatteryDischargeKWh, m.LoadKWh, m.InverterOutputKWh} {
		if p != nil && *p != 0 {
			return false
		}
	}
	return true
}

type Bucket struct {
	Start   string `json:"start"`
	Partial bool   `json:"partial,omitempty"`
	Metrics
}

type ReportOut struct {
	Device  string   `json:"device_masked"`
	From    string   `json:"from"`
	To      string   `json:"to"`
	GroupBy string   `json:"group_by"`
	Status  string   `json:"status"` // ok, partial, no_data
	Totals  Metrics  `json:"totals"`
	Buckets []Bucket `json:"buckets"`
	Notes   []string `json:"notes"`
}

func parseDate(name, v string) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", v, london)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be a date like 2026-10-31, got %q", name, v)
	}
	return t, nil
}

func dayStart(t time.Time) time.Time {
	y, m, d := t.In(london).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, london)
}

func daysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// report fetches one month (daily values) or one year (monthly values),
// caching it so repeat questions don't use up the daily call allowance.
func (s *Service) report(ctx context.Context, sn, dim string, year, month int) (map[string][]float64, error) {
	key := fmt.Sprintf("%s|%s|%d|%d", sn, dim, year, month)
	now := s.now()
	s.mu.Lock()
	e, ok := s.cache[key]
	s.mu.Unlock()
	if !ok || !now.Before(e.expiry) {
		series, err := s.api.ReportQuery(ctx, sn, dim, year, month, reportVars)
		if err != nil {
			return nil, err
		}
		ly, lm, _ := now.In(london).Date()
		open := year > ly || (year == ly && (dim == "year" || month >= int(lm)))
		ttl := closedTTL
		if open {
			ttl = openTTL
		}
		e = cacheEntry{series: series, expiry: now.Add(ttl)}
		s.mu.Lock()
		s.cache[key] = e
		s.mu.Unlock()
	}
	out := map[string][]float64{}
	for _, sr := range e.series {
		out[sr.Variable] = sr.Values
	}
	return out, nil
}

func valueAt(series map[string][]float64, variable string, idx int) *float64 {
	v, ok := series[variable]
	if !ok || idx < 0 || idx >= len(v) {
		return nil
	}
	return &v[idx]
}

func metricsAt(series map[string][]float64, idx int) Metrics {
	return Metrics{
		PVKWh: valueAt(series, vPV, idx), ExportKWh: valueAt(series, vFeedIn, idx), ImportKWh: valueAt(series, vGrid, idx),
		BatteryChargeKWh: valueAt(series, vCharge, idx), BatteryDischargeKWh: valueAt(series, vDischarge, idx),
		LoadKWh: valueAt(series, vLoads, idx), InverterOutputKWh: valueAt(series, vGen, idx),
	}
}

func (s *Service) GetEnergyReport(ctx context.Context, device, from, to, groupBy string) (*ReportOut, error) {
	if groupBy == "" {
		groupBy = "day"
	}
	if groupBy != "day" && groupBy != "month" {
		return nil, fmt.Errorf("group_by must be day or month, got %q", groupBy)
	}
	start, err := parseDate("from", from)
	if err != nil {
		return nil, err
	}
	end, err := parseDate("to", to)
	if err != nil {
		return nil, err
	}
	if end.Before(start) {
		return nil, errors.New("to is before from")
	}
	today := dayStart(s.now())
	var notes []string
	if end.After(today) {
		end = today
		notes = append(notes, "to is in the future, so the range stops at today.")
	}
	if start.After(end) {
		return nil, errors.New("from is after today; there is no data yet")
	}
	switch groupBy {
	case "day":
		if int(end.Sub(start).Hours()/24)+1 > maxDayRange {
			return nil, fmt.Errorf("day grouping is limited to %d days; use group_by month for longer ranges", maxDayRange)
		}
	case "month":
		sy, sm, _ := start.Date()
		ey, em, _ := end.Date()
		if (ey-sy)*12+int(em-sm)+1 > maxMonthRange {
			return nil, fmt.Errorf("month grouping is limited to %d months", maxMonthRange)
		}
	}

	sn, err := s.resolveDevice(ctx, device)
	if err != nil {
		return nil, err
	}
	out := &ReportOut{Device: mask(sn), From: start.Format("2006-01-02"), To: end.Format("2006-01-02"), GroupBy: groupBy, Buckets: []Bucket{}}
	var anyPartial bool
	seen := map[string]bool{}

	if groupBy == "day" {
		y, m := start.Year(), start.Month()
		for ; time.Date(y, m, 1, 0, 0, 0, 0, london).Before(end.AddDate(0, 0, 1)); y, m = nextMonth(y, m) {
			series, err := s.report(ctx, sn, "month", y, int(m))
			if err != nil {
				return nil, err
			}
			noteVars(series, seen)
			for d := 1; d <= daysIn(y, m); d++ {
				day := time.Date(y, m, d, 0, 0, 0, 0, london)
				if day.Before(start) || day.After(end) {
					continue
				}
				out.Buckets = append(out.Buckets, bucket(day.Format("2006-01-02"), day.Equal(today), metricsAt(series, d-1), &anyPartial))
			}
		}
	} else {
		for y := start.Year(); y <= end.Year(); y++ {
			series, err := s.report(ctx, sn, "year", y, 0)
			if err != nil {
				return nil, err
			}
			noteVars(series, seen)
			for m := time.January; m <= time.December; m++ {
				first := time.Date(y, m, 1, 0, 0, 0, 0, london)
				last := first.AddDate(0, 1, -1)
				if last.Before(start) || first.After(end) {
					continue
				}
				cy, cm, _ := today.Date()
				out.Buckets = append(out.Buckets, bucket(first.Format("2006-01"), y == cy && m == cm, metricsAt(series, int(m)-1), &anyPartial))
			}
		}
		notes = append(notes, "Month buckets cover whole calendar months, even if from or to falls mid-month.")
	}

	allZero := true
	for i := range out.Buckets {
		out.Totals.add(out.Buckets[i].Metrics)
		if !out.Buckets[i].allZeroOrNil() {
			allZero = false
		}
		out.Buckets[i].finish()
	}
	out.Totals.finish()

	switch {
	case len(seen) == 0 || allZero:
		out.Status = "no_data"
		notes = append(notes, "FoxESS reported no energy at all for this period. The inverter or its connection may have been offline; this is missing data, not necessarily zero use.")
	case anyPartial:
		out.Status = "partial"
		notes = append(notes, "The range includes today or the current month, which is still incomplete.")
	default:
		out.Status = "ok"
	}
	for _, v := range reportVars {
		if len(seen) > 0 && !seen[v] {
			notes = append(notes, fmt.Sprintf("FoxESS did not return %q for this period, so the matching fields are null.", v))
		}
	}
	notes = append(notes, "Dates are calendar days or months as FoxESS reports them. pv_self_consumed_kwh is pv minus export (all export counted as solar); self_sufficiency_pct is the share of load not bought from the grid.")
	out.Notes = notes
	return out, nil
}

func nextMonth(y int, m time.Month) (int, time.Month) {
	if m == time.December {
		return y + 1, time.January
	}
	return y, m + 1
}

func noteVars(series map[string][]float64, seen map[string]bool) {
	for _, v := range reportVars {
		if _, ok := series[v]; ok {
			seen[v] = true
		}
	}
}

func bucket(start string, partial bool, m Metrics, any *bool) Bucket {
	if partial {
		*any = true
	}
	return Bucket{Start: start, Partial: partial, Metrics: m}
}
