package runtime

import (
	"context"
	"fmt"
	"net/http"
)

// checkHealth performs GET {nodeURL}/health and returns an error if the
// response status is not 200 OK. Used by vLLM, TGI, and llama.cpp probes.
func checkHealth(ctx context.Context, client *http.Client, nodeURL string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", nodeURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("build health request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("health request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/health returned %d", resp.StatusCode)
	}
	return nil
}
