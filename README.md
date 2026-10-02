# herdr-recall

A pane recall picker for [Herdr](https://herdr.dev). It remembers which panes
you actually use and lets you jump back to any of them from one overlay list.

Press `prefix+shift+l` and a picker opens in the style of Herdr's built-in
goto overlay (`prefix+k`). It starts in browse mode; press `/` to search.
Panes are listed in three groups:

- **FAVORITES** - panes you starred, always at the top
- **RECENT** - most recently focused panes
- **MOST USED** - panes with the highest visit counts whose last-seen time
  aged out of the recent list

## Keys

| Key | Action |
| --- | ------ |
| `prefix+shift+l` | open the picker |
| `/` | enter search mode (browse mode never treats a key as a filter) |
| type to filter | search mode only: typed characters filter the list |
| `ctrl+u` | search mode: clear the query |
| `enter` | jump to the selected pane |
| `f` | browse mode: favorite / unfavorite the selected row |
| `j` / `k` or arrows | browse mode: move the selection; arrows also move in search mode |
| `esc` | search mode: leave search and keep the picker open |
| `esc` | browse mode: close the picker |

The footer shows the keys for the mode you are in: key names in accent,
hint words dim, the same way `prefix+k` does. Status appears as a coloured
dot plus a coloured word (red blocked, yellow working, teal done, green
idle), section headers are coloured, and the favourite star is yellow.

## Screenshots

Favorites view:

![favorites](assets/screenshot-favorites.png)

Recency view:

![recency](assets/screenshot-recency.png)

Most used view:

![most used](assets/screenshot-most-used.png)

After pressing `f` on a row:

![favorite toggle](assets/screenshot-favorite-toggle.png)

Browse mode (footer shows `/ search`):

![browse mode](assets/screenshot-browse.png)

Search mode after pressing `/` with a typed query:

![search mode](assets/screenshot-search.png)

Status colours, red blocked / yellow working / teal done / green idle:

![status colours](assets/screenshot-status-colours.png)

The picker draws to the width of the pane it opens in, measured from its own
tty, so a row never wraps even in a narrow overlay.

## Install

Needs Go 1.26+ and Herdr 0.8.0 or newer.

```sh
git clone https://github.com/RooseveltAdvisors/herdr-recall
cd herdr-recall
./install_plugin.sh        # builds bin/herdr-recall and links the plugin
```

Then bind it in `~/.config/herdr/config.toml`:

```toml
[[keys.command]]
key = "prefix+shift+l"
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

Enter runs `herdr workspace focus` and `herdr tab focus` for the target pane,
then walks `herdr pane focus` hops (choosing the direction from the geometry
in `herdr api snapshot`) until the target pane is the focused pane.

## Companion CLI

- `herdr-recall stats` - print the current state JSON
- `herdr-recall record` - record the focused pane now
- `herdr-recall jump <pane-id>` - jump without the picker
- `herdr-recall favorite <pane-id>` - toggle a favorite
- `herdr-recall render` - print one picker frame (honours `RECALL_QUERY`,
  `RECALL_SEL`, `RECALL_W`, `RECALL_H`, `RECALL_STATUS`); used for tests and
  screenshots
