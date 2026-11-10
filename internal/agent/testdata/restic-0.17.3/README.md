What restic 0.17.3 (the Alpine 3.21 package the agent's image installs) printed
for the commands the backup tool runs for restores (#30), captured from the
binary itself on a local repository, with the scratch directories' paths
rewritten to the job's (/volumes/vx, /restore/0). The tool's tests read these
lines, and the fake restic (internal/testutil/fakerestic) prints lines of the
same shape.

- snapshots.json: `snapshots --json ID`. A snapshot of 0.17 carries its
  `summary`; an id that is not there prints `[]`, exit code 0, and "Ignoring"
  on the error output.
- ls.json: `ls --json ID /volumes/vx`. The snapshot first, then the listed
  folder's own node, then what it holds, not deeper. `mode` is Go's
  os.FileMode: a folder has bit 31, setgid is bit 22. A folder that is not
  there prints the snapshot alone, exit code 0.
- restore-dry-run.json: `restore ID:/volumes/vx --target T --dry-run -vv
  --json --overwrite always --delete` into a destination where a.txt changed,
  extra.txt was added and sub/deep/c.txt removed. `item` is relative to the
  restored folder; folders are always "restored"; nothing is created, not even
  T. A real run with `-vv` prints the same verbose_status lines.
- restore-status.json: a real restore of 60 MB at RESTIC_PROGRESS_FPS=20.
- restore-errors.txt: the error output of a restore that met a folder where
  the snapshot has a file: a JSON error line per item, then "Fatal: There were
  1 errors", exit code 1. The summary on the standard output still counts the
  item as restored.
- restore-include.txt: which --include patterns pick what.
- dump-names.txt: `tar -tv` of `dump --archive tar ID /volumes/vx`: the
  folder's own entry is left out, and every name is its snapshot path without
  the leading slash. `dump --archive tar ID FILE` writes the file as it is.

Also seen: a restore's target root keeps the owner and mode it had, or root
and 0700 when restic creates it (the tool applies the snapshot folder's own);
`--delete` deletes only under `--target`; `--overwrite never` keeps a changed
file ("unchanged"); `restore ID:FILE` fails with "not a directory";
`restore ID:MISSING` fails with "path …: not found", exit code 1; `dump ID
MISSING` fails with "Fatal: cannot dump file: path … not found in snapshot",
exit code 1; `--retry-lock` is taken by restore and dump; a wrong password is
exit code 12 for every command.
