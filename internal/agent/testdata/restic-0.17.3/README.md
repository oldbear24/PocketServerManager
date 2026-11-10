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
- restore-include.txt: which --include patterns pick what, globs and their
  backslash escapes too; the fake restic is held to its globs.
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

For the integrity checks, the locks and the retries (#31), the same binary on
local repositories of 40 snapshots of one random file each; a damage is bytes
overwritten in place. Each of these files starts with what it shows, on lines
starting with "# ", then holds one or more runs: "$ restic ARGS", then
"--- stdout", "--- stderr" and "--- exit N"
(internal/testutil/resticcapture reads them). A job's paths replace the
scratch ones: the repository is /repo, the check's cache
/tmp/pocketcloud-check-cache; the cache options of backup and unlock are left
out. A holder of a lock ran in a UTS namespace of its own, with a job's
hostname, pc-k3j9x2m4q8w1z7v, or nas.

- check-ok.txt: pass 1 (`--no-lock check --read-data-subset=1/20`) with no
  error. The counters' last lines come without RESTIC_PROGRESS_FPS.
- check-progress.txt: the counter of the packs read, at RESTIC_PROGRESS_FPS=2.
- check-packs.txt: pass 1 and pass 2 of a group with a damaged data pack: "pack
  ID contains N errors: [...]" on one line, the "restic repair packs IDS" and
  "restic repair snapshots --forget" hints, then "Fatal: repository contains
  errors", exit code 1.
- check-index.txt: a damaged index file: "error: error loading index …", the
  "restic repair index" hint; check stops after loading the index.
- check-tree.txt: a damaged tree pack, structure only: "error for tree ID:"
  and its error indented by two spaces, then "The repository is damaged and
  must be repaired".
- check-missing.txt: a deleted data pack: "pack ID: does not exist", the
  backend's "Load(<data/…>, …) failed" retries and the read's "download error".
- check-locked.txt: pass 2 refused by an exclusive lock (exit code 11, the four
  lines), and by a shared one with `--retry-lock 6s` ("repo already locked,
  waiting up to 6s for the lock" on the standard output first); and pass 1
  passing while the exclusive lock was held.
- check-nolock-concurrent-prune.txt: pass 1 reading while forget --prune from
  another process deleted most packs: the false errors that pass 2 filters out.
  No lock file existed while pass 1 read.
- check-empty.txt: a repository with no snapshot: counters without a total;
  a size subset fails on it ("Cannot read from a repository having size 0").
- check-invalid-lock.txt: a lock file of random bytes: pass 2 fails with
  "Fatal: invalid lock file: …", exit code 1; pass 1 passes.
- check-readonly.txt: the repository bound read-only: pass 1 passes; pass 2
  retries its lock file for 15 minutes, then fails "unable to create lock in
  backend: … read-only file system", exit code 1.
- check-repository.txt: a wrong password (exit code 12) and no repository (exit
  code 10).
- backup-locked.txt: a backup with the tool's flags refused by a lock: the
  four lines in plain text despite --json, exit code 11.
- unlock.txt: unlock removing nothing (silent, exit code 0) while the holder
  ran, then from another host after the holder was killed; on the holder's
  own host after it was killed, "successfully removed 1 locks"; a lock file
  restic cannot read stays.
- invalid-lock.txt: a backup refused by a lock file of random bytes: "Fatal:
  invalid lock file: …" and the advice to use unlock --remove-all, exit code 1.
- locks.txt: `list locks`, and `--no-lock cat lock ID` of an exclusive and a
  shared lock: "time" (UTC), "exclusive", "hostname", "username", "pid".

Also seen: check prints "using temporary cache in DIR/restic-check-cache-N" and
removes that folder at its end; `list locks` takes no lock; a restic stopped
with SIGTERM removes its lock at once ("signal terminated received, cleaning
up" on the error output), though a backup slowed by --limit-upload still ran a
minute later.
