---
title: First hour
---

# First hour

```sh
keepawake           # read-only status (ON, OFF, or PARTIAL)
keepawake status    # same as bare keepawake
keepawake on        # closed-lid awake
keepawake off       # restore normal lid-close sleep and stop caffeinate
```

`status` (and bare `keepawake`) is read-only. `on` and `off` may call `sudo pmset`.

`keepawake off` always restores `pmset -a disablesleep 0`, verifies the setting took effect, stops any verified managed `caffeinate`, and removes the PID record.

`PARTIAL` means the system setting and the caffeinate process disagree. Run `keepawake off` to restore normal sleep, or `keepawake repair` to resume ON.
