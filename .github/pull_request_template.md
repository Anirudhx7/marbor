## What does this PR do?

<!-- One or two sentences. Link the issue if one exists. -->

## Checklist

- [ ] `go test ./...` passes
- [ ] New code has tests (happy path + one failure case)
- [ ] No fake/mock/random data in production paths
- [ ] `config.example.yaml` updated if config fields changed
- [ ] Streaming stays unbuffered if `proxy.go` was touched
