# Contributing

Thanks for taking an interest in `keepawake`.

Before opening a pull request:

1. Keep changes focused and explain the user-visible behavior.
2. Run `make verify` on macOS.
3. Update the README when commands, safety behavior, or installation changes.
4. Include tests for lifecycle and process-identity changes.

This project is macOS-specific. Changes that touch `pmset`, `caffeinate`, PID handling, locking, or rollback should explain the failure mode they protect against.
