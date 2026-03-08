// Command miniflow is the CLI client for the miniflow workflow engine.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"miniflow/internal/client"
)

// ── Color helpers ──────────────────────────────────────────────────────────

var noColor = os.Getenv("NO_COLOR") != ""

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorGray   = "\033[90m"
	colorBold   = "\033[1m"
	colorCyan   = "\033[36m"
)

func c(color, text string) string {
	if noColor {
		return text
	}
	return color + text + colorReset
}

func bold(text string) string { return c(colorBold, text) }

func statusColor(status string) string {
	switch status {
	case "completed":
		return colorGreen
	case "running":
		return colorYellow
	case "failed":
		return colorRed
	case "pending":
		return colorBlue
	case "cancelled":
		return colorGray
	default:
		return colorReset
	}
}

func coloredStatus(status string) string {
	return c(statusColor(status), status)
}

// ── Formatting helpers ─────────────────────────────────────────────────────

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func humanAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func humanDuration(start, end *time.Time) string {
	if start == nil {
		return "-"
	}
	var d time.Duration
	if end != nil {
		d = end.Sub(*start)
	} else {
		d = time.Since(*start)
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%.1fm", d.Minutes())
}

// printTable renders a simple aligned table to stdout.
func printTable(headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	// Header
	for i, h := range headers {
		fmt.Printf("%-*s", widths[i]+2, bold(h))
	}
	fmt.Println()
	// Separator
	for i := range headers {
		fmt.Print(strings.Repeat("-", widths[i]+2))
	}
	fmt.Println()
	// Rows
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) {
				fmt.Printf("%-*s", widths[i]+2, cell)
			}
		}
		fmt.Println()
	}
}

// ── Main entrypoint ────────────────────────────────────────────────────────

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	baseURL := os.Getenv("MINIFLOW_API")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}
	cli := client.NewClient(baseURL)

	subcmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch subcmd {
	case "register":
		err = cmdRegister(cli, args)
	case "run":
		err = cmdRun(cli, args)
	case "status":
		err = cmdStatus(cli, args)
	case "events":
		err = cmdEvents(cli, args)
	case "list":
		err = cmdList(cli, args)
	case "cancel":
		err = cmdCancel(cli, args)
	case "workflows":
		err = cmdWorkflows(cli, args)
	case "stats":
		err = cmdStats(cli, args)
	case "help", "--help", "-h":
		printUsage()
		return
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", subcmd)
		printUsage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// ── Commands ───────────────────────────────────────────────────────────────

func cmdRegister(cli *client.Client, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: miniflow register <workflow.json>")
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return fmt.Errorf("reading file: %w", err)
	}

	var req struct {
		Name       string                    `json:"name"`
		Definition client.WorkflowDefinition `json:"definition"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return fmt.Errorf("parsing JSON: %w", err)
	}
	if req.Name == "" {
		return fmt.Errorf("workflow JSON must contain a \"name\" field")
	}

	wf, err := cli.RegisterWorkflow(req.Name, req.Definition)
	if err != nil {
		return err
	}
	fmt.Printf("%s Workflow %s registered (ID: %s)\n",
		c(colorGreen, "OK"),
		bold(wf.Name),
		c(colorGray, shortID(wf.ID)),
	)
	return nil
}

func cmdRun(cli *client.Client, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	input := fs.String("input", "{}", "JSON input for the workflow run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: miniflow run <workflow-name> [--input '{}']")
	}
	workflowName := fs.Arg(0)

	run, err := cli.StartRun(workflowName, *input)
	if err != nil {
		return err
	}
	fmt.Printf("%s Run started: %s\n",
		c(colorGreen, "OK"),
		bold(run.ID),
	)
	return nil
}

func cmdStatus(cli *client.Client, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: miniflow status <run-id>")
	}
	runID := args[0]

	detail, err := cli.GetRun(runID)
	if err != nil {
		return err
	}

	// Run info header
	fmt.Println(bold("Run Information"))
	fmt.Println(strings.Repeat("-", 50))
	fmt.Printf("  %-14s %s\n", "Run ID:", detail.ID)
	fmt.Printf("  %-14s %s\n", "Workflow ID:", detail.WorkflowID)
	fmt.Printf("  %-14s %s\n", "Status:", coloredStatus(detail.Status))
	fmt.Printf("  %-14s %d\n", "Current Step:", detail.CurrentStep)
	fmt.Printf("  %-14s %s\n", "Created:", detail.CreatedAt.Local().Format(time.RFC3339))
	if detail.StartedAt != nil {
		fmt.Printf("  %-14s %s\n", "Started:", detail.StartedAt.Local().Format(time.RFC3339))
	}
	if detail.CompletedAt != nil {
		fmt.Printf("  %-14s %s\n", "Completed:", detail.CompletedAt.Local().Format(time.RFC3339))
		fmt.Printf("  %-14s %s\n", "Duration:", humanDuration(detail.StartedAt, detail.CompletedAt))
	}
	if detail.Input != "" && detail.Input != "{}" {
		fmt.Printf("  %-14s %s\n", "Input:", detail.Input)
	}
	if detail.Output != "" {
		fmt.Printf("  %-14s %s\n", "Output:", detail.Output)
	}

	// Activity table
	if len(detail.Activities) > 0 {
		fmt.Println()
		fmt.Println(bold("Activities"))

		headers := []string{"NAME", "STATUS", "ATTEMPTS", "DURATION"}
		var rows [][]string
		for _, a := range detail.Activities {
			rows = append(rows, []string{
				a.ActivityName,
				coloredStatus(a.Status),
				fmt.Sprintf("%d/%d", a.Attempts, a.MaxRetries+1),
				humanDuration(a.StartedAt, a.CompletedAt),
			})
		}
		printTable(headers, rows)
	}
	return nil
}

func cmdEvents(cli *client.Client, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: miniflow events <run-id>")
	}
	runID := args[0]

	events, err := cli.GetEvents(runID)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		fmt.Println(c(colorGray, "No events found."))
		return nil
	}

	fmt.Println(bold("Event Timeline"))
	fmt.Println()

	for i, ev := range events {
		timestamp := ev.CreatedAt.Local().Format("15:04:05.000")
		eventColor := eventTypeColor(ev.EventType)
		connector := "|"
		if i == len(events)-1 {
			connector = " "
		}
		fmt.Printf("  %s  %s  %s\n",
			c(colorGray, timestamp),
			c(eventColor, fmt.Sprintf("%-28s", ev.EventType)),
			activityLabel(ev.ActivityRunID),
		)
		if ev.Payload != "" && ev.Payload != "{}" {
			fmt.Printf("  %s  %s  %s\n",
				strings.Repeat(" ", 12),
				c(colorGray, connector),
				c(colorGray, truncate(ev.Payload, 80)),
			)
		} else {
			fmt.Printf("  %s  %s\n",
				strings.Repeat(" ", 12),
				c(colorGray, connector),
			)
		}
	}
	return nil
}

func cmdList(cli *client.Client, args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	status := fs.String("status", "", "Filter by status")
	limit := fs.Int("limit", 20, "Maximum number of runs to list")
	if err := fs.Parse(args); err != nil {
		return err
	}

	runs, err := cli.ListRuns(*status, *limit)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		fmt.Println(c(colorGray, "No runs found."))
		return nil
	}

	headers := []string{"ID", "WORKFLOW", "STATUS", "STEP", "AGE"}
	var rows [][]string
	for _, r := range runs {
		rows = append(rows, []string{
			shortID(r.ID),
			r.WorkflowID,
			coloredStatus(r.Status),
			fmt.Sprintf("%d", r.CurrentStep),
			humanAge(r.CreatedAt),
		})
	}
	printTable(headers, rows)
	return nil
}

func cmdCancel(cli *client.Client, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: miniflow cancel <run-id>")
	}
	runID := args[0]
	if err := cli.CancelRun(runID); err != nil {
		return err
	}
	fmt.Printf("%s Run %s cancelled.\n",
		c(colorGreen, "OK"),
		bold(shortID(runID)),
	)
	return nil
}

func cmdWorkflows(cli *client.Client, _ []string) error {
	wfs, err := cli.ListWorkflows()
	if err != nil {
		return err
	}
	if len(wfs) == 0 {
		fmt.Println(c(colorGray, "No workflows registered."))
		return nil
	}

	headers := []string{"NAME", "ID", "ACTIVITIES", "CREATED"}
	var rows [][]string
	for _, wf := range wfs {
		rows = append(rows, []string{
			bold(wf.Name),
			shortID(wf.ID),
			fmt.Sprintf("%d", len(wf.Definition.Activities)),
			humanAge(wf.CreatedAt),
		})
	}
	printTable(headers, rows)
	return nil
}

func cmdStats(cli *client.Client, _ []string) error {
	stats, err := cli.Stats()
	if err != nil {
		return err
	}
	if len(stats.Counts) == 0 {
		fmt.Println(c(colorGray, "No stats available."))
		return nil
	}

	fmt.Println(bold("Run Statistics"))
	fmt.Println()

	// Find max count for bar scaling
	maxCount := 0
	for _, count := range stats.Counts {
		if count > maxCount {
			maxCount = count
		}
	}

	// Sort statuses for consistent output
	statuses := make([]string, 0, len(stats.Counts))
	for s := range stats.Counts {
		statuses = append(statuses, s)
	}
	sort.Strings(statuses)

	barWidth := 40
	for _, status := range statuses {
		count := stats.Counts[status]
		barLen := 0
		if maxCount > 0 {
			barLen = int(math.Round(float64(count) / float64(maxCount) * float64(barWidth)))
		}
		if barLen < 1 && count > 0 {
			barLen = 1
		}
		bar := strings.Repeat("█", barLen) + strings.Repeat("░", barWidth-barLen)
		fmt.Printf("  %-12s %s %s\n",
			c(statusColor(status), fmt.Sprintf("%-10s", status)),
			c(statusColor(status), bar),
			fmt.Sprintf("%d", count),
		)
	}
	fmt.Println()

	// Total
	total := 0
	for _, count := range stats.Counts {
		total += count
	}
	fmt.Printf("  %s %d\n", bold("Total:"), total)
	return nil
}

// ── Usage ──────────────────────────────────────────────────────────────────

func printUsage() {
	fmt.Println(bold("miniflow") + " - workflow engine CLI")
	fmt.Println()
	fmt.Println(bold("USAGE:"))
	fmt.Println("  miniflow <command> [arguments]")
	fmt.Println()
	fmt.Println(bold("COMMANDS:"))
	fmt.Println("  register <workflow.json>          Register a workflow from a JSON file")
	fmt.Println("  run <workflow-name> [--input '{}'] Start a workflow run")
	fmt.Println("  status <run-id>                   Show run details and activity status")
	fmt.Println("  events <run-id>                   Show event timeline for a run")
	fmt.Println("  list [--status X] [--limit N]     List workflow runs")
	fmt.Println("  cancel <run-id>                   Cancel a running workflow")
	fmt.Println("  workflows                         List registered workflows")
	fmt.Println("  stats                             Show run statistics")
	fmt.Println("  help                              Show this help message")
	fmt.Println()
	fmt.Println(bold("ENVIRONMENT:"))
	fmt.Println("  MINIFLOW_API    Base URL of the miniflow server (default: http://localhost:8080)")
	fmt.Println("  NO_COLOR        Disable colored output when set")
}

// ── Helpers ────────────────────────────────────────────────────────────────

func eventTypeColor(eventType string) string {
	switch {
	case strings.HasSuffix(eventType, "_completed"):
		return colorGreen
	case strings.HasSuffix(eventType, "_started"):
		return colorYellow
	case strings.HasSuffix(eventType, "_failed"), strings.HasSuffix(eventType, "_timed_out"):
		return colorRed
	case strings.HasSuffix(eventType, "_scheduled"):
		return colorBlue
	case strings.HasSuffix(eventType, "_cancelled"):
		return colorGray
	case strings.HasSuffix(eventType, "_retried"):
		return colorCyan
	default:
		return colorReset
	}
}

func activityLabel(activityRunID string) string {
	if activityRunID == "" {
		return ""
	}
	return c(colorGray, "(activity: "+shortID(activityRunID)+")")
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
