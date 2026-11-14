package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The permanent agent token must never be printed by `agent enable` or
// `agent regenerate`, in text or --json output; operators use the install
// command, which carries a short-lived enrollment code instead.
func TestAgentEnableRegenerate_NeverPrintTokenValue(t *testing.T) {
	const secret = "permanent-agent-token-value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"node":"gpu1","enabled":true,"port":9200,"scheme":"http","token":"` + secret +
			`","install_command":"curl enroll-unix","install_command_windows":"iwr enroll-win"}`))
	}))
	defer srv.Close()

	runs := map[string]func(*globalFlags, *bytes.Buffer, *bytes.Buffer) int{
		"enable": func(f *globalFlags, o, e *bytes.Buffer) int {
			return runAgentEnable(f, "gpu1", 9200, "", o, e)
		},
		"regenerate": func(f *globalFlags, o, e *bytes.Buffer) int {
			return runAgentRegenerate(f, "gpu1", o, e)
		},
	}
	for name, run := range runs {
		for _, asJSON := range []bool{false, true} {
			withTempConfigDir(t)
			mustSaveSession(t, srv.URL, "saved-tok")
			var out, errb bytes.Buffer
			code := run(&globalFlags{server: srv.URL, jsonOutput: asJSON}, &out, &errb)
			if code != ExitOK {
				t.Fatalf("%s json=%v: exit %d stderr=%s", name, asJSON, code, errb.String())
			}
			all := out.String() + errb.String()
			if strings.Contains(all, secret) {
				t.Errorf("%s json=%v: output leaks the agent token: %s", name, asJSON, all)
			}
			if asJSON {
				var obj map[string]any
				if err := json.Unmarshal(out.Bytes(), &obj); err != nil {
					t.Fatalf("%s: --json output is not valid JSON: %v\n%s", name, err, out.String())
				}
				if _, has := obj["token"]; has {
					t.Errorf("%s: --json must not carry a token key: %v", name, obj)
				}
				if obj["install_command"] != "curl enroll-unix" || obj["install_command_windows"] != "iwr enroll-win" {
					t.Errorf("%s: install_command fields missing: %v", name, obj)
				}
			}
			if !strings.Contains(all, "curl enroll-unix") || !strings.Contains(all, "iwr enroll-win") {
				t.Errorf("%s json=%v: install commands missing: %s", name, asJSON, all)
			}
		}
	}
}
