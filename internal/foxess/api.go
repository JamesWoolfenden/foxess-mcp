package foxess

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
)

// Device is one inverter as listed by the API.
type Device struct {
	SN          string `json:"deviceSN"`
	ModuleSN    string `json:"moduleSN"`
	StationID   string `json:"stationID"`
	StationName string `json:"stationName"`
	Status      int    `json:"status"` // 1 online, 2 fault, 3 offline
	HasPV       bool   `json:"hasPV"`
	HasBattery  bool   `json:"hasBattery"`
	DeviceType  string `json:"deviceType"`
	ProductType string `json:"productType"`
}

// DeviceDetail is the extra information from the device detail endpoint.
type DeviceDetail struct {
	Capacity    float64          `json:"capacity"` // inverter rating, kW
	Status      int              `json:"status"`
	BatteryList []map[string]any `json:"batteryList"`
}

// RealPoint is one live reading.
type RealPoint struct {
	Variable string `json:"variable"`
	Unit     string `json:"unit"`
	Name     string `json:"name"`
	Value    any    `json:"value"`
	Time     string `json:"time"`
}

// ReportSeries is one variable's values across a period: by day for a month
// query, by month for a year query.
type ReportSeries struct {
	Variable string    `json:"variable"`
	Unit     string    `json:"unit"`
	Values   []float64 `json:"values"`
}

// DeviceList returns every device on the account.
func (c *Client) DeviceList(ctx context.Context) ([]Device, error) {
	var all []Device
	for page := 1; ; page++ {
		var res struct {
			Total int      `json:"total"`
			Data  []Device `json:"data"`
		}
		body := map[string]int{"currentPage": page, "pageSize": 100}
		if err := c.do(ctx, http.MethodPost, "/op/v0/device/list", nil, body, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Data...)
		if len(res.Data) == 0 || len(all) >= res.Total {
			return all, nil
		}
	}
}

// DeviceDetail returns rating and battery information for one device.
func (c *Client) DeviceDetail(ctx context.Context, sn string) (*DeviceDetail, error) {
	var d DeviceDetail
	if err := c.do(ctx, http.MethodGet, "/op/v0/device/detail", url.Values{"sn": {sn}}, nil, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// RealResult is a live-data answer, with enough about its shape to tell
// "the inverter sent nothing" from "the answer wasn't what we expected".
type RealResult struct {
	Points  []RealPoint
	Entries int      // device entries in the answer
	Fields  []string // field names seen on those entries (never values)
}

// RealQuery returns the latest readings. Variables with no data are simply
// absent from the result; omit variables to get every one.
func (c *Client) RealQuery(ctx context.Context, sn string, variables []string) (*RealResult, error) {
	var res []map[string]json.RawMessage
	body := map[string]any{"sns": []string{sn}}
	if len(variables) > 0 {
		body["variables"] = variables
	}
	if err := c.do(ctx, http.MethodPost, "/op/v0/device/real/query", nil, body, &res); err != nil {
		return nil, err
	}
	out := &RealResult{Entries: len(res)}
	seen := map[string]bool{}
	for _, entry := range res {
		for k := range entry {
			if !seen[k] {
				seen[k] = true
				out.Fields = append(out.Fields, k)
			}
		}
		// Only this serial was requested, so don't insist the echo matches.
		raw, ok := entry["datas"]
		if !ok {
			raw, ok = entry["data"] // tolerate the other spelling
		}
		if !ok {
			continue
		}
		var pts []RealPoint
		if err := json.Unmarshal(raw, &pts); err != nil {
			return nil, fmt.Errorf("foxess real query: unexpected readings shape: %w", err)
		}
		out.Points = append(out.Points, pts...)
	}
	sort.Strings(out.Fields)
	return out, nil
}

// ReportQuery returns energy totals. dimension "month" gives one value per day
// of the month, "year" gives one value per month of the year.
func (c *Client) ReportQuery(ctx context.Context, sn, dimension string, year, month int, variables []string) ([]ReportSeries, error) {
	body := map[string]any{"sn": sn, "year": year, "dimension": dimension, "variables": variables}
	if month > 0 {
		body["month"] = month
	}
	var res []ReportSeries
	if err := c.do(ctx, http.MethodPost, "/op/v0/device/report/query", nil, body, &res); err != nil {
		return nil, err
	}
	return res, nil
}
