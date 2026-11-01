package foxess

import (
	"context"
	"net/http"
	"net/url"
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

// RealQuery returns the latest readings for the named variables. Variables
// with no data are simply absent from the result.
func (c *Client) RealQuery(ctx context.Context, sn string, variables []string) ([]RealPoint, error) {
	var res []struct {
		SN    string      `json:"deviceSN"`
		Datas []RealPoint `json:"datas"`
	}
	body := map[string]any{"sns": []string{sn}, "variables": variables}
	if err := c.do(ctx, http.MethodPost, "/op/v0/device/real/query", nil, body, &res); err != nil {
		return nil, err
	}
	var out []RealPoint
	for _, r := range res {
		if r.SN == sn {
			out = append(out, r.Datas...)
		}
	}
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
