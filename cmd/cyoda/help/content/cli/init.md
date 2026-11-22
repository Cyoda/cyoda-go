---
topic: cli.init
title: "cyoda init — first-run bootstrap"
stability: stable
see_also:
  - config
  - cli.migrate
---

# cli.init

## NAME

cli.init — write a starter user config enabling sqlite.

## SYNOPSIS

`cyoda init [--force]`

## DESCRIPTION

`cyoda init` is the recommended first step for local desktop use. It writes a minimal user config file that sets `CYODA_STORAGE_BACKEND=sqlite`, enabling persistent local storage without requiring a database server.

The user config file is written to the OS-appropriate XDG config path:

- **Linux / macOS:** `$XDG_CONFIG_HOME/cyoda/cyoda.env` (falls back to `~/.config/cyoda/cyoda.env` if `XDG_CONFIG_HOME` is unset).
- **Windows:** `%AppData%\cyoda\cyoda.env` (falls back to `%USERPROFILE%\AppData\Roaming\cyoda\cyoda.env`).

The sqlite database itself defaults to the OS data directory:

- **Linux / macOS:** `$XDG_DATA_HOME/cyoda/cyoda.db` (falls back to `~/.local/share/cyoda/cyoda.db`).
- **Windows:** `%LocalAppData%\cyoda\cyoda.db`.

The database path can be overridden by setting `CYODA_SQLITE_PATH` in the config or shell environment.

**Idempotency:** `cyoda init` is safe to run more than once. Without `--force`, it exits 0 immediately if a config already exists (either system-wide or at the user path). It does not modify, migrate, or overwrite existing configs.

**System config detection:** On Linux, if `/etc/cyoda/cyoda.env` exists, `cyoda init` prints a note and exits 0 without writing a user config — a system-wide config already covers the system. Pass `--force` to write a user config anyway.

## OPTIONS

- `--force` — Overwrite an existing user config, or write a user config even when a system config is present.

## EXIT CODES

- `0` — Success, including the no-op case (config already exists), or `-h` / `--help` (the usage goes to stderr).
- `1` — I/O error (cannot compute user path, cannot create directory, cannot write file).
- `2` — Flag-parse error, or an argument `init` does not take. Nothing is written.

## EXAMPLES

```
# First-run bootstrap: write ~/.config/cyoda/cyoda.env
cyoda init

# Overwrite an existing user config
cyoda init --force

# Typical first-run sequence
cyoda init && cyoda
```

## SEE ALSO

- config
- cli.migrate
