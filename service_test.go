package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JamesWoolfenden/foxess-mcp/internal/foxess"
)

// All identifiers below are fake.
const (
	testKey = "test-key-NOT-REAL-1234"
	testSN  = "TESTSN000001"
	testSN2 = "TESTSN000002"
)

// fakeAPI serves fixture responses shaped like the FoxESS Open API and checks
// the token and signature on every request. Nothing here talks to the real service.
type fakeAPI struct {
	srv       *httptest.Server
	devices   []string
	omitVar   map[string]bool // report variables the API leaves out
	zeroAll   bool            // every report value is 0 (inverter offline)
	mu        sync.Mutex
	reportReq []map[string]any
}

// factor gives each report variable a distinct, predictable value: the value
// for day (or month) n is factor * n.
var factor = map[string]float64{
	"PVEnergyTotal": 2, "generation": 2, "feedin": 1, "gridConsumption": 3,
	"chargeEnergyToTal": 0.5, "dischargeEnergyToTal": 0.5, "loads": 4,
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{devices: []string{testSN}, omitVar: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) service(t *testing.T, now time.Time) *Service {
	t.Helper()
	c, err := foxess.NewClient(f.srv.URL, testKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetTiming(0, time.Millisecond)
	s := NewService(c, "")
	s.now = func() time.Time { return now }
	return s
}

func (f *fakeAPI) reports() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reportReq)
}

func reply(w http.ResponseWriter, result any) {
	json.NewEncoder(w).Encode(map[string]any{"errno": 0, "result": result})
}

func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	ts := r.Header.Get("timestamp")
	sum := md5.Sum([]byte(r.URL.Path + `\r\n` + testKey + `\r\n` + ts))
	if r.Header.Get("token") != testKey || r.Header.Get("signature") != hex.EncodeToString(sum[:]) {
		json.NewEncoder(w).Encode(map[string]any{"errno": 40256, "msg": "illegal signature"})
		return
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)

	switch r.URL.Path {
	case "/op/v0/device/list":
		var data []map[string]any
		for _, sn := range f.devices {
			data = append(data, map[string]any{"deviceSN": sn, "moduleSN": "TESTMOD0001", "stationID": "station-test-1", "stationName": "1 Test Street", "status": 1, "hasPV": true, "hasBattery": true, "deviceType": "H1-5.0-E", "productType": "H1"})
		}
		reply(w, map[string]any{"currentPage": 1, "pageSize": 100, "total": len(data), "data": data})
	case "/op/v0/device/detail":
		reply(w, map[string]any{"deviceSN": r.URL.Query().Get("sn"), "capacity": 5.0, "status": 1, "batteryList": []map[string]any{{"batterySN": "TESTBAT0001"}}})
	case "/op/v0/device/real/query":
		sns, _ := body["sns"].([]any)
		reply(w, []map[string]any{{"deviceSN": sns[0], "datas": []map[string]any{
			{"variable": "pvPower", "unit": "kW", "name": "PV Power", "value": 1.234, "time": "2026-11-02 12:00:00 GMT+0000"},
			{"variable": "SoC", "unit": "%", "name": "SoC", "value": 64, "time": "2026-11-02 12:00:00 GMT+0000"},
		}}})
	case "/op/v0/device/report/query":
		f.mu.Lock()
		f.reportReq = append(f.reportReq, body)
		f.mu.Unlock()
		n := 12
		if body["dimension"] == "month" {
			n = 31
		}
		var res []map[string]any
		for _, v := range body["variables"].([]any) {
			name := v.(string)
			if f.omitVar[name] {
				continue
			}
			vals := make([]float64, n)
			for i := range vals {
				if !f.zeroAll {
					vals[i] = factor[name] * float64(i+1)
				}
			}
			res = append(res, map[string]any{"variable": name, "unit": "kWh", "values": vals})
		}
		reply(w, res)
	default:
		http.NotFound(w, r)
	}
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func approx(a, b float64) bool { return math.Abs(a-b) < 0.005 }

func val(p *float64) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprint(*p)
}

func TestGetEnergyReportDaily(t *testing.T) {
	f := newFakeAPI(t)
	s := f.service(t, at("2026-11-02T12:00:00Z"))
	out, err := s.GetEnergyReport(context.Background(), "", "2026-10-30", "2026-11-02", "day")
	if err != nil {
		t.Fatal(err)
	}
	// Day n of a month has pv=2n, export=n, import=3n, load=4n.
	want := []struct {
		start   string
		pv      float64
		partial bool
	}{{"2026-10-30", 60, false}, {"2026-10-31", 62, false}, {"2026-11-01", 2, false}, {"2026-11-02", 4, true}}
	if len(out.Buckets) != len(want) {
		t.Fatalf("got %d buckets, want %d: %+v", len(out.Buckets), len(want), out.Buckets)
	}
	for i, w := range want {
		b := out.Buckets[i]
		if b.Start != w.start || b.PVKWh == nil || !approx(*b.PVKWh, w.pv) || b.Partial != w.partial {
			t.Errorf("bucket %d = %s pv=%s partial=%v, want %s pv=%v partial=%v", i, b.Start, val(b.PVKWh), b.Partial, w.start, w.pv, w.partial)
		}
	}
	if out.Totals.PVKWh == nil || !approx(*out.Totals.PVKWh, 128) {
		t.Errorf("total pv = %s, want 128", val(out.Totals.PVKWh))
	}
	if out.Status != "partial" {
		t.Errorf("status = %s, want partial (today is included)", out.Status)
	}
	if got := f.reports(); got != 2 {
		t.Errorf("report calls = %d, want 2 (October and November)", got)
	}
	if out.Device != "********0001" {
		t.Errorf("device = %q, want masked", out.Device)
	}
}

func TestDerivedFields(t *testing.T) {
	s := newFakeAPI(t).service(t, at("2026-11-20T12:00:00Z"))
	out, err := s.GetEnergyReport(context.Background(), "", "2026-11-10", "2026-11-10", "day")
	if err != nil {
		t.Fatal(err)
	}
	b := out.Buckets[0]
	// Day 10: pv 20, export 10, import 30, load 40.
	if b.PVSelfConsumedKWh == nil || !approx(*b.PVSelfConsumedKWh, 10) {
		t.Errorf("self consumed = %s, want 10", val(b.PVSelfConsumedKWh))
	}
	if b.SelfSufficiencyPct == nil || !approx(*b.SelfSufficiencyPct, 25) {
		t.Errorf("self sufficiency = %s, want 25", val(b.SelfSufficiencyPct))
	}
	if out.Status != "ok" {
		t.Errorf("status = %s, want ok", out.Status)
	}
}

func TestGetEnergyReportMonthly(t *testing.T) {
	f := newFakeAPI(t)
	s := f.service(t, at("2026-11-02T12:00:00Z"))
	out, err := s.GetEnergyReport(context.Background(), "", "2025-11-01", "2026-10-31", "month")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Buckets) != 12 || out.Buckets[0].Start != "2025-11" || out.Buckets[11].Start != "2026-10" {
		t.Fatalf("buckets = %d, first %s", len(out.Buckets), out.Buckets[0].Start)
	}
	// Month m has pv = 2m: Nov25 22, Dec25 24, Jan-Oct26 2*(1..10) = 110.
	if out.Totals.PVKWh == nil || !approx(*out.Totals.PVKWh, 156) {
		t.Errorf("total pv = %s, want 156", val(out.Totals.PVKWh))
	}
	if got := f.reports(); got != 2 {
		t.Errorf("report calls = %d, want 2 (2025 and 2026)", got)
	}
	if out.Status != "ok" {
		t.Errorf("status = %s, want ok", out.Status)
	}
	for _, r := range f.reportReq {
		if r["dimension"] != "year" {
			t.Errorf("dimension = %v, want year", r["dimension"])
		}
	}
}

func TestFutureRangeIsCappedAtToday(t *testing.T) {
	s := newFakeAPI(t).service(t, at("2026-11-02T12:00:00Z"))
	out, err := s.GetEnergyReport(context.Background(), "", "2026-11-01", "2026-12-31", "day")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Buckets) != 2 || out.To != "2026-11-02" {
		t.Errorf("buckets = %d, to = %s; want 2 buckets ending 2026-11-02", len(out.Buckets), out.To)
	}
	if !strings.Contains(strings.Join(out.Notes, " "), "future") {
		t.Errorf("notes = %v, want a mention of the future cap", out.Notes)
	}
}

func TestMissingVariableIsNullNotZero(t *testing.T) {
	f := newFakeAPI(t)
	f.omitVar["loads"] = true
	s := f.service(t, at("2026-11-20T12:00:00Z"))
	out, err := s.GetEnergyReport(context.Background(), "", "2026-11-10", "2026-11-11", "day")
	if err != nil {
		t.Fatal(err)
	}
	b := out.Buckets[0]
	if b.LoadKWh != nil || b.SelfSufficiencyPct != nil {
		t.Errorf("load = %s, sufficiency = %s; want both null", val(b.LoadKWh), val(b.SelfSufficiencyPct))
	}
	if b.PVKWh == nil {
		t.Error("pv should still be reported")
	}
	if !strings.Contains(strings.Join(out.Notes, " "), `"loads"`) {
		t.Errorf("notes = %v, want a mention of the missing loads variable", out.Notes)
	}
}

func TestAllZeroIsNoDataNotZeroUse(t *testing.T) {
	f := newFakeAPI(t)
	f.zeroAll = true
	s := f.service(t, at("2026-11-20T12:00:00Z"))
	out, err := s.GetEnergyReport(context.Background(), "", "2026-11-05", "2026-11-07", "day")
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "no_data" || !strings.Contains(strings.Join(out.Notes, " "), "missing data") {
		t.Errorf("status = %s, notes = %v; want no_data with an explanation", out.Status, out.Notes)
	}
	if _, err := json.Marshal(out); err != nil {
		t.Errorf("zero-energy result must still encode as JSON: %v", err)
	}
}

func TestReportErrors(t *testing.T) {
	f := newFakeAPI(t)
	s := f.service(t, at("2026-11-02T12:00:00Z"))
	tests := []struct {
		name, from, to, group, want string
	}{
		{"bad group", "2026-10-01", "2026-10-02", "week", "group_by"},
		{"bad from", "01/10/2026", "2026-10-02", "day", "from must be a date"},
		{"bad to", "2026-10-01", "tomorrow", "day", "to must be a date"},
		{"reversed", "2026-10-05", "2026-10-02", "day", "before from"},
		{"too many days", "2025-01-01", "2026-10-01", "day", "limited to 366"},
		{"too many months", "2022-01-01", "2026-10-01", "month", "limited to 36"},
		{"starts in the future", "2027-01-01", "2027-01-31", "day", "after today"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.GetEnergyReport(context.Background(), "", tc.from, tc.to, tc.group)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if got := f.reports(); got != 0 {
		t.Errorf("invalid requests made %d report calls, want 0", got)
	}
}

func TestRepeatQuestionsAreCached(t *testing.T) {
	f := newFakeAPI(t)
	now := at("2026-11-02T12:00:00Z")
	s := f.service(t, now)
	for i := 0; i < 3; i++ {
		if _, err := s.GetEnergyReport(context.Background(), "", "2026-10-01", "2026-10-31", "day"); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.reports(); got != 1 {
		t.Fatalf("report calls = %d, want 1 (the daily call allowance is limited)", got)
	}
	// The current month is still changing, so it expires sooner.
	s.now = func() time.Time { return now.Add(10 * time.Minute) }
	before := f.reports()
	s.GetEnergyReport(context.Background(), "", "2026-11-01", "2026-11-01", "day")
	s.now = func() time.Time { return now.Add(20 * time.Minute) }
	s.GetEnergyReport(context.Background(), "", "2026-11-01", "2026-11-01", "day")
	if got := f.reports() - before; got != 2 {
		t.Errorf("open-month refetches = %d, want 2 after the 5 minute TTL", got)
	}
}

func TestDeviceSelection(t *testing.T) {
	f := newFakeAPI(t)
	f.devices = []string{testSN, testSN2}
	s := f.service(t, at("2026-11-02T12:00:00Z"))
	_, err := s.GetRealtime(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "2 devices") || strings.Contains(err.Error(), testSN) {
		t.Fatalf("err = %v, want a prompt to choose, with masked serials only", err)
	}
	for _, want := range []string{testSN2, "********0002"} {
		out, err := s.GetRealtime(context.Background(), want)
		if err != nil || out.Device != "********0002" {
			t.Errorf("device %q: out = %+v, err = %v", want, out, err)
		}
	}
	if _, err := s.GetRealtime(context.Background(), "NOSUCHSN"); err == nil {
		t.Error("an unknown device must be an error")
	}
}

func TestGetRealtime(t *testing.T) {
	s := newFakeAPI(t).service(t, at("2026-11-02T12:00:00Z"))
	out, err := s.GetRealtime(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" || len(out.Readings) != 2 || out.Readings[0].Variable != "pvPower" || out.Readings[0].Unit != "kW" {
		t.Errorf("out = %+v", out)
	}
}

func TestGetPlantsOmitsAddressAndSerial(t *testing.T) {
	s := newFakeAPI(t).service(t, at("2026-11-02T12:00:00Z"))
	out, err := s.GetPlants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Devices) != 1 || out.Devices[0].Status != "online" || out.Devices[0].Batteries != 1 || !approx(out.Devices[0].CapacityKW, 5) {
		t.Errorf("devices = %+v", out.Devices)
	}
}

func TestNoSecretsOrAddressesInAnyOutput(t *testing.T) {
	f := newFakeAPI(t)
	s := f.service(t, at("2026-11-02T12:00:00Z"))
	plants, _ := s.GetPlants(context.Background())
	rt, _ := s.GetRealtime(context.Background(), "")
	rep, _ := s.GetEnergyReport(context.Background(), "", "2026-10-01", "2026-10-05", "day")
	rep2, err := s.GetEnergyReport(context.Background(), "NOSUCHSN", "2026-10-01", "2026-10-05", "day")
	if err == nil || rep2 != nil {
		t.Fatalf("want an error for an unknown device, got %v", err)
	}
	b, err := json.Marshal([]any{plants, rt, rep, err.Error()})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testKey, testSN, "1 Test Street", "station-test-1", "TESTMOD0001", "TESTBAT0001"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("output leaks %q", secret)
		}
	}
}

func TestBadKeyIsReportedWithoutLeakingIt(t *testing.T) {
	f := newFakeAPI(t)
	c, _ := foxess.NewClient(f.srv.URL, "wrong-key-NOT-REAL", nil)
	c.SetTiming(0, time.Millisecond)
	s := NewService(c, "")
	_, err := s.GetPlants(context.Background())
	if err == nil || !strings.Contains(err.Error(), "40256") {
		t.Fatalf("err = %v, want the FoxESS errno", err)
	}
	if strings.Contains(err.Error(), "wrong-key") {
		t.Errorf("error leaks the key: %v", err)
	}
}

func TestMaskAndRound(t *testing.T) {
	for in, want := range map[string]string{"": "", "abc": "***", "abcd": "****", "ABCDEFGH1234": "********1234"} {
		if got := mask(in); got != want {
			t.Errorf("mask(%q) = %q, want %q", in, got, want)
		}
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := round(v, 2); got != 0 {
			t.Errorf("round(%v) = %v, want 0 so JSON can encode it", v, got)
		}
	}
}
