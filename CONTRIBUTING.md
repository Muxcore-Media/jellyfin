# Contributing

## Development Setup

1. Go 1.26+
2. Clone `core` and this module as siblings (`go.mod` replace paths expect `../core`)
3. `make test` — unit tests
4. `make lint` — golangci-lint
5. `make build` — binary name comes from Makefile `BINARY` (currently `jellyfin`)

### Run against a local muxcored

```bash
# Terminal 1: start core in dev mode
cd ../core
MUXCORE_INSECURE_DISABLE_TLS=true ./muxcored

# Terminal 2: start module
make build
MUXCORE_INSECURE_DISABLE_TLS=true MUXCORE_GRPC_ADDR=localhost:9090 ./jellyfin
# or: ./jellyfin --muxcore-mesh-addr localhost:9090
```

## Code Conventions

- No comments explaining what the code does — name things well instead.
- Comments only for non-obvious WHY — hidden invariants, workarounds.
- No `os.Exit` from library code — only `main` exits.
- Structured logging via `log/slog` — no `fmt.Println` in non-test code.
- Context propagation — every function that does I/O takes `ctx context.Context` as its first argument.
- Error wrapping — use `fmt.Errorf("operation %q: %w", name, err)`.

## Branch Naming

```
feat/<short-description>
fix/<short-description>
docs/<short-description>
refactor/<short-description>
```

## Pull Request Process

1. Branch from `main`
2. Run `make ci` locally — it must pass
3. Open a PR against `main`
4. Squash-merge preferred

## Security Vulnerabilities

Do **not** open a public issue. See [SECURITY.md](SECURITY.md) for the private reporting process.

## License

By contributing, you agree that your contributions will be licensed under the GPL-3.0 license.
