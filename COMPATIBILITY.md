# Configuration compatibility

## Supported settings

- Build the runtime with `go build -o fathom ./cmd/fathom`.
- Fathom discovers `fathom.config.yaml` and `fathom.policy.yaml`, with fallback
  to the corresponding `fantazm.*.yaml` files in each searched directory.
  Global config discovery also accepts both names under `~/.config/` and
  the legacy home dot directories. An explicit `--config` path still works.
- Go runtime settings accept `FATHOM_*` environment variables first and
  fall back to their `FANTAZM_*` equivalents. An explicitly empty FATHOM
  variable overrides the old name. Bundled skill subprocesses continue to
  use their documented `FANTAZM_*` protocol variables.
- Existing `.fantazm` data paths, the vault keychain service, installed skill
  directory, launchd service identifier, and wire protocol names are retained
  for compatibility. User data stays in its configured location.
- To update an installed background service to the new executable path,
  run `fathom service install` from its existing project directory after
  installing the binary in a permanent location.
- Recall continues to use the `recall` command and its existing data directory.

