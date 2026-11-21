# Encrypted remotes

An **encrypted remote** (rclone's [crypt](https://rclone.org/crypt/))
encrypts files on the worker, before they leave it, and stores them in a
folder of another of your remotes. Jobs, the file browser and folder sizes
see the files as they are; the storage (a bucket, an SFTP server) only ever
holds encrypted contents and, by default, encrypted names.

```
What jobs and the browser see       What the bucket of "B2 offsite" holds
Vault: documents/taxes/2026.pdf ──► vault/p0e52nreeaj0a5ea7s64m4j72s/9f0eb7tq1kcd3m6l8ah4s2n0rg/l42g6771hnv3hn3ejbbdm6i9ec
       (the encrypted remote)         (its folder "vault": encrypted names and contents)
```

Encryption and decryption happen on the worker that runs the job. The
controller only stores the remote's settings and its passwords, encrypted
at rest with `secrets.key`, and sends them to that worker, as for any
remote's secrets.

![The new-remote form of an encrypted remote with a generated password](screenshots/crypt/01-new-remote.png)

## Creating one

Under **Remotes, Add remote**, choose **Encrypted (crypt)**:

| Field | What it is |
|---|---|
| **Store in** | One of your remotes and a folder on it (**Browse** picks one). Only encrypted data is written there. |
| **Password** | The key to the files. See [the password](#the-password). |
| **Salt (second password)** | Optional. Also needed to read the files: keep it with the password. |
| **File name encryption** | **Standard** (the default) encrypts names; **Obfuscate** hides them but keeps their length; **Off** leaves them readable. |
| **Encrypt folder names** | On by default; only with file name encryption. |
| **Name encoding** (Advanced) | How encrypted names are written: base32 (the default, safe everywhere), base64 (shorter, needs a case-sensitive remote) or base32768 (for remotes that limit name length in characters rather than bytes). |
| **Suffix** (Advanced) | Added to file names when names are not encrypted (`.bin` by default; `none` for no suffix). |

**Store in** can be any of your remotes except a **Local directory**, a
**union** or another encrypted remote ([limits](#limits)). Use a folder of
its own, unless you mean to read data stored there before, with
[exactly the same settings](#a-second-remote-over-the-same-folder). A file
in it that the encrypted remote did not write (another tool's, or another
encrypted remote's) has a name it cannot decrypt, and Syncr has rclone
refuse such names instead of skipping them: listing the folder that holds
it fails and names it, and so does a job that reads that folder or a
folder above it (a sync from the root, for example). Jobs on other folders
work, and the [password check](#a-wrong-password) is not fooled by a few
such files.

**Test remote** reads the first bytes of one of the files already stored,
then lists the encrypted remote's root: with a wrong password or salt the
test fails, in every name mode ([a wrong password](#a-wrong-password)).

The remote you store in shows the encrypted remote under **Used by** in the
Remotes list, and cannot be deleted until the encrypted remote is deleted
first.

## The password

The form says it above the field:

> Syncr never shows this password again. Without it, nobody can read the
> files in this remote, not Syncr and not you. Keep a copy somewhere safe,
> such as a password manager.

- **Generate** fills in a random password of 32 characters, made in your
  browser, and shows it with a copy button.
- **Create** stays disabled until you tick **I have saved the password
  somewhere else**. Generating again unticks it.
- The salt is a second password. It is optional, but if you set one, it is
  needed as much as the password: keep both together.
- Keep them **outside Syncr** too. Losing the controller's database or its
  `secrets.key` would otherwise lose the files as well; with the passwords
  you can still [read the data with rclone](#reading-the-data-without-syncr).

## What is fixed after creation

The password, the salt, **File name encryption**, **Encrypt folder names**,
**Name encoding** and **Suffix** are set when the remote is created. The
edit page shows them locked, and the API refuses another value.

They are what turns the stored data back into your files: with another
password or other name settings, the files already stored would no longer
decrypt, and new files would be written under other names beside them.
Making stored data unreadable by editing a form is not something Syncr
lets happen by accident.

- **The name and Store in can change.** Pointing Store in at another
  folder is how you follow data you moved (with the same settings).
- **The API refuses any value for a fixed secret,** even the stored one.
  Accepting an equal password would let anyone with an API token (yours, or
  a stolen one) confirm guesses of it. Forms never send the fixed secrets
  when you save an encrypted remote.
- **To change the password,** create a second encrypted remote with the
  new password in another folder, run a **Copy** job from the old one to
  the new one, check it, and then delete the old one.

## Checksums

rclone keeps **no checksums** on an encrypted remote: it cannot hash the
contents without decrypting them. So a transfer or a two-way sync with an
encrypted side **cannot compare checksums**:

- the editors do not offer it. The transfer's comparison and the two-way
  sync's **Compare files by** say "This remote keeps no checksums", and
  compare by size and modification time instead;
- the API refuses it with a field error on
  `definition.rclone_transfer.options.checksum` or
  `definition.rclone_bisync.options.compare[i]`: "is not available: a
  remote that keeps no checksums". Schedules are refused when they are
  saved, and one stored before fails when it runs.

This holds for any remote that keeps no checksums, not only encrypted
ones ([which remotes](remotes.md#checksums)). rclone still checks each
file's contents as it decrypts it, so a damaged or tampered file fails to
read.

## A wrong password

Since the password is fixed, a wrong one only happens with a **second
encrypted remote over the same folder**, made with a mistyped password or
salt ([other name settings](#a-second-remote-over-the-same-folder) are
another matter).

**Before anything uses an encrypted remote, the worker checks its
password.** It lists the remote from its root and reads the first bytes of
one of its files that is not empty, which must decrypt. With a wrong
password nothing decrypts (rclone checks every block it decrypts), so:

- a **copy, sync or move** from or into the remote (a dry run too), a
  **two-way sync** with a side on it and a **backup** of a folder of it
  (mirror or mount) fail with `execution_failed` before rclone changes
  anything: *the encrypted remote cannot decrypt the files stored in its
  folder, so its password is wrong…*, followed by rclone's message.
  Nothing is copied, deleted or overwritten, no mirror is touched and no
  snapshot is made, whatever folder the job uses;
- **Test remote**, the file browser and the folder pickers fail the same
  way.

This holds for every name setting and on every storage. It costs one short
rclone run per encrypted remote per job or listing: a listing that stops at
the first file with data, and one read of at most 64 KiB.

**A few foreign or damaged files do not fail it.** The check stops at the
first file that decrypts, and only fails once it has met three signs of a
wrong password without one: a file whose contents do not decrypt (a damaged
file), a name that does not (a file the remote did not write; the check
skips it, unlike jobs), or a folder of the listing that turns out to be
missing. With a wrong password it usually meets about three (a few more
when rclone is quicker than the stop). With the right password, it fails
only when three such files come before the first one that decrypts: rclone
reads the files of a folder after it has listed the whole folder (on S3 and
B2, a batch of 100 files of the recursive listing), so three foreign files
in the first folder it reads (or batch) fail it, wherever the good files
are. A remote whose only files with data are foreign or damaged fails it
too.

Why each name setting needs it (rclone 1.75.2; Syncr has rclone refuse
names it cannot decrypt in jobs and listings, `strict_names`):

| File name encryption | With a wrong password, without the check |
|---|---|
| **Standard** | Listings fail: "undecryptable". |
| **Obfuscate** | Names list, garbled, without an error. A garbled folder's name encrypts to another name: on SFTP that folder is missing, but on S3 and B2 it lists as **empty**. A sync from the remote's root would empty its destination, a sync into it would delete the files at its root, and a backup would empty its mirror. |
| **Off** | Names list as they are; only reading a file fails ("failed to authenticate decrypted block - bad password?"). |

**What the check cannot see.** A remote that holds no file with data
(nothing yet, or only empty files) has nothing to decrypt, so its jobs run.
With obfuscated names and a wrong password, a sync from such a remote can
then replace its destination's copies of those empty files with garbled
names; nothing with contents is at stake. And if a mistyped second remote
writes into a folder while it is still empty, its files use its own
password: each remote then reads its own files and not the other's, and
the check of either one passes or fails depending on which files it meets
first. The check also counts a file as empty when the storage does not
report its size (rare: some WebDAV servers), so a remote of only such files
has nothing to check. Use a folder of its own for each encrypted remote.

### A second remote over the same folder

A second encrypted remote over data that another one stored (to follow
data you moved, or to read it from another account) must use **exactly
the same** password, salt, **File name encryption**, **Encrypt folder
names**, **Name encoding** and **Suffix**. Each of them changes the names
the files are stored under, and the password and salt also the contents.

The password check catches a different password or salt in every mode,
and most other differences, because the names then do not decrypt (rclone
1.75.2, checked on MinIO): standard, obfuscated or no name encryption read
as another one, another name encoding, and another suffix than `.bin` all
fail the check. Two differences pass it, because the names still decrypt:

- **Encrypt folder names** off over data written with it on: the stored
  folder names are read as plain names. The second remote sees your
  folders under their encrypted names, and writes new folders under their
  plain names beside them. A **sync into it deletes the first remote's
  files** (the source has no folders of those names), and the first remote
  then fails to list the folder: the second remote's files have names it
  cannot decrypt.
- **Suffix** `none` over data written with `.bin` (names not encrypted):
  the files are listed with `.bin` in their names, so a **sync into it
  writes every file again without the suffix and deletes the stored
  ones**, and the first remote then fails to list them.

Nothing in Syncr tells these apart (a file's contents decrypt either
way): check the settings before running a job that writes into a second
remote. In both cases the sync's source still has the files, but the
first remote's copies are deleted.

**Folders that do not exist.** Before a transfer or a backup reads a folder
below the remote's root, the worker also looks for that folder in its
parent's listing. A missing one fails with `not_found`: *…is not in its
parent folder on the crypt remote…; if it exists, the remote's password is
wrong*. On S3 and B2, where a missing folder of a plain bucket is an empty
source, a missing folder of an encrypted remote is `not_found`.

**Two-way syncs** check both sides the same way. Besides, a second
encrypted remote has its own pair state (the state key includes the
remote), so its first run is a resync, which never deletes.

**Folder sizes** are not checked: with a wrong password they may count a
folder as empty.

## Reading the data without Syncr

The data is ordinary rclone crypt data. With the passwords and the
settings you chose, any rclone can read it, without Syncr. Configure the
remote you store in under a name of your choice, then a crypt remote over
the same folder. Its `remote` is that remote's root as Syncr uses it (an S3
or B2 bucket, an SFTP remote's base folder) followed by the folder under
**Store in**. rclone keeps passwords obscured in its configuration:
`rclone obscure 'your password'` prints the form to paste (a new one each
time; any of them works).

```ini
# rclone.conf (rclone config file shows where yours is)
[offsite]
type = s3
provider = Cloudflare
access_key_id = <your access key ID>
secret_access_key = <your secret access key>
endpoint = https://<account id>.r2.cloudflarestorage.com

[vault]
type = crypt
# the bucket, then the folder under "Store in"
remote = offsite:my-bucket/vault
# rclone obscure '<the password>'
password = <obscured password>
# rclone obscure '<the salt>' (leave the line out if you set no salt)
password2 = <obscured salt>
# the remote's settings: these are the defaults
filename_encryption = standard
directory_name_encryption = true
filename_encoding = base32
suffix = .bin
strict_names = true
```

Then `rclone lsf vault:` lists your files and
`rclone copy vault:documents ./documents` decrypts a folder. `rclone
cryptdecode vault: <encrypted name>` tells which file an encrypted name is.

## Limits

- **No nesting.** An encrypted remote cannot store its data in a union or
  in another encrypted remote, and cannot be a union member. Each layer
  would need its own password and its own checks, and a mistake in one
  would hide the others; unions of encrypted remotes may come later.
- **Not in a worker's local folder.** A Local directory remote is a folder
  of one worker, which every user may browse on that worker; the
  encrypted remote must be stored on one of your remotes. To encrypt onto
  a disk, use a remote that reaches it (SFTP, WebDAV) instead.
- **No restic repository on an encrypted remote.** restic encrypts its
  repositories already, so a second layer would only add a password to
  lose. Put the repository on the remote you store in instead (in another
  folder). **Backups of a folder of an encrypted remote** work, through a
  mirror or a mount ([restic](restic.md#back-up-a-folder-of-a-remote)):
  restic reads the decrypted files. A backup whose source is stored in, around or inside its
  own repository's folder is refused.
- **Transfers that overlap through the wrapper are not refused,** such as
  a sync from an encrypted remote into the folder it stores its data in.
  The plain files it writes there have names the encrypted remote cannot
  decrypt, so listing the folders that hold them fails, and so does every
  job that reads them: it fails loudly. Two-way syncs are checked: their sides are compared by
  where the data is stored, and pairs on an encrypted remote wait for pairs
  on the folder it is stored in ([two-way sync](bisync.md)).
- **Changing Store in while jobs wait** leaves those queued jobs waiting
  for the old folder's other jobs, as for union members.
- **Progress counts encrypted bytes,** a little more than the files' sizes
  (a 32-byte header per file and 16 bytes per 64 KiB).

## Older workers

Workers report that they can run encrypted remotes (the `rclone.crypt`
feature) from 2.3.0. An older worker shows as **Outdated** and:

- a job on an encrypted remote sent to it fails with `worker_outdated`:
  *worker "…" must be upgraded to run jobs on remotes of type "crypt"*. A
  schedule on a label that picks such a worker fails the same way;
- folder listings, **Test remote** and folder sizes of an encrypted remote
  pick only upgraded workers; with none online, they say the workers must
  be upgraded.

Upgrade them with the command on the worker's page
([upgrading](upgrade.md)).
