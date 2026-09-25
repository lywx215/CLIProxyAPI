# Historical blocked-build record qualification

`blocked-binary.json` is retained byte-for-byte. Only its `main.go` hash maps to
the blocked driver executable. The Python/CJS entries describe support files at
that historical moment; they are not compiled into the driver. The runner later
changed, so that record, the run-06 snapshot and final submission intentionally
have different Python hashes.

R2 neither executed nor rebuilt nor renamed the refused CPA executable. Its
SHA-256 remains `da12f45f2719a4e199eb160036caa6dc94eda4bc0cc3fdf40cf1fa5c4fb7c71f`,
and `main.go` remains `393abcdcaab6af5b449d2777debd180dbee49a70bb4cb9caee878bc49e3842e0`.
See [R2 identities](../DIAG-07-R2-validation/binary-identities.json) and
[source attestations](../DIAG-07-R2-validation/binary-provenance.json).
Invocation HEAD and current support hashes must not be relabeled as the build
source of a pre-existing executable.
