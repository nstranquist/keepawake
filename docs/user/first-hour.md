---
title: First hour
---

# First hour

```sh
keepawake status    # ON, OFF, or PARTIAL
keepawake on        # closed-lid awake
keepawake off       # restore the prior sleep policy
```

`status` is read-only. `on` and `off` may call `sudo pmset`.

When keepawake starts, it records the previous `SleepDisabled` value. `off` restores that value. It does not assume sleep was enabled before.

`PARTIAL` means the system setting and the caffeinate process disagree. Run `keepawake repair` toward ON, or `off` to clear both.
