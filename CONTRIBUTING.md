# Contributing to Ollama Mesh

First off, thank you for considering contributing to Ollama Mesh!

## Local Development

To set up the project locally:

1. **Build the full project:**
   ```bash
   make build
   ./ollama-mesh
   ```

2. **Run tests:**
   ```bash
   go test ./...
   ```

3. **Run UI in dev mode:**
   If you are making changes to the React frontend, you can run the Vite dev server:
   ```bash
   make dev-ui
   ```

## Pull Request Guidelines

- **One feature per PR:** Please keep your pull requests focused on a single feature or bug fix to make reviewing easier.
- **Tests required:** Ensure all existing tests pass (`go test ./...`) and add new tests for any new functionality.
- **Update documentation:** If your PR changes API endpoints or configuration options, please update `README.md` and `config.example.yaml`.
