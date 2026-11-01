// foxess-mcp is a read-only MCP server (stdio) for FoxESS Cloud solar and battery data.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"github.com/JamesWoolfenden/foxess-mcp/internal/foxess"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var version = "dev"

func main() {
	// stdout carries the MCP protocol; log to stderr only, and never the key.
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	key := os.Getenv("FOXESS_API_KEY")
	if key == "" {
		log.Error("FOXESS_API_KEY must be set (FoxESS Cloud: profile > API Management)")
		os.Exit(2)
	}
	base := os.Getenv("FOXESS_BASE_URL")
	if base == "" {
		base = foxess.DefaultBaseURL
	}
	api, err := foxess.NewClient(base, key, nil)
	if err != nil {
		log.Error("creating client", "err", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	pinned := os.Getenv("FOXESS_DEVICE_SN")
	server := newServer(NewService(api, pinned))
	log.Info("foxess-mcp starting", "version", version, "base", base, "pinned_device", mask(pinned))
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

// jsonResult renders a value as indented JSON text content.
func jsonResult(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
}

func wrap[In any, Out any](f func(context.Context, In) (Out, error)) mcp.ToolHandlerFor[In, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		out, err := f(ctx, in)
		if err != nil {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("error: %v", err)}}}, nil, nil
		}
		return jsonResult(out)
	}
}

type noInput struct{}

type deviceIn struct {
	Device string `json:"device,omitempty" jsonschema:"inverter serial, full or masked as get_plants shows it; optional when the account has one inverter"`
}

type reportIn struct {
	Device  string `json:"device,omitempty" jsonschema:"inverter serial, full or masked; optional when the account has one inverter"`
	From    string `json:"from" jsonschema:"start date YYYY-MM-DD (UK local calendar day)"`
	To      string `json:"to" jsonschema:"end date YYYY-MM-DD, inclusive; capped at today"`
	GroupBy string `json:"group_by,omitempty" jsonschema:"day (default, max 366 days) or month (whole calendar months, max 36)"`
}

func newServer(svc *Service) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "foxess-mcp", Version: version}, nil)
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_plants",
		Description: "FoxESS inverters on the account: status, model, whether it has solar and a battery, battery count and inverter rating. Serial numbers are masked and the station name is omitted.",
		Annotations: ro,
	}, wrap(func(ctx context.Context, _ noInput) (*PlantsOut, error) { return svc.GetPlants(ctx) }))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_energy_report",
		Description: "Solar generation, grid import and export, battery charge and discharge and household load in kWh, per day or month, with totals. Fills the gap the grid meters leave: how much solar was made and how much was used in the house. Missing variables are null, and a period with no energy at all is reported as no_data rather than zero.",
		Annotations: ro,
	}, wrap(func(ctx context.Context, in reportIn) (*ReportOut, error) {
		return svc.GetEnergyReport(ctx, in.Device, in.From, in.To, in.GroupBy)
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_realtime",
		Description: "Live readings: solar, load, grid import and export, battery charge and discharge power, and battery state of charge.",
		Annotations: ro,
	}, wrap(func(ctx context.Context, in deviceIn) (*RealtimeOut, error) { return svc.GetRealtime(ctx, in.Device) }))

	return s
}
