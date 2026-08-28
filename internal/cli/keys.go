package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

func printKeyUsage(w io.Writer) { writeHelp(w, findCommand(root(), "key")) }

func runKeySetLocalOnly(flags *globalFlags, name, value string, stdout, stderr io.Writer) int {
	var localOnly bool
	switch value {
	case "true":
		localOnly = true
	case "false":
		localOnly = false
	default:
		fmt.Fprintf(stderr, "invalid value %q for local_only (want true or false)\n", value)
		return ExitUserError
	}
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.PatchKeyLocalOnly(name, localOnly); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "key": name, "local_only": localOnly}); handled {
		return code
	}
	fmt.Fprintf(stdout, "key %q local_only=%v\n", name, localOnly)
	return ExitOK
}

func runKeySetAllowLocalDegradation(flags *globalFlags, name, value string, stdout, stderr io.Writer) int {
	var allow bool
	switch value {
	case "true":
		allow = true
	case "false":
		allow = false
	default:
		fmt.Fprintf(stderr, "invalid value %q for allow_local_degradation (want true or false)\n", value)
		return ExitUserError
	}
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.PatchKeyAllowLocalDegradation(name, allow); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "key": name, "allow_local_degradation": allow}); handled {
		return code
	}
	fmt.Fprintf(stdout, "key %q allow_local_degradation=%v\n", name, allow)
	return ExitOK
}

func runKeyList(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	keys, err := client.ListKeys()
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, keys); handled {
		return code
	}
	tw := newTabWriter(stdout)
	fmt.Fprintln(tw, "NAME\tKEY\tRATE\tDAILY\tMONTHLY\tMODELS\tEXPIRES\tLOCAL_ONLY\tDEGRADATION\tSTATUS")
	for _, k := range keys {
		models := "-"
		if len(k.AllowedModels) > 0 {
			models = strings.Join(k.AllowedModels, ",")
		}
		expires := k.ExpiresAt
		if expires == "" {
			expires = "-"
		}
		keyPreview := k.Key
		if keyPreview == "" {
			keyPreview = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\t%s\t%s\n", k.Name, keyPreview, k.RateLimit, k.RequestsToday, k.RequestsThisMonth, models, expires, yesNo(k.LocalOnly), yesNo(k.AllowLocalDegradation), k.Status)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	return ExitOK
}

func runKeyCreate(flags *globalFlags, stdout, stderr io.Writer, ctx *RunCtx) int {
	name := ctx.String("name")
	if name == "" {
		fmt.Fprintln(stderr, "error: --name is required")
		return ExitUserError
	}
	req := KeyCreateRequest{Name: name}
	if ctx.IsSet("rate-limit") {
		req.RateLimit = ctx.Int("rate-limit")
	}
	if ctx.IsSet("daily-limit") {
		req.DailyLimit = ctx.Int("daily-limit")
	}
	if ctx.IsSet("monthly-limit") {
		req.MonthlyLimit = ctx.Int("monthly-limit")
	}
	if ctx.IsSet("daily-usd-cap") {
		v := ctx.String("daily-usd-cap")
		if v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				fmt.Fprintf(stderr, "invalid --daily-usd-cap %q: %v\n", v, err)
				return ExitUserError
			}
			req.DailyUsdCap = f
		}
	}
	if ctx.IsSet("monthly-usd-cap") {
		v := ctx.String("monthly-usd-cap")
		if v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				fmt.Fprintf(stderr, "invalid --monthly-usd-cap %q: %v\n", v, err)
				return ExitUserError
			}
			req.MonthlyUsdCap = f
		}
	}
	if ctx.IsSet("models") {
		raw := ctx.String("models")
		if raw != "" {
			parts := strings.Split(raw, ",")
			var out []string
			for _, p := range parts {
				if s := strings.TrimSpace(p); s != "" {
					out = append(out, s)
				}
			}
			req.Models = out
		}
	}
	if ctx.IsSet("expires-at") {
		req.ExpiresAt = ctx.String("expires-at")
	}
	if ctx.IsSet("key") {
		req.Key = ctx.String("key")
	}
	if ctx.IsSet("local-only") {
		v := ctx.String("local-only")
		if v == "true" {
			req.LocalOnly = true
		} else if v == "false" {
			req.LocalOnly = false
		} else {
			fmt.Fprintf(stderr, "invalid --local-only %q (want true or false)\n", v)
			return ExitUserError
		}
	}
	if ctx.IsSet("allow-local-degradation") {
		v := ctx.String("allow-local-degradation")
		if v == "true" {
			req.AllowLocalDegradation = true
		} else if v == "false" {
			req.AllowLocalDegradation = false
		} else {
			fmt.Fprintf(stderr, "invalid --allow-local-degradation %q (want true or false)\n", v)
			return ExitUserError
		}
	}
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	created, err := client.CreateKey(req)
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, created); handled {
		return code
	}
	fmt.Fprintf(stdout, "key %q created", name)
	if created != nil && created.Key != "" {
		fmt.Fprintf(stdout, "; store this key now - it will not be shown again\n  %s\n", created.Key)
	} else {
		fmt.Fprintln(stdout)
	}
	return ExitOK
}

func runKeyRevoke(flags *globalFlags, name string, stdout, stderr io.Writer, ctx *RunCtx) int {
	yes := false
	if ctx != nil {
		yes = ctx.Bool("yes")
	}
	if err := requireConfirm("revoke", name, yes, stderr); err != nil {
		return reportError(err, stderr)
	}
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.RevokeKey(name); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "key": name, "revoked": true}); handled {
		return code
	}
	fmt.Fprintf(stdout, "key %q revoked\n", name)
	return ExitOK
}

func runKeyPatch(flags *globalFlags, name string, stdout, stderr io.Writer, ctx *RunCtx) int {
	var patch KeyPatch
	hasField := false
	parseIntPtr := func(s string) (*int, error) {
		if s == "" {
			return nil, fmt.Errorf("empty value")
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, err
		}
		return &n, nil
	}
	parseFloatPtr := func(s string) (*float64, error) {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, err
		}
		return &f, nil
	}
	parseBoolPtr := func(s string) (*bool, error) {
		if s == "true" {
			b := true
			return &b, nil
		}
		if s == "false" {
			b := false
			return &b, nil
		}
		return nil, fmt.Errorf("want true or false, got %q", s)
	}
	if ctx.IsSet("rate-limit") {
		v, err := parseIntPtr(ctx.String("rate-limit"))
		if err != nil {
			fmt.Fprintf(stderr, "invalid --rate-limit %q: %v\n", ctx.String("rate-limit"), err)
			return ExitUserError
		}
		patch.RateLimit = v
		hasField = true
	}
	if ctx.IsSet("daily-limit") {
		v, err := parseIntPtr(ctx.String("daily-limit"))
		if err != nil {
			fmt.Fprintf(stderr, "invalid --daily-limit %q: %v\n", ctx.String("daily-limit"), err)
			return ExitUserError
		}
		patch.DailyLimit = v
		hasField = true
	}
	if ctx.IsSet("monthly-limit") {
		v, err := parseIntPtr(ctx.String("monthly-limit"))
		if err != nil {
			fmt.Fprintf(stderr, "invalid --monthly-limit %q: %v\n", ctx.String("monthly-limit"), err)
			return ExitUserError
		}
		patch.MonthlyLimit = v
		hasField = true
	}
	if ctx.IsSet("daily-usd-cap") {
		v, err := parseFloatPtr(ctx.String("daily-usd-cap"))
		if err != nil {
			fmt.Fprintf(stderr, "invalid --daily-usd-cap %q: %v\n", ctx.String("daily-usd-cap"), err)
			return ExitUserError
		}
		patch.DailyUsdCap = v
		hasField = true
	}
	if ctx.IsSet("monthly-usd-cap") {
		v, err := parseFloatPtr(ctx.String("monthly-usd-cap"))
		if err != nil {
			fmt.Fprintf(stderr, "invalid --monthly-usd-cap %q: %v\n", ctx.String("monthly-usd-cap"), err)
			return ExitUserError
		}
		patch.MonthlyUsdCap = v
		hasField = true
	}
	if ctx.IsSet("models") {
		raw := ctx.String("models")
		if raw == "" {
			empty := []string{}
			patch.Models = &empty
		} else {
			parts := strings.Split(raw, ",")
			var out []string
			for _, p := range parts {
				if s := strings.TrimSpace(p); s != "" {
					out = append(out, s)
				}
			}
			patch.Models = &out
		}
		hasField = true
	}
	if ctx.IsSet("expires-at") {
		s := ctx.String("expires-at")
		patch.ExpiresAt = &s
		hasField = true
	}
	if ctx.IsSet("local-only") {
		b, err := parseBoolPtr(ctx.String("local-only"))
		if err != nil {
			fmt.Fprintf(stderr, "invalid --local-only %q (want true or false)\n", ctx.String("local-only"))
			return ExitUserError
		}
		patch.LocalOnly = b
		hasField = true
	}
	if ctx.IsSet("allow-local-degradation") {
		b, err := parseBoolPtr(ctx.String("allow-local-degradation"))
		if err != nil {
			fmt.Fprintf(stderr, "invalid --allow-local-degradation %q (want true or false)\n", ctx.String("allow-local-degradation"))
			return ExitUserError
		}
		patch.AllowLocalDegradation = b
		hasField = true
	}
	if !hasField {
		fmt.Fprintln(stderr, "error: no patch fields supplied (pass at least one --flag)")
		return ExitUserError
	}
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.PatchKey(name, patch); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "key": name, "updated": true}); handled {
		return code
	}
	fmt.Fprintf(stdout, "key %q updated\n", name)
	return ExitOK
}

func runSpill(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	rows, err := client.SpillCounters()
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, rows); handled {
		return code
	}
	tw := newTabWriter(stdout)
	fmt.Fprintln(tw, "KEY\tSERVED BY\tREQUESTS")
	for _, row := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%d\n", row.KeyName, row.ServedBy, row.Requests)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	return ExitOK
}
