# Vision

`herdr-recall` exists so that the captain can switch across 20 to 30 live agent threads, unblock each agent with quick feedback, and cycle back without losing track of what needs attention next.
It serves a single operator running high-concurrency Herdr sessions, and it turns live Herdr snapshots plus local recall state into a keyboard-first triage inbox and pane switcher on `prefix+k`.
It owns exactly one thing: the modal recall and triage overlay (`herdr-plugin.toml`, `main.go`, and `recall.json` in the plugin config directory).

## Inbox Zero for Live Agent Threads

The default view is a Superhuman-style Inbox of agent panes that currently require a cycle-back: panes that are blocked, newly done, manually flagged, due from a reminder, or recently touched and not yet cleared.
Once a pane is in Inbox, its queue position stays pinned while you jump in and review it so the top thread never shifts away until you explicitly press `e` or `z`.
Closing and reopening the popup restores the active tab, sort order, status filter, search query, and selected pane.
Pressing `e` marks the selected pane done for the current cycle and removes it from Inbox.
Done is never mute: any new agent activity returns the thread to Inbox.
Viewing a pane is the operator reading, not agent work, so it never returns a thread and never moves the pinned queue.
Pressing `z` snoozes the selected pane with a quick reminder (`20m`, `1h`, `3h`, `Tomorrow 9am`, or `Clear`) and hides it from Inbox until due.
A snooze carries a return rule: `If no reply` breaks early on new agent activity, `Regardless` keeps the timer.
Panes with a due reminder render `!` and panes with a pending reminder render `~` beside the status column.
Triage history for a live pane is never pruned; only closed panes age out.

## Context Annotations and Manual Overrides

Any pane can carry a one-line sticky note set with `n` so the captain can record what a thread is waiting on before jumping away.
The sticky note renders inline in a dim column beside the Herdr tab name and is searchable with `/`.
When Herdr's runtime detector reports `idle` or `working` on a thread the captain knows is stuck on an external dependency or human decision, pressing `!` toggles a manual `blocked` override on that pane.
A manual override takes precedence in status filtering and Inbox triage until the captain clears it or marks the pane done with `e`.

## Native Herdr Visual Fidelity and Direct Focus

The picker opens as a 95% by 95% Herdr popup modal and reuses Herdr's exact colour vocabulary, status dots, and clean tab-only row titles.
Row titles display the Herdr tab name alone, never concatenating terminal titles, workspace names, or directory basenames into the label.
The coloured status dot (`blocked` red, `working` yellow, `done` teal, `idle` green) sits immediately to the left of the tab name, while the status word and recency metrics align in fixed right-hand columns.
Closed panes are filtered out against the live snapshot on every open, and pressing `enter` focuses the target pane directly over `HERDR_SOCKET_PATH` (`pane.focus`) rather than stepping through neighbor geometry.

## Vim, Tmux, and Superhuman Keyboard Grammar

Browse mode never types into the search box; `/` enters search, `esc` leaves search while keeping the filter, and a second `esc` closes the popup.
Navigation uses `j` and `k` (or arrows) for rows, `g` and `G` for ends, `ctrl-u` and `ctrl-d` for half-pages, `b/w/i/d/a` for status filters, and `h` and `l` to switch between `Favorites`, `Inbox`, `Recent`, `Most used`, custom tabs, and `All`.
Triage actions stay on single keys in browse mode: `e` marks done, `z` sets a reminder, `n` edits the sticky note, `!` toggles manual blocked, and `m` marks a favorite.
Custom tab management follows tmux and vim conventions: `c` creates a custom tab via a visible multi-field form, `r` revises the current custom tab's criteria, and `x` deletes it after a confirmation dialog that defaults to Cancel.
Custom sorts use self-contained directional names (`Most recent`, `Oldest`, `Most used`, `Least used`), display the active sort order as a pill in the header on sortable tabs (`All`, `Favorites`, and custom tabs), and refuse invented priority scores or separate ascending/descending rows.

## Scope

`herdr-recall` is not a replacement for Herdr's core server, not a workspace tree browser (`Map` is retired), not an agent spawner, and not a cross-host fleet daemon.
It does not send prompts into agent panes, does not read or summarize agent transcripts with an LLM, and does not run background timers outside Herdr's event hooks and popup lifecycle.
The live plugin is installed into `~/.config/herdr/plugins/github/` via `herdr plugin install`, never linked from a transient `~/code` working tree.

A change aligns when it makes switching, triaging, annotating, or resurfacing live agent panes across 20 to 30 threads faster and more reliable inside the popup.
A change should be resisted when it invents opaque heuristics, clutters row titles with non-tab metadata, steals single-letter browse keys for typing, or requires a background daemon to function.
