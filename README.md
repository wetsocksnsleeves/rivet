![CODED BY](https://img.shields.io/badge/CODED%20BY-LLM-d29922?style=for-the-badge)

# rivet

rivet is a dotfile manager. It symlinks each file in a dotfiles set
into the directory where that file belongs, and a background watcher copies
changes made in that directory back into the set. When an app creates a new
file in `~/.claude`, the file is synced.

# Why

GNU Stow links a whole directory when it can. When it can't, it links the
files inside the directory one by one. Stow falls back to per-file links when:

- the target directory already exists, which is common because apps create
  `~/.config/<app>` or `~/.claude` on first run
- two packages put files in the same directory
- you pass `--no-folding`

After that, a file an app creates in the target is not synced.

rivet always links individual files, and its watcher handles the changes that
links alone miss:

- A new file in the destination is moved into the set and replaced with a
  symlink.
- Many apps save by writing a temp file and renaming it over the original,
  which replaces the symlink with a real file. The watcher copies the new
  contents into the set and restores the symlink.
- Deleting a link in the destination deletes the file from the set. Deleting
  a file from the set removes its link.
- Files added to the set by `git pull` or a branch switch are linked
  automatically.
- When the watcher starts, it reconciles changes made while it was stopped.


# Installation

To install, run:

```sh
curl -fsSL https://raw.githubusercontent.com/wetsocksnsleeves/rivet/main/install.sh | sh
```

# Usage

## Create a dotfiles root

```sh
mkdir ~/.dotfiles && cd ~/.dotfiles
rivet init
```

`rivet init` creates a `.rivet` marker file. Every other command looks for it
in the current directory and its parents, the way git looks for `.git`.

## Add a set

```sh
rivet init claude --dest '$HOME/.claude' --exclude 'history.jsonl,projects/**'
```

This creates `claude/rivet.yaml`. The quotes keep `$HOME` unexpanded in the
file, so the same set works on machines with different home directories.

If `~/.claude` already exists, rivet moves each file that isn't excluded into
`claude/` and links it back. Excluded files stay in `~/.claude`.

## Link a set on another machine

```sh
git clone <your-dotfiles-repo> ~/.dotfiles && cd ~/.dotfiles
rivet link claude
```

If a different file already exists at a target path, `link` lists every
conflict and changes nothing. Run it again with one of these flags:

| Flag | Effect |
|---|---|
| `--adopt` | Move the existing file into the set, overwriting the set's version, then link it. |
| `--force` | Rename the existing file to `<name>.rivet-bak`, then link the set's version. |
| `--skip` | Leave conflicting paths alone and link everything else. |

A real file with the same contents as the set's file is not a conflict. rivet
replaces it with a link.

## Start the watcher

```sh
rivet watch install
```

This registers the watcher as a service that starts at login, and starts it
now: a launchd agent on macOS, or a systemd user service on Linux (logs go to
the journal; read them with `journalctl --user -u rivet`). `rivet watch stop`,
`rivet watch start`, `rivet watch status` and `rivet watch uninstall` manage
the service. `rivet watch run` runs the watcher in the foreground instead.

## Check status

```sh
rivet status
```

Prints one line per set with whether it is in sync, linked sets first. The
last line says whether the watcher is running. Pass a set name to show only
that set.

```
Linked:
  claude → ~/.claude       not synced
  nvim   → ~/.config/nvim  synced
Not linked (run `rivet link <set>`):
  zsh    → ~
Run `rivet status -v` to see what needs attention.
```

`-v` lists the paths that need attention in each set: conflicts, files the
watcher hasn't processed yet, and untracked files. `-vv` also lists every
link.

```
claude → ~/.claude   2 linked
  Untracked (new in dest; the watcher will adopt them):
    agents/draft.md
```

## Exclude files

```sh
cd ~/.claude
rivet exclude history.jsonl,todos/,'projects/**'
```

Adds each path to the exclude list in the set's `rivet.yaml`. Run it inside a
set or inside a linked set's dest, or pass `--set <name>` from anywhere in the
root. Paths are relative to the working directory, can be globs, and can be
separated by commas or spaces. A path that is already linked is replaced with
a copy of its file: it keeps its contents but no longer syncs. The set's copy
stays in the repo for you to delete.

## Stop managing a set

```sh
rivet unlink claude
```

Replaces each of the set's links with a copy of the file it pointed to. The
files in `~/.claude` keep working with the same contents, but changes no
longer sync in either direction. The set's files stay in the repo.

# rivet.yaml

Each set has a `rivet.yaml` at its top level.

```yaml
dest: $HOME/.claude
exclude:
  - history.jsonl
  - projects/**
strict: false
```

| Key | Meaning |
|---|---|
| `dest` | Required. The directory the set's files are linked into. The set's tree mirrors the tree under `dest`. rivet expands `~` and environment variables. |
| `exclude` | Glob patterns relative to the set and to `dest`. `**` matches any number of directories. `rivet.yaml` is always excluded. |
| `strict` | Defaults to `false`. When `true`, the watcher still restores links after atomic saves but never adopts new files, and `status` doesn't list untracked files. Use it for sets with a broad `dest` like `$HOME`. |

Several sets can share a `dest`. When a new file appears, rivet adopts it into
the non-strict set whose `dest` is the deepest directory containing it, so a
new file in `~/.claude` goes to a `claude` set rather than one with
`dest: $HOME`. If two sets have the same `dest`, rivet leaves the file in
place and `status` lists it as claimed by both.

rivet keeps per-machine state in `~/.local/state/rivet/state.yaml`, outside
the repo. It records which sets are linked on that machine, so each machine
can link a different subset.
