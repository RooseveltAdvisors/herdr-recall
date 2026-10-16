# herdr-recall

A Superhuman-style agent triage inbox and pane recall picker for [Herdr](https://herdr.dev).
When you switch across 20 to 30 live agent threads to give quick feedback and unblock work, `herdr-recall` keeps track of which threads need a cycle-back, lets you leave sticky notes and reminders, and jumps straight to any pane from `prefix+k`.

See [VISION.md](VISION.md) for the project vision and acceptance policy.

Press `prefix+k` and the picker opens as a 95% by 95% popup on `Inbox`.
`Inbox`, `All`, and `Favorites` are the built-in tabs.
`Recent` and `Most used` start as custom tabs you can delete with `x` (it asks you to confirm), revise with `r`, or create from scratch with `c`.

In `Inbox`, press `e` to mark a thread Done for the current cycle.
It stays out of `Inbox` until its agent transitions into `blocked` or `done`, starts `working` on a new prompt, or a scheduled reminder fires.
Press `z` to snooze a thread (`20m`, `1h`, `3h`, `Tomorrow 9am`, or `Clear`).
Press `n` to leave a one-line sticky note beside the tab name.
Press `!` to toggle a manual `blocked` override when a thread is waiting on something outside Herdr's detector.

## Keys

| Key | Action |
| --- | ------ |
| `prefix+k` | open the recall and triage popup |
| `j` / `k` / `↑` / `↓` | move one row (`ctrl-n` / `ctrl-p` and arrows also move while searching) |
| `g` / `G` | jump to the first / last row |
| `ctrl+u` / `ctrl+d` | half-page up / down (in search or note: `ctrl+u` clears text) |
| `h` / `l` / `←` / `→` | switch tabs (`Inbox`, `All`, `Favorites`, `Recent`, `Most used`, ...) |
| `enter` | focus the selected pane directly via Herdr socket |
| `e` | mark the selected thread Done (or move an already-done thread back to `Inbox`) |
| `z` | set or clear a reminder (`20m`, `1h`, `3h`, `Tomorrow 9am`, `Clear`) |
| `n` | set or edit a sticky note on the selected thread |
| `!` | toggle manual `blocked` override on the selected thread |
| `m` | mark or unmark the selected thread as a favorite (`*`) |
| `b` / `w` / `i` / `d` / `a` | filter by status: blocked, working, idle, done, or all |
| `/` | search tab names and sticky notes (`esc` leaves search, second `esc` closes) |
| `c` / `r` / `x` | create, revise, or delete a custom tab |
| `s` | cycle sort (`Most recent`, `Oldest`, `Most used`, `Least used`) on `All` and `Favorites` |
| `?` | open or close help |
| `esc` / `q` | leave modal/search, then close the picker |

The footer shows the keys for the mode you are in: key names in accent,
hint words dim, the same way `prefix+k` does. Status appears as a coloured
dot plus a coloured word (red blocked, yellow working, teal done, green
idle), section headers are coloured, and the favourite star is yellow.

## Screenshots

Inbox triage view (default view with pinned queue order, sticky notes, and due `!` reminder badge):

![inbox triage view](assets/screenshot-browse.png)

All view showing all four status colours (red blocked, yellow working, teal done, green idle) and a snoozed `~` reminder badge:

![all view with status colours](assets/screenshot-status-colours.png)

Favorites view (`[sort: Starred]`):

![favorites](assets/screenshot-favorites.png)

Recent view (`[sort: Most recent]`):

![recency](assets/screenshot-recency.png)

Most used view (`[sort: Most used]`):

![most used](assets/screenshot-most-used.png)

Setting a reminder after pressing `z` on a row:

![reminder modal](assets/screenshot-favorite-toggle.png)

Search mode after pressing `/` with a typed query matching a sticky note:

![search mode](assets/screenshot-search.png)

The picker draws to the width of the pane it opens in, measured from its own
tty, so a row never wraps even in a narrow overlay.

## Install

Needs Go 1.26+ and Herdr 0.8.0 or newer.

```sh
herdr plugin install RooseveltAdvisors/herdr-recall --ref main --yes
herdr server reload-config
```

Herdr keeps the installed copy in its own plugin folder. Do not clone the
repo into `~/code` and link that path. A missing checkout makes the keybinding
fail or open an old copy.

Then bind it in `~/.config/herdr/config.toml`:

```toml
[[keys.command]]
key = "prefix+k"
type = "plugin_action"
command = "RooseveltAdvisors.herdr-recall.open-picker"
description = "recall picker"
```

and run `herdr server reload-config`.

## Where state lives

`herdr plugin config-dir RooseveltAdvisors.herdr-recall` prints the directory;
the state file is `recall.json` inside it. It records, per pane: visit count,
last-seen time, and the label shown in the list, plus the favorites list.
Old non-favorite entries are pruned past 200 panes.

## How jumping works

Enter sends `pane.focus` directly over `HERDR_SOCKET_PATH` for the selected live pane.

## Companion CLI

- `herdr-recall stats` - print the current state JSON
- `herdr-recall record` - record the focused pane now
- `herdr-recall jump <pane-id>` - jump without the picker
- `herdr-recall favorite <pane-id>` - toggle a favorite
- `herdr-recall render` - print one picker frame (honours `RECALL_QUERY`,
  `RECALL_SEL`, `RECALL_W`, `RECALL_H`, `RECALL_STATUS`); used for tests and
  screenshots
