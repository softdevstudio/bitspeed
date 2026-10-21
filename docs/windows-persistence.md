# Windows persistence design

Transfer's protocol, storage layout and recovery rules are the same on every platform. What changes on Windows is how each step of the persistence sequence in transfer-v1.md and finalization.md is carried out. This document maps every Unix primitive Transfer relies on to its Windows counterpart, says what each one guarantees and lists the crash tests that back it.

Scope: Windows 10 and Server 2019 or later, amd64, local NTFS volumes on fixed drives. ReFS (including Dev Drive), FAT and exFAT, removable drives, network shares and mapped drives are refused by the receiver. The sender spool is not checked for filesystem type, as on Unix, but it needs the same calls to succeed.

## Primitive mapping

| Step | Linux and macOS | Windows |
|---|---|---|
| Private directory | `mkdir` with mode 0700, checked as exactly 0700 | `CreateDirectoryW` with a protected DACL created in the same call, granting full control to the current user, SYSTEM and Administrators, inherited by children. Checked by reading the DACL through a handle opened without following reparse points |
| Private file | created 0600 inside a private directory, checked for no group or other bits | created inside a private directory and inherits its DACL. Checked the same way as directories |
| Sealed snapshot and manifest | `chmod 0400` and checked as exactly 0400 | no read-only attribute (it would block the flush described below). Privacy is checked as for any private file. Immutability rests on the SHA-256 revalidation every reuse already performs |
| File sync | `fsync` on the open file, including read-only descriptors | `FlushFileBuffers`. A handle opened only for reading cannot be flushed on Windows, so Transfer opens a second handle with write access, no reparse following and full sharing, and flushes that. The flush covers the file's cached data whatever handle wrote it |
| Directory sync | `fsync` on the directory | `FlushFileBuffers` on a directory handle opened with write access and backup semantics. NTFS accepts this and it flushes the directory's metadata. The call must succeed; there is no silent fallback |
| Atomic rename | `rename` | `MoveFileExW` with `MOVEFILE_REPLACE_EXISTING` and `MOVEFILE_WRITE_THROUGH`, followed by the same directory sync as on Unix |
| No-replace publication | `link` fails with EEXIST if the destination exists | `CreateHardLinkW` fails with `ERROR_ALREADY_EXISTS`. NTFS hard links give the same two-names-one-file witness, and `os.SameFile` compares volume serial and file index |
| Exclusive lock | `flock(LOCK_EX|LOCK_NB)` on .lock | `LockFileEx` with `LOCKFILE_EXCLUSIVE_LOCK|LOCKFILE_FAIL_IMMEDIATELY` on the first byte of .lock. Released when the handle closes or the process dies |
| Free space | `statfs` | `GetDiskFreeSpaceExW`, bytes available to the caller |
| Filesystem gate | `statfs` type or name | `GetVolumePathNameW`, then `GetDriveTypeW` must report a fixed drive and `GetVolumeInformationW` must report NTFS with hard link support |
| Stable source | open with `O_NOFOLLOW|O_NONBLOCK`, compare identity, size and modification time before and after | open with `FILE_FLAG_OPEN_REPARSE_POINT` and share mode read only, which refuses the open while another process holds write access and blocks writers, renames and deletes for the whole copy. The same identity, size and time comparison follows |

## Rules that only matter on Windows

A file or directory that is open cannot be renamed or deleted unless every handle allows it. Transfer closes each handle before renaming, linking or removing the file it refers to. The chunk upload path closes its temporary file before removing it on every error path.

Antivirus software sometimes opens a new file for a moment. If that makes a rename, link or delete fail, the step reports storage_unavailable, nothing is acknowledged, and repeating the request or restarting the receiver resumes from the last durable step.

Another program can hold a published destination open without sharing write access, for example a media player. While it does, status cannot flush the destination and reports storage_unavailable, which is retryable. Nothing is marked failed and no data changes. The same applies to a spool snapshot opened by another program during reuse.

A receiver root or spool directory created by the user must already have a private DACL: only allow entries for the current user, SYSTEM and Administrators, inherited or explicit. A folder under the user's profile normally qualifies. A folder made directly under C:\ normally does not, because it inherits access for Authenticated Users. Let Transfer create the root, or tighten an existing one with `icacls DIR /inheritance:r /grant:r "%USERNAME%:(OI)(CI)F" SYSTEM:(OI)(CI)F Administrators:(OI)(CI)F`. The token file needs the same kind of DACL, which a file created in the profile normally has.

The owner must be one of those three accounts too, because an owner can change the list at any time. Allow entries for OWNER RIGHTS are accepted, since they grant access only to that checked owner; Python's private temporary folders use them. Allow entries for any other account, a missing list, and object or callback entries all make the check fail. Deny entries for any account are accepted because they only remove access.

Token files may end with one newline in Unix or Windows form, so a file saved by a Windows editor works. The README shows a PowerShell command that writes one in plain ASCII.

## What the guarantees mean

Durable on Windows means the same as on Unix: the documented calls returned success on a supported filesystem. Disk write caching that ignores flush requests, storage drivers, virtualization layers and power loss can still defeat that. Process kills do not test power loss.

## Crash and failure tests

The existing process tests run unchanged on the Windows CI runner: a child process is killed with TerminateProcess during snapshot preparation, during publication of the spool directory, during chunk upload on both sides and at each finalization stage. Each restart must recover to a valid state and only send chunks the receiver cannot prove it has. The injected write, sync, close, rename and link failure tests run on Windows too.

Windows-specific tests cover:

- the directory DACL check accepting Transfer's own directories and rejecting a directory that also grants access to Everyone or Authenticated Users;
- a token file readable by another account being refused;
- the root lock refusing a second receiver or spool user, and being released when the holder is killed;
- a source file held open for writing by another handle being refused, and writes to it being blocked during preparation;
- flushing a file and a directory through the real calls;
- refusing a hard link over an existing destination;
- an executable smoke test of serve, send, kill, restart, status and clean with the packaged Windows binary.
