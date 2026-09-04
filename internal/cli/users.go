package cli

import (
	"fmt"
	"io"
	"strconv"
)

func runUsersList(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	users, err := client.ListUsers()
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, users); handled {
		return code
	}
	tw := newTabWriter(stdout)
	fmt.Fprintln(tw, "ID\tUSERNAME\tEMAIL\tROLE\tSTATUS\tAPI_KEY\tCREATED")
	for _, u := range users {
		email := u.Email
		if email == "" {
			email = "-"
		}
		apiKey := u.APIKeyName
		if apiKey == "" {
			apiKey = "-"
		}
		created := u.CreatedAt
		if created == "" {
			created = "-"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", u.ID, u.Username, email, u.Role, u.Status, apiKey, created)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	return ExitOK
}

func runUsersCreate(flags *globalFlags, stdout, stderr io.Writer, ctx *RunCtx) int {
	username := ctx.String("user")
	if username == "" {
		fmt.Fprintln(stderr, "error: --user is required")
		return ExitUserError
	}
	req := CreateUserRequest{Username: username}
	if ctx.IsSet("email") {
		req.Email = ctx.String("email")
	}
	role := ctx.String("role")
	if role != "" && role != "admin" && role != "user" {
		fmt.Fprintf(stderr, "invalid --role %q (want admin or user)\n", role)
		return ExitUserError
	}
	req.Role = role
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	resp, err := client.CreateUser(req)
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, resp); handled {
		return code
	}
	fmt.Fprintf(stdout, "user %q (id %d) created", resp.Username, resp.ID)
	if resp.InitialPassword != "" {
		fmt.Fprintf(stdout, "; password: %s - store it now, it will not be shown again\n", resp.InitialPassword)
	} else {
		fmt.Fprintln(stdout)
	}
	return ExitOK
}

func runUsersApprove(flags *globalFlags, idStr string, stdout, stderr io.Writer, ctx *RunCtx) int {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		fmt.Fprintf(stderr, "invalid user id %q\n", idStr)
		return ExitUserError
	}
	req := ApproveUserRequest{}
	if ctx.IsSet("api-key-name") {
		req.APIKeyName = ctx.String("api-key-name")
	}
	if ctx.Bool("create-key") {
		ck := &struct {
			Name         string   `json:"name,omitempty"`
			RateLimit    int      `json:"rate_limit_per_hour,omitempty"`
			DailyLimit   int      `json:"daily_limit,omitempty"`
			MonthlyLimit int      `json:"monthly_limit,omitempty"`
			Models       []string `json:"models,omitempty"`
		}{}
		if req.APIKeyName != "" {
			ck.Name = req.APIKeyName
		}
		if ctx.IsSet("key-rate-limit") {
			ck.RateLimit = ctx.Int("key-rate-limit")
		}
		if ctx.IsSet("key-daily-limit") {
			ck.DailyLimit = ctx.Int("key-daily-limit")
		}
		if ctx.IsSet("key-monthly-limit") {
			ck.MonthlyLimit = ctx.Int("key-monthly-limit")
		}
		if ctx.IsSet("key-models") {
			ck.Models = parseCommaList(ctx.String("key-models"))
		}
		req.CreateKey = ck
	}
	client, err2 := authenticatedClient(flags)
	if err2 != nil {
		return reportError(err2, stderr)
	}
	resp, err2 := client.ApproveUser(id, req)
	if err2 != nil {
		return reportError(err2, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, resp); handled {
		return code
	}
	fmt.Fprintf(stdout, "user %d approved", id)
	if resp.APIKeyValue != "" {
		fmt.Fprintf(stdout, "; api key: %s - store it now\n", resp.APIKeyValue)
	} else {
		fmt.Fprintln(stdout)
	}
	return ExitOK
}

func runUsersSuspend(flags *globalFlags, idStr string, stdout, stderr io.Writer, ctx *RunCtx) int {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		fmt.Fprintf(stderr, "invalid user id %q\n", idStr)
		return ExitUserError
	}
	yes := false
	if ctx != nil {
		yes = ctx.Bool("yes")
	}
	if err := requireConfirm("suspend", idStr, yes, stderr); err != nil {
		return reportError(err, stderr)
	}
	client, err2 := authenticatedClient(flags)
	if err2 != nil {
		return reportError(err2, stderr)
	}
	if err := client.SuspendUser(id); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "id": id, "status": "suspended"}); handled {
		return code
	}
	fmt.Fprintf(stdout, "user %d suspended (sessions revoked)\n", id)
	return ExitOK
}

func runUsersResetPassword(flags *globalFlags, idStr string, stdout, stderr io.Writer) int {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		fmt.Fprintf(stderr, "invalid user id %q\n", idStr)
		return ExitUserError
	}
	client, err2 := authenticatedClient(flags)
	if err2 != nil {
		return reportError(err2, stderr)
	}
	pw, err2 := client.ResetUserPassword(id)
	if err2 != nil {
		return reportError(err2, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"id": id, "password": pw}); handled {
		return code
	}
	fmt.Fprintf(stdout, "user %d password reset; new password: %s - store it now\n", id, pw)
	return ExitOK
}

func runUsersPatch(flags *globalFlags, idStr string, stdout, stderr io.Writer, ctx *RunCtx) int {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		fmt.Fprintf(stderr, "invalid user id %q\n", idStr)
		return ExitUserError
	}
	var req PatchUserRequest
	hasField := false
	if ctx.IsSet("email") {
		v := ctx.String("email")
		req.Email = &v
		hasField = true
	}
	if ctx.IsSet("role") {
		v := ctx.String("role")
		if v != "admin" && v != "user" {
			fmt.Fprintf(stderr, "invalid --role %q (want admin or user)\n", v)
			return ExitUserError
		}
		req.Role = &v
		hasField = true
	}
	if !hasField {
		fmt.Fprintln(stderr, "error: no patch fields supplied (pass --email or --role)")
		return ExitUserError
	}
	client, err2 := authenticatedClient(flags)
	if err2 != nil {
		return reportError(err2, stderr)
	}
	updated, err2 := client.PatchUser(id, req)
	if err2 != nil {
		return reportError(err2, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, updated); handled {
		return code
	}
	fmt.Fprintf(stdout, "user %d updated\n", id)
	return ExitOK
}

func runUsersDelete(flags *globalFlags, idStr string, stdout, stderr io.Writer, ctx *RunCtx) int {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		fmt.Fprintf(stderr, "invalid user id %q\n", idStr)
		return ExitUserError
	}
	yes := false
	if ctx != nil {
		yes = ctx.Bool("yes")
	}
	if err := requireConfirm("delete", idStr, yes, stderr); err != nil {
		return reportError(err, stderr)
	}
	client, err2 := authenticatedClient(flags)
	if err2 != nil {
		return reportError(err2, stderr)
	}
	if err := client.DeleteUser(id); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "id": id, "deleted": true}); handled {
		return code
	}
	fmt.Fprintf(stdout, "user %d deleted\n", id)
	return ExitOK
}
