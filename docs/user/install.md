---
title: Install
---

# Install

`keepawake` keeps a Mac running with the lid closed. It combines `pmset disablesleep` with a managed `caffeinate -i`.

```sh
cd ~/tools/keepawake
make install
export PATH="$HOME/.local/bin:$PATH"
keepawake status
```

The binary lands at `~/.local/bin/keepawake`. macOS will ask for administrator approval when a command changes sleep policy.
