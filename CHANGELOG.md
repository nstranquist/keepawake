# Changelog

## [0.1.1] - 2026-07-31

- Preserve the pre-existing `SleepDisabled` setting instead of overwriting it on shutdown.
- Fail closed when `pmset -g` output is missing or has an invalid `SleepDisabled` value.
- Add regression coverage for legacy records and external power-setting ownership.

## [0.1.0] - 2026-07-31

- Initial public release of the macOS `keepawake` CLI.
- Includes combined power/process status, stale-PID protection, locking, and rollback handling.
