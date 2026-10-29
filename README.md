# herdr-recall

![herdr-recall hero](assets/hero.png)

A Superhuman-style agent triage inbox and pane recall picker for [Herdr](https://herdr.dev).
When you switch across 20 to 30 live agent threads to give quick feedback and unblock work, `herdr-recall` keeps track of which threads need a cycle-back, lets you leave sticky notes and reminders, and jumps straight to any pane from `prefix+k`.

See [VISION.md](VISION.md) for the project vision and acceptance policy.

Press `prefix+k` and the picker opens as a 95% by 95% popup.
Tabs from left to right are `Favorites`, `Inbox`, `Later`, `Recent`, `Most used`, and `All`.
`Transcripts` is added only when `ccrider` is installed.
`Recent` and `Most used` start as custom tabs. `t c` creates one, `t r` revises it, and `t x` deletes it after a confirmation.

In `Inbox`, press `e` to mark a thread Done for the current cycle.
Done is not mute: any new agent activity brings the thread back to `Inbox`.
The row leaves the list you are looking at, on every tab, the way Superhuman
archives out of whatever view you are triaging. On the reference tabs the
limit is applied after done rows are hidden, so a limit of 50 always shows 50
live rows instead of losing slots to hidden ones. `z` undoes the last `e` and
the row returns to the top of the list. A thread also returns on its own when
its agent does something new. A row with a reminder shows the next time
beside the name, such as `in 20m`, `3:04 PM`, or `tmr 9:00 AM`.
An overdue reminder says `overdue 3m` and rises to the top of Inbox.
The other rows keep their order. Panes with no reminder keep the same columns.
Viewing a pane is you reading, not the agent moving, so it never returns a thread and never moves the pinned queue.
Press `e` again on that row to undo.
Press `h` to set a reminder (`20m`, `1h`, `3h`, `tomorrow`, or `clear`).
If one is already set, `h` shows that time. Choose Update or Cancel.
`x` on that choice also cancels it.
The return rule is `If no reply` or `Regardless`.
Press `n` to leave a one-line sticky note beside the tab name.
Press `!` to toggle a manual `blocked` override when a thread is waiting on something outside Herdr's detector.

## Keys

| Key | Action |
| --- | ------ |
| `prefix+k` | open the recall and triage popup |
| `j` / `k` / `↑` / `↓` | move one row, or N rows with a count such as `3j` |
| `gg` / `G` | jump to the first / last row |
| `g` then `i` / `l` / `t` / `r` / `f` / `m` / `a` | jump to Inbox, Later, Transcripts, Recent, Favorites, Most used, All |
| Transcripts `/` | search as you type. Default is your words. `tab` or `r` switches to agent, then all |
| `←` / `→` | switch tabs. `h` and `l` do not |
| `ctrl+u` / `ctrl+d` | half-page up / down (in search or note: `ctrl+u` clears text) |
| `enter` | focus the selected pane directly via Herdr socket |
| `e` | mark done. The row leaves the current list. `z` undoes |
| `h` | set, change, or cancel a reminder. Shows the current time when one is set |
| `z` | undo the last `e`. `u` does the same |
| `n` | set or edit a sticky note on the selected thread |
| `!` | toggle manual `blocked` override on the selected thread |
| `m` | mark or unmark the selected thread as a favorite (`*`) |
| `i` | teach Jev: prefer or demote, check feature chips, or type a rule |
| `J` | inspect and correct Jev learned feature weights |
| `b` / `w` / `i` / `d` / `a` | filter by status: blocked, working, idle, done, or all |
| `/` | search tab names and sticky notes, with vim editing |
| `t` then `c` / `r` / `x` | create, revise, or delete a custom tab |
| `s` | cycle sort (`Most recent`, `Oldest`, `Most used`, `Least used`) on `All` and `Favorites` |
| `?` | open or close help |
| `esc` | insert to normal, normal to browse, then clears the search words, then the status filter, then closes |
| `ctrl+c` | close from anywhere |

The footer shows the keys for the mode you are in: key names in accent,
hint words dim, the same way `prefix+k` does. Status appears as a coloured
dot plus a coloured word (red blocked, yellow working, teal done, green
idle), section headers are coloured, and the favourite star is yellow.

## Vim editing

`/` search and `n` sticky note share one vim line editor, so both feel the same.

- `esc` or `jj` enters normal mode. `i`, `I`, `a`, `A` go back to insert.
- Motions: `w` `W` `b` `B` `e` `E`, `0` `^` `$`, `f` `F` `t` `T`, `;` `,`.
- Edits: `x` `X`, `r{char}`, `s`, `d` `c` `y` with a motion, `D`, `C`, `Y`.
- `p` `P` paste, `u` undo, `ctrl+r` redo, `~` swap case, `.` repeat.
- Counts work, so `3w`, `2dw`, `5x` all behave.
- `jj` leaves insert mode, with a 150 ms window before a lone `j` lands.
- `esc` unwinds one thing per press: insert to normal, normal to browse with
  the filter kept, then the search words clear, then the status filter, then
  the picker closes.
- While a field is focused, `j` `k` and the arrows move the list and `enter` jumps.

A mode chip sits left of the prompt: `N` normal, `I` insert, `R` replace, and
`d` `c` `y` `f` `r` when a command is waiting on its motion. The cursor is
painted in the line, so it matches the screenshots exactly.

## Remind times

`z` takes a typed time. Units are `min`, `h`, `d`, `w`, `mo`, `y`, and `m`
means months to match the `3h 3d 2w 2m` scale, so minutes are always `min`.
Compound sequences work (`1d12h`), bare units mean one (`h`), `a` and `an`
mean one, and `tomorrow` is 9 AM. The resolved absolute time is shown while
you type, so an ambiguous unit is visible before it commits. `clear` removes
the reminder.

## Jev triage priority

Jev is optional. With no decision proxy on the box and no `HERDR_RECALL_JEV`
override, the picker behaves exactly as it does without this section.

When Jev is present, each new Inbox arrival is asked one question: should the
captain triage this thread before the others right now. A new arrival can
land at the front. Rows already in the queue keep their order. Pressing `e`
archives the selected row only.

Preference is learned locally and does not need the decision proxy.
Only the `i` teach dialog writes weights. Opening a row does not.
Pressing `e` does not. `e` only archives the selected row.

`i` is the only teach path. Prefer, Neutral, and Demote apply only to the
checked feature chips. Uncheck a chip when that signal should not get the
credit. Free text can name a comparison (`billing over docs`) or a
conditional rule (`demote idle until afternoon`). The raw text is also
stored for later review.

Topic words are not a fixed list. Jev takes them from the tab name, the
sticky note, the workspace name, and the words you type. A new project name
becomes a chip with no code change. Terms that sit on most panes start
unchecked, so path noise does not get the credit.

Press `J` to inspect what Jev has learned. Weights sort by impact,
positive green and negative red. Enter clears the one weight under the cursor.
There is no key that clears every weight.

## Showcase

Designed frames of the picker. Live terminal captures follow below.
The social card is [assets/og.png](assets/og.png).

The first open is Inbox. Later opens restore the tab you left. A coloured dot plus a coloured word marks status:
red blocked, yellow working, teal done, green idle. `e` marks a thread done
for this cycle. A sticky note from `n` sits beside the name.

![triage inbox](assets/showcase-inbox.png)

`i` opens teach mode on the selected thread. Prefer, Neutral, and Demote are
chips. Feature chips sit under them. An optional instruction is saved with
the choice.

![teach Jev](assets/showcase-teach.png)

`J` shows what Jev has learned. Positive weights are teal. Negative weights
are red. The inbox preview says why the top threads rank first.

![Jev dashboard](assets/showcase-jev.png)

`h` sets a reminder with a live preview (`3h` resolves to "in 3 hours"
before it commits). A set reminder can be updated or cancelled there.

![snooze](assets/showcase-snooze.png)

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

Snooze dialog after pressing `z`, with the return rule as a field:

![reminder modal](assets/screenshot-favorite-toggle.png)

Search with the shared vim editor. The chip shows the mode and the cursor is painted in the line:

![search mode](assets/screenshot-search.png)

The picker draws to the width of the pane it opens in, measured from its own
tty, so a row never wraps even in a narrow overlay.

## Install

Needs Go 1.26+ and Herdr 0.8.0 or newer.
Transcript search is optional. It needs [ccrider](https://github.com/neilberkman/ccrider).
Without `ccrider` on `PATH`, the Transcripts tab is not shown.

```sh
./install_plugin.sh
```

The script does not ask a question. Agents pass `--with-transcripts` to install ccrider.
Without that flag, ccrider is skipped and the Transcripts tab stays hidden.

You can still install the picker alone:

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
Rows for closed panes are pruned past 200.
A live pane keeps its triage state no matter how long it sits.

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
