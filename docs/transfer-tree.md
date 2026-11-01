# Folder transfers (trees)

From Transfer 0.5.0, `send --prepare-only --source=FOLDER` prepares a whole folder as one transfer. The folder becomes a single payload called a tree, and the protocol in transfer-v1.md moves it like any other file: the same manifest, chunks, resume, parallel uploads and completion receipt. On the receiver the tree is published as files/ID.data, and `bitspeed-transfer unpack` writes it out as a folder.

Why one payload and not one transfer per file: a receiver root holds at most 16 sessions, and the receiver never takes file names from the sender for its own storage. A tree keeps both rules. The single receipt covers the whole folder, because its digest covers the index, and the index holds every file's digest.

## Format, version 1

A tree payload has three parts, in this order:

1. The bytes of every file, one after another, in index order. Nothing separates them; the index gives each size.
2. The index: canonical JSON with no whitespace, `{"format":"bitspeed-tree","version":1,"entries":[...]}`. Each entry is `{"type":"dir"|"file","path":...,"size":...,"sha256":...}`, with the fields in that order. Folders have size 0 and an empty sha256.
3. A 16-byte footer: the index length as an unsigned 64-bit big-endian number, then the eight ASCII bytes `BSTREE01`.

The canonical encoding is what Go's encoding/json produces for this structure: fields in the order above, no whitespace, and `<`, `>`, `&`, U+2028 and U+2029 inside strings written as `\u003c`, `\u003e`, `\u0026`, `\u2028` and `\u2029`. Another implementation must produce the same bytes. Entries are sorted by the bytes of their path, with no duplicates. Every entry's parent folder appears earlier in the list, so a reader can create folders and files in one pass. Only the exact canonical bytes are accepted: extra fields, repeated or differently cased keys, whitespace and trailing bytes are all refused. The sizes in the index must add up to exactly the bytes before it.

Paths are relative and `/`-separated UTF-8. A path has no empty, `.` or `..` segment and no backslash, NUL, other control character or U+FFFD (the replacement character, which is also what Windows names with unpaired surrogates turn into). A segment is at most 255 bytes and a whole path at most 4096. A tree holds at most 100,000 entries, and its index is at most 16 MiB. The whole payload stays within the protocol's 64 GiB and 16,384-chunk limits.

## Preparing a folder

Preparation walks the folder without following symbolic links. It carries folders, including empty ones, and regular files, including empty ones. A symbolic link, device, socket or pipe anywhere in the folder stops preparation with an error naming the path. Permissions, owners, timestamps and extended attributes are not kept.

Each file is opened, checked to be the same file the walk saw, copied and checked again, as for a single file. After copying, the folder is walked a second time. A file that grew, shrank, changed, appeared or disappeared, or a folder that appeared or disappeared, fails preparation and removes the half-made spool entry. As with a single file, this detects observable changes; it does not promise a point-in-time copy of a folder another program is writing.

The spool must not be inside the folder, and the folder must not be inside the spool. The spool needs room for the whole tree, as it does for a single file. Prepare prints `"tree": {"files": N, "folders": N, "bytes": N}` beside the manifest; bytes counts file contents only.

## Unpacking on the receiver

```
bitspeed-transfer unpack --root=DIR --id=ID --out=DIR
```

Unpack works on a completed transfer only. It reads the session's manifest, journal and saved receipt, then files/ID.data. It takes no lock and writes nothing in the root, so it can run while `serve` is running.

OUT must not exist, its parent must exist, and it must be outside the receiver root (checked both by resolved name and by comparing the identity of every existing folder above OUT with the root, so another spelling of the root, a different letter case or a bind mount does not get past it). Unpack writes into a new hidden folder next to OUT, named `.bitspeed-unpack-` and a random suffix. Each file is checked against its index digest as it is written and then synced. When every file is written, the rest of the payload is read and the digest of the whole stream must equal the receipt's file digest. Only then are the folders synced, deepest first, and the hidden folder renamed to OUT, and OUT's parent synced. On Linux and macOS, OUT is first claimed with an exclusive empty folder so the rename can never replace a folder someone else made in the meantime; Windows refuses to rename onto an existing folder anyway. Any failure unpack reports before the rename removes the hidden folder, so OUT never appears half written. If the process is killed, a `.bitspeed-unpack-` folder can be left beside OUT; delete it. If the final sync of OUT's parent fails, unpack reports the error although OUT is complete; checking it again with a fresh unpack into another folder is safe.

Files and folders are created exclusively. On a filesystem that ignores case or normalises Unicode, two names that would land on the same entry make unpack fail with a name-clash error instead of merging them. On Windows, names Windows cannot hold (reserved characters, a trailing dot or space, device names such as CON or COM1) are refused before anything is written. Unpacked files are created private (0600, folders 0700, or the folder's inherited access list on Windows); change them afterwards if others need to read them.

A transfer that is not a tree, one that is not completed, and a payload whose footer, index or bytes do not check out are all refused. Unpack does not change the received payload, so it can be repeated into another OUT, and `clean` treats a tree like any other completed transfer.

## Disk use

The sender spool holds one copy of the folder. The receiver holds the chunks and the published payload, as for a single file, and unpack writes a third copy into OUT. Running `clean` after delivery removes the chunks.
