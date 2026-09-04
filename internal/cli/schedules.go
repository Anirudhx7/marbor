package cli

// schedules.go - `marbor schedules list/create/patch/delete`, time-of-day
// warmup/unload/drain/undrain automation (P-A2-03, A2 three-surface-parity
// audit: GET/POST/PATCH/DELETE /admin/schedules had full UI coverage in
// Warmup.tsx but no CLI).

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

func printSchedulesUsage(w io.Writer) { writeHelp(w, findCommand(root(), "schedules")) }

// parseIntList splits a comma-separated flag value into []int, matching
// parseCommaList's "empty string means empty slice" convention.
func parseIntList(s string) ([]int, error) {
	out := []int{}
	s = strings.TrimSpace(s)
	if s == "" {
		return out, nil
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid --days entry %q (want an integer 0-6)", part)
		}
		out = append(out, n)
	}
	return out, nil
}

func printSchedule(w io.Writer, sc Schedule) {
	tw := newTabWriter(w)
	fmt.Fprintf(tw, "ID\t%s\n", sc.ID)
	fmt.Fprintf(tw, "ACTION\t%s\n", sc.Action)
	fmt.Fprintf(tw, "NODE\t%s\n", sc.Node)
	fmt.Fprintf(tw, "MODELS\t%v\n", sc.Models)
	fmt.Fprintf(tw, "AT\t%s\n", sc.At)
	fmt.Fprintf(tw, "DAYS\t%v\n", sc.Days)
	fmt.Fprintf(tw, "ENABLED\t%v\n", sc.Enabled)
	if sc.LastRunAt != "" {
		fmt.Fprintf(tw, "LAST RUN\t%s (%s)\n", sc.LastRunAt, sc.LastStatus)
	}
	tw.Flush()
}

// runSchedulesList implements `marbor schedules list`.
func runSchedulesList(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	scheds, err := client.ListSchedules()
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, scheds); handled {
		return code
	}
	tw := newTabWriter(stdout)
	fmt.Fprintln(tw, "ID\tACTION\tNODE\tAT\tDAYS\tENABLED\tLAST RUN")
	for _, sc := range scheds {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%v\t%v\t%s\n", sc.ID, sc.Action, sc.Node, sc.At, sc.Days, sc.Enabled, sc.LastRunAt)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	return ExitOK
}

// runSchedulesCreate implements `marbor schedules create --action --node
// --at [--models a,b] [--days 0,1,2] [--enabled]`.
func runSchedulesCreate(flags *globalFlags, action, node, at, models, days string, enabled bool, stdout, stderr io.Writer) int {
	dayList, err := parseIntList(days)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitUserError
	}
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	sc, err := client.CreateSchedule(ScheduleCreateRequest{
		Action: action, Node: node, At: at, Models: parseCommaList(models), Days: dayList, Enabled: enabled,
	})
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, sc); handled {
		return code
	}
	fmt.Fprintf(stdout, "schedule created:\n")
	printSchedule(stdout, *sc)
	return ExitOK
}

// runSchedulesPatch implements `marbor schedules patch <id> [--enabled
// true|false] [--action x] [--node x] [--models a,b] [--at HH:MM] [--days
// 0,1,2]` - only visited flags are sent (RunCtx.IsSet).
func runSchedulesPatch(ctx *RunCtx, id string) int {
	patch := SchedulePatch{}
	if ctx.IsSet("enabled") {
		v := ctx.Bool("enabled")
		patch.Enabled = &v
	}
	if ctx.IsSet("action") {
		v := ctx.String("action")
		patch.Action = &v
	}
	if ctx.IsSet("node") {
		v := ctx.String("node")
		patch.Node = &v
	}
	if ctx.IsSet("models") {
		v := parseCommaList(ctx.String("models"))
		patch.Models = &v
	}
	if ctx.IsSet("at") {
		v := ctx.String("at")
		patch.At = &v
	}
	if ctx.IsSet("days") {
		v, err := parseIntList(ctx.String("days"))
		if err != nil {
			fmt.Fprintf(ctx.Stderr, "error: %v\n", err)
			return ExitUserError
		}
		patch.Days = &v
	}
	client, err := authenticatedClient(ctx.Flags)
	if err != nil {
		return reportError(err, ctx.Stderr)
	}
	sc, err := client.PatchSchedule(id, patch)
	if err != nil {
		return reportError(err, ctx.Stderr)
	}
	if handled, code := emitJSON(ctx.Stdout, ctx.Stderr, ctx.Flags.jsonOutput, sc); handled {
		return code
	}
	fmt.Fprintf(ctx.Stdout, "schedule %q updated:\n", id)
	printSchedule(ctx.Stdout, *sc)
	return ExitOK
}

// runSchedulesDelete implements `marbor schedules delete <id>`.
func runSchedulesDelete(flags *globalFlags, id string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.DeleteSchedule(id); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "id": id, "deleted": true}); handled {
		return code
	}
	fmt.Fprintf(stdout, "schedule %q deleted\n", id)
	return ExitOK
}
