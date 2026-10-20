# Transfer protocol and skeleton review

An independent review task, without the implementation conversation, examined the session-six protocol, manifest arithmetic/canonical encoding, command skeleton, smoke checks and README. It ran scoped tests, vet, race tests, a command build and executable Transfer smoke with Go 1.27.3. No code blocker was found. The review did not treat specified data movement as implemented.

The review identified a missing durability gate for recovered chunks. The contract now requires revalidation followed by file/directory synchronization before reporting recovered progress or acknowledging a duplicate. A synchronization failure cannot return success. A future failure test must kill the receiver between chunk publication and directory synchronization, then inject recovery synchronization failure.

The review also required an explicit repair transition when finalization finds missing chunks. The contract now defines replacing finalizing intent with a durable uploading journal only when neither a publication witness nor destination exists, and accepting PUT only after that update. A valid ready witness finishes publication; an invalid witness blocks recovery. Completed sessions reject chunk writes and return the same verified receipt on finalize retries.

A follow-up caught ambiguous missing-receipt wording. Absence is expected before completion; a missing receipt blocks a completed journal, while a malformed receipt blocks whenever present. The reviewer confirmed all findings resolved and no remaining blocker in session-six scope.

Full repository checks and recorded source fingerprints are under test/results. Transfer remains at 0.0.0-dev with help/version and fail-closed operational commands. Snapshot preparation, HTTP operations, persistence, crash recovery and final publication still require their later implementation and failure tests. Inspect 0.1.1 is unchanged.
