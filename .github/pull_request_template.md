## What does this PR do?

<!-- One or two sentences. Link the issue if one exists. -->

## Checklist

- [ ] All commits are signed off (DCO) - `git commit -s`
- [ ] `go test ./...` passes
- [ ] New code has tests (happy path + one failure case)
- [ ] No fake/mock/random data in production paths
- [ ] New settings fields wired DB-first: default in `config.go Validate()`, `settings` KV entry, admin API + UI control (see CONTRIBUTING.md)
- [ ] Streaming stays unbuffered if `proxy.go` was touched
