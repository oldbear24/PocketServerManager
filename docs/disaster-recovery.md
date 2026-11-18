# Disaster recovery of the panel

> Status: new with [#32](https://github.com/oldbear24/PocketServerManager/issues/32). This guide brings a panel back on a new machine from its own backup and its recovery kit, so that every server reconnects with its own certificate and nothing is enrolled again. The commands and sentences are the ones the panel and the agent ship, and the restore drawer of a panel job (Backups › the panel's job › Restore) fills them in for you. Rehearse it before you need it (see [Rehearsing without an outage](#rehearsing-without-an-outage)). The formats, records and routes are specified in [architecture.md](architecture.md#panel-backup-and-disaster-recovery-32); why it works this way is [ADR 0008](adr/0008-the-panel-backup-is-sealed-and-the-kit-breaks-the-loop.md).

## What makes a panel recoverable

An agent enrolls once. It keeps its key, its certificate, the panel's root certificate and the address it dialled in its own volume (`identity.json`), trusts no other CA than that root ([ADR 0006](adr/0006-agents-pin-the-panel-ca.md)), and from then on dials that address and ignores `PANEL`. The CA's private keys exist only sealed with the master key, inside the panel's database. So a panel comes back for its servers from three things together:

1. **its pb_data**, which holds the CA, every server, the revocations and the audit log: the panel's own backup job;
2. **the same master key**, which opens every sealed secret, the CA's keys included: the recovery kit;
3. **the same address**, the one the agents dial: your DNS name or IP address and port.

A new panel started on empty volumes has none of these: it makes a new CA, and every server would have to enroll again. Nothing in this guide does that.

## Before a disaster

- **Back the panel up.** Backups › Back up the panel: a job whose source is the panel itself, carried by a server other than the panel's own machine, to a target off that machine (an S3 bucket, an SFTP server, a rest-server). It runs daily at 04:00 local time and keeps 14 daily, 8 weekly and 12 monthly snapshots by default; 10 minutes after a server is enrolled or removed or a certificate revoked, it runs once more. The carrier fetches the panel's copy already encrypted: neither it nor the repository ever sees the database or a stored file's contents unencrypted, though each stored file's path and size stay readable (see “Who can do what” in the [reference](#reference)). The `panel_backup` alert stays open while the panel has no such job, while it is off or has no target, and when it has not succeeded for 48 hours.
- **Download the recovery kit and check it.** Settings › Recovery kit › Download a recovery kit: it needs your sign-in password and a passphrase of at least 12 characters (“Generate one” makes a 25-character code). Keep the `.pckit` file off the panel's machine, apart from its passphrase and from the backups, for example in a password manager. Then upload it in “Check a saved kit…”: the panel opens it as a recovery would. The `recovery_kit` alert stays open until a check opened a kit that is current, turns critical after 7 days, and opens again, naming why, when the master key, the panel's addresses, its backup job or one of the job's targets change: download and check a new kit then.
- **Keep the address.** Servers dial what they enrolled with. Enroll them through a DNS name (set `POCKETCLOUD_PUBLIC_URL`, or `POCKETCLOUD_GRPC_PUBLIC_ADDR` for the agents' address) with a low TTL, so a new machine takes over by a DNS change. Settings › Recovery kit lists the addresses the servers dial and warns about each IP address: a new machine must then take over that IP address.
- **Update the agents.** Only a #32 agent can carry the panel's backup, fetch it (`panel-recovery`) and change its address (`repoint`).
- **Pin the CA.** Start the panel with `POCKETCLOUD_EXPECT_CA_PIN=<pin>` (Settings › Recovery kit shows it, and so does `--env-out` below). A start on empty or someone else's volumes then fails with a sentence instead of making a new CA.
- **PocketBase's `--encryptionEnv`.** If you started PocketBase with it, the kit does not hold that variable's value: keep it with the kit, or do not use it.
- **Rehearse twice a year**, as below.

`panel keygen` is not a backup of the key: the recovery kit is. Never run `keygen` on a new machine during a recovery.

## What a backup holds

| In the backup | Not in the backup |
|---|---|
| `data.db`, copied consistently with `VACUUM INTO`: every collection, the internal CA, the sealed secrets, the servers, the revocations, the audit log and its seal | request logs (`auxiliary.db`), metrics (`metrics.db`) and diagnostic bundles: they are rebuilt or expire |
| every stored file the database refers to, such as the final dumps of removed databases | the master key: it is in the kit |
| | the panel's environment and ports: `recover --env-out` writes them from the kit |
| | your DNS records, and files PocketBase keeps in S3 |

The recovery point is the last panel backup: daily at 04:00, and 10 minutes after any enrollment, removal or revocation. Everything done after it is not in the backup (see step 9 below).

## Restore on a new machine

You need the `.pckit` file, its passphrase, and Docker on the new machine. The commands use the image names of your release: `<agent image>` is the agents' image (the kit's `Agent-Image` line says which, and the launcher command in the restore drawer of the panel's job names it), and `<panel image>` is `pocketcloud-panel` in the same registry with the same version tag. Copy the kit into the working directory as `kit.pckit`.

1. **Do not start a panel on empty volumes, and do not run `panel keygen`.** Either makes a new CA or a new key. If one ran already, step 4 says how to keep it out of the way.
2. **Make the address lead to the new machine.** Point the DNS name at it, or give it the old IP address. Open the agents' port (9443 unless you chose another) and the UI's (8090, or 80 and 443 with your HTTPS or reverse proxy). Keep the old machine off for good: two panels with one CA would split the servers.
3. **Fetch the panel's backup.** restic runs only in the agent's own hardened job container, as for every backup. Either (a) on any server that survived, through its running agent: type the passphrase into the shell without echo, then fetch:

   ```sh
   read -rs POCKETCLOUD_KIT_PASSPHRASE && export POCKETCLOUD_KIT_PASSPHRASE
   docker exec -i -e POCKETCLOUD_KIT_PASSPHRASE pocketcloud-agent pocketcloud-agent panel-recovery fetch --kit - --target <target> --snapshot <id|latest> --out - < kit.pckit > pocketcloud-panel.pcpb.part && mv pocketcloud-panel.pcpb.part pocketcloud-panel.pcpb
   ```

   or (b) on the new machine, with the agent's image as a launcher, which asks for the passphrase:

   ```sh
   docker run --rm -it -v /var/run/docker.sock:/var/run/docker.sock -v "$PWD":/work <agent image> panel-recovery fetch --kit /work/kit.pckit --target <target> --snapshot <id|latest> --out /work/pocketcloud-panel.pcpb
   ```

   `POCKETCLOUD_KIT_PASSPHRASE` is for `docker exec -e` only: an exec's environment stays out of the container's configuration, but `docker run -e` stores the value there, where `docker inspect` shows it. `-it` needs a terminal; without one, leave out `-t` and add `--passphrase-file` as in step 4. `<target>` is the name of one of the panel job's targets; leave `--target` out for the kit's first remote target. `panel-recovery snapshots` with the same `--kit` and `--target` lists the panel's snapshots, newest first; `latest` takes the newest. A local target (a directory of a server) can only be fetched on its own server. `fetch` checks every part of the copy with the kit's key as it writes it, and on any failure exits 1: it removes the file it was writing with `--out FILE`, and with `--out -` the `&& mv` leaves what reached the shell under the `.part` name, never the copy's (delete it). `recover` refuses a cut copy anyway (“The backup is damaged or incomplete at segment N.”). Its two lines on stderr name the kit, the target and snapshot, the size and digest, and the backup's time, panel version and number of servers. The launcher mounts `docker.sock`, which is root over Docker on that machine, the same trust as an agent. Copy `pocketcloud-panel.pcpb` to the new machine's working directory if you fetched it elsewhere.
4. **Restore it into the new panel's volumes:**

   ```sh
   docker run --rm -it -v pocketcloud:/data -v pocketcloud-secrets:/run/secrets -v "$PWD":/work <panel image> recover --kit /work/kit.pckit --from /work/pocketcloud-panel.pcpb --env-out /work/panel.env
   ```

   It asks for the passphrase on the terminal, which `-it` needs; never give it to `docker run` with `-e` (step 3 says why). Without a terminal, as in a script, leave out `-t`, put the passphrase in a file only you can read in the working directory (`umask 077` before you write it), add `--passphrase-file /work/<that file>`, and delete the file afterwards (step 10). `recover` runs before PocketBase opens anything. It checks the backup with the kit's key as it decrypts it next to pb_data, compares the backup's key and CA with the kit's, checks the database (SQLite's integrity check, migrations this panel knows, one CA whose key opens with the kit's), and only then writes the master key to `/run/secrets/pocketcloud-master-key` (kept if it is the kit's already), puts pb_data in place and writes `panel.env`: the old panel's environment and `POCKETCLOUD_EXPECT_CA_PIN`. Any refusal exits 1 and writes nothing. It ends like this:

   ```
   Opened the recovery kit KIT-7Q2M-9XHC of http://192.0.2.2:8090, made 2026-11-17 10:02 UTC by admin@example.com.
   The backup was made 2026-11-17 04:00 UTC by panel 0.32.0: 3 servers, audit log up to entry 3812, CA sha256:5d1e….
   Checked it: every part opens with the kit's key, data.db passes SQLite's integrity check, and its CA opens with the kit's key.
   Wrote the master key to /run/secrets/pocketcloud-master-key.
   Restored /data/pb_data: data.db and 1284 stored files, 312 MB.
   Servers dial 192.0.2.2:9443 (an IP address: this machine must take it over). Start the panel there with /work/panel.env.
   Before the first start, run: panel audit verify. Nothing done after 2026-11-17 04:00 UTC is in this backup: servers enrolled since must enroll again.
   ```

   Check the backup's time and audit entry: an older snapshot than you expect shows there. If a panel did start on these volumes before (“… already holds a panel”), add `--replace`: what was there is kept as `pb_data.replaced-<UTC>`, never deleted. If you mounted the master key from a host file instead of the `pocketcloud-secrets` volume, give `recover` a writable place for it (`--master-key-file /work/master.key`) and mount that file read-only when you start. `--dry-run` checks everything and writes nothing.
5. **Check the audit log before the first start**, with the key read-only:

   ```sh
   docker run --rm -v pocketcloud:/data -v pocketcloud-secrets:/run/secrets:ro <panel image> audit verify
   ```

   It must say “The audit log is intact”.
6. **Start the panel** with the environment `recover` wrote, the key read-only, and the port the agents dial:

   ```sh
   docker run -d --name pocketcloud-panel --restart unless-stopped --env-file panel.env -p 8090:8090 -p <dial port>:9443 -v pocketcloud:/data -v pocketcloud-secrets:/run/secrets:ro <panel image>
   ```

   Add your HTTPS ports and variables as before (see the README). The first start finishes the restore before the gateway listens: it cancels what was queued when the backup was made (“The panel was restored from a backup made {T}: what was queued then is not sent. Queue it again if it is still wanted.”), marks every server offline without an alert, ends every session, writes a sealed “Panel restored” entry where the restored audit log resumes, and logs “the panel was restored from a backup”. Its log shows no setup link: the accounts are the backup's. For 90 seconds no server is marked offline, while the agents come back from their back-off.
7. **Sign in again**, with an account of the backup: every session and link from before the disaster stopped working.
8. **Verify.** The CA pin in Settings › Recovery kit is the kit's (`CA-Pin` line). The block “Restored from a backup” shows “Servers back: N of N” within about two minutes, and the `panel_restored` alert resolves once every server is back. In Audit & activity › Security, the integrity check passes, and after “Panel restored” come only sign-ins, yours included (every sign-in writes one), and “Certificate rejected” for a server enrolled after the backup (step 9). Run the panel's backup job and one other job: their sealed secrets open on the new machine.
9. **Review what the backup did not know.** A server enrolled after the backup was made is refused (“this server was removed from the panel: enroll it again”): remove its agent's volume and enroll it again. Removals and revocations made since are undone, so a revoked certificate works again: revoke or remove those servers again. Domain, edge and site changes made since are pushed back to the servers as the backup had them: make them again. Targets added since, and passwords PocketCloud made since (a new target's repository password, a rotated database password), are gone with the old machine.
10. **Clean up.** Where you typed the passphrase, run `unset POCKETCLOUD_KIT_PASSPHRASE` (or close that shell), and delete any passphrase file you wrote. Delete `pocketcloud-panel.pcpb` (and a `.part` a failed fetch left), `panel.env` and the copies of `kit.pckit` wherever you put them (keep the kit you saved), and `pb_data.replaced-*` once you no longer need it. Download and check a new kit if Settings asks for one.

## Rolling a running panel back

To return a panel to an earlier backup on the same machine: stop and remove its container (`docker stop pocketcloud-panel && docker rm pocketcloud-panel`; its data stays in the volumes, and step 6 needs the name free), fetch the snapshot you want as in step 3, run step 4 with `--replace`, then steps 5 and 6. `recover` refuses while a panel serves the directory (“A panel is serving <dir>: stop it first.”). The same limits as in step 9 apply: everything after the backup is undone.

## When the address cannot be kept

If the new machine cannot take over the old address, tell each server's agent where the panel is now. On each server:

```sh
docker exec pocketcloud-agent pocketcloud-agent repoint --panel panel.example.com:9443 --check
docker exec pocketcloud-agent pocketcloud-agent repoint --panel panel.example.com:9443
docker restart pocketcloud-agent
```

`repoint` dials the new address with the server's own certificate, checks that the panel there holds this server's CA and knows this server, and only then changes the address in its identity; the key and certificates stay. `--check` changes nothing. It refuses, changing nothing, with “The panel at X does not hold this server's CA: nothing was changed.” or “The panel at X does not know this server: nothing was changed.” A server you cannot reach any more must be enrolled again.

## Rehearsing without an outage

Restore onto a machine that the agents cannot reach at their address, such as one on an isolated network, and go through steps 3 to 8 without step 2. Never let a rehearsal panel answer at the agents' address: two panels with one CA would both command the servers, and the agents would go to whichever answered. Of step 8, what needs no server passes there: the CA pin is the kit's, `panel audit verify` (step 5) says the log is intact, Audit & activity › Security shows “Panel restored”, and Settings › Recovery kit shows the block “Restored from a backup”. What needs the servers cannot: “Servers back” stays at 0 of N, the `panel_restored` alert stays open, and no job runs, as its server is offline to this panel. That is a rehearsal going right; `recover` (step 4) has already shown that the backup's CA opens with the kit's key, the key that seals the targets' secrets too. Remove the rehearsal machine and its copies afterwards. Rehearse twice a year, and after you change the panel's job, its targets or the address.

## Troubleshooting

| Symptom | Cause | What to do |
|---|---|---|
| The new panel's log shows a setup link, or its CA pin is not the kit's | It started on empty volumes and made a CA of its own | Stop it. Run `recover` with `--replace`; start with `panel.env`, whose `POCKETCLOUD_EXPECT_CA_PIN` stops this from happening again |
| “This master key (id X) did not seal this database (id Y): start the panel with the key file of its recovery kit.” | The key file is not the backup's, such as one `keygen` made | Run `recover` again, or put the kit's key in place: `recover` writes it |
| “This database has no internal CA, but POCKETCLOUD_EXPECT_CA_PIN expects <pin>: restore the panel's backup into it first.” | The panel started on empty volumes with the pin set, as it should refuse | Run `recover` (step 4) first |
| “This database had servers or a CA … but holds no internal CA: …” | The database lost its CA record: it is damaged or not this panel's | Restore it from a backup with `recover --replace` |
| “The database holds two internal CAs: restore it whole, never merge.” | Two databases were merged | Restore the backup whole with `recover --replace` |
| “POCKETCLOUD_EXPECT_CA_PIN is A, but this database's CA is B: this is not the panel your servers trust.” | These volumes hold another panel | Use the right volumes, or `recover --replace` |
| “The backup comes from a newer panel (migration X): recover with panel V or newer.” or, at the start, “The data comes from a newer panel (V): start that version or a newer one.” | The panel image is older than the one that made the backup | Use the panel image of version V or newer |
| “The passphrase does not open this kit, or the kit was changed.” | A wrong passphrase, or an edited kit | Type it again; check that the file is the one you kept |
| “This backup was sealed with another master key (id X) than the kit's (id Y): use the kit of the panel that made it.” | A kit of another panel, or of before a key change | Use that panel's kit |
| “The backup is damaged or incomplete at segment N.” | The copy was cut or changed | Fetch it again, from another target or an older snapshot |
| “<dir> already holds a panel (it may have started with a CA of its own): …” | A panel started on these volumes before | Add `--replace` |
| “<file> holds another master key: it opens nothing of this backup. Move it away and run recover again.” | `keygen` ran, or another panel's key is there | Move the file away and run `recover` again |
| “Another panel serves <dir>: …” or “A panel is serving <dir>: stop it first.” | Two panels, or a panel and `recover`, on one pb_data | Stop the other one |
| Agents keep trying to connect, and the panel shows them offline | The address they dial does not lead to the new machine | Finish step 2 (DNS, IP address, port), or `repoint` each server |
| A server's agent logs “this server was removed from the panel: enroll it again” | It enrolled after the backup was made | Remove its agent's volume and enroll it again |
| The passphrase is lost | Nothing sealed with it can be opened, by anyone | Without another kit or the key file, the backups cannot be opened: set up a new panel and enroll every server again |

## Reference

**`panel recover`** runs as the panel image's first argument, before PocketBase opens pb_data:

```
panel recover --kit FILE --from FILE.pcpb [--dir DIR] [--master-key-file PATH] [--passphrase-file F] [--replace] [--env-out FILE] [--dry-run]
```

`--dir` defaults to `$POCKETCLOUD_DATA_DIR` (`/data/pb_data` in the image), `--master-key-file` to `$POCKETCLOUD_MASTER_KEY_FILE` (`/run/secrets/pocketcloud-master-key`). The passphrase comes from `--passphrase-file`, `POCKETCLOUD_KIT_PASSPHRASE` (through `docker exec -e` only, never `docker run -e`), or a prompt without echo on a terminal. Exit 0 once the backup is in place, 1 on any refusal, with nothing written. Its other refusals: “This backup's CA (pin X) is not the kit's (pin Y): use the kit of the panel that made it.”, “The backup's database fails SQLite's integrity check: …”, “The backup holds N internal CAs, not one: it is not a whole panel's database.”, “The backup's CA does not open with the kit's key: the kit is not of the panel that made it.”, “The backup's CA is not the one its manifest names (pin X): the backup is not whole.”, “The backup's database was sealed with another master key (id X) than the kit's (id Y): use the kit of the panel that made it.”, and “The master key cannot be written to <file>: mount it writable for recover.”

**`pocketcloud-agent panel-recovery targets|snapshots|fetch`** `--kit FILE|-` `[--passphrase-file F]` `[--target NAME]`, and for `fetch` `[--snapshot ID|latest] [--out FILE|-] [--no-verify]` (`--no-verify` only to look at a damaged copy). The passphrase comes from `--passphrase-file`, `POCKETCLOUD_KIT_PASSPHRASE` (through `docker exec -e` only), or, with `--kit FILE`, a prompt without echo on a terminal. It never opens the agent's command store, so it runs beside a running agent. Its errors: “the passphrase does not open this kit, or the kit was changed”, “no panel snapshot of job:<id> on <target>”, “the copy does not open with the kit's key: it belongs to another panel”, and restic's own lines with the secrets masked.

**`pocketcloud-agent repoint --panel HOST:PORT [--check]`**, above.

**Panel options.** `POCKETCLOUD_EXPECT_CA_PIN` / `--expect-ca-pin`: the pin the database's CA must have.

**Files.** The kit is PCKIT1: a text file whose eight header lines (kit id, time, panel, key id, CA pin, versions, KDF) anyone can read and whose payload is sealed with AES-256-GCM under argon2id of the passphrase (t=3, 64 MiB, 4 threads). The backup is PCPB1, `pocketcloud-panel.pcpb`, the one file of a panel snapshot (restic host `panel`, tags `pocketcloud`, `job:<id>`, `pocketcloud-panel`): a 48-byte header with the master key's id, then the manifest, the database and the stored files, each under a plain segment header with its name and size and sealed in 64 KiB chunks with a key derived from the master key, and a trailer that lists them. A download of a panel snapshot (Backups › Restore › Download the encrypted copy) is the same file and a valid `--from`. Both formats are specified in [architecture.md](architecture.md#panel-backup-and-disaster-recovery-32).

**Alerts.** `recovery_kit` (no checked kit is current; critical after 7 days), `panel_backup` (no job, off, without targets, or no success in 48 hours), `panel_restored` (after a restore, until every server is back); none is raised before the first server.

**Audit.** Security entries “Recovery kit downloaded” (`recovery_kit_exported`), “Recovery kit checked” (`recovery_kit_checked`) and “Panel restored” (`panel_restored`). None holds the kit, its passphrase, the key or a target's secret.

**PocketBase's own backups.** The PocketBase dashboard's backups still work, without the panel's metrics and its files, but restoring one is refused: “Restore the panel with `panel recover`, which checks the master key first: see docs/disaster-recovery.md.” Such a zip holds `data.db` unencrypted, with the session signing secrets: keep it as safe as the panel itself.

**Who can do what.**

| Who has | What they can do |
|---|---|
| the kit alone | Read its header: the panel's address, key id, CA pin and versions. Nothing else: the rest is sealed with the passphrase, and argon2id makes each guess cost 64 MiB and three passes |
| the kit and its passphrase | Read every secret of the panel: the master key opens every sealed secret and the CA's keys, so they could pose as the panel to the servers and read the backup targets. Rotate every credential and enroll every server again |
| a backup repository and its password | Delete or roll back panel snapshots, which the integrity checks and `recover`'s summary of the backup's time and audit entry show. Read what a copy leaves in the clear: the export's time, the number of files, each stored file's path and size (the names and times of database dumps, the names of uploads, record ids), and which files did not change between snapshots. Neither read the database, the manifest or a file's contents, nor forge a copy, as the keys come from the master key. They can read every ordinary backup in that repository anyway |
| the carrier server | Only what any server that backs up to the targets has: the encrypted copy, with what it leaves in the clear (above), and the repository's access, given as the run is delivered |
| a PocketBase dashboard backup | Everything in `data.db` unencrypted, the session signing secrets included, but no sealed secret without the master key |
