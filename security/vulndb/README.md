# Retained Go vulnerability database

`vulndb.zip` is an unchanged bulk snapshot downloaded from
<https://vuln.go.dev/vulndb.zip>, published by the Go security team.
`snapshot.json` records its SHA-256, source, retrieval time, and database
modification time. Git retains the exact input with the source revision; the
required Nix checks never fetch this database from an upstream URL.

The Go Authors publish the database entries under CC-BY-4.0. The upstream
license is retained in `LICENSE`; see the attribution and licensing in
<https://github.com/golang/vulndb>. This archive has not been modified or filtered.

Update explicitly with:

```text
nix run .#vulndb -- update security/vulndb
nix run .#test-vulndb
nix run .#dependency-check
nix run .#security
```

Review the manifest and advisory changes, then run the full local push gate.
Commit the archive and manifest together. The updater validates all input before
replacing either file. If interrupted between the two file replacements,
verification rejects the mismatched pair; rerun the update or revert both files.
Do not use Git LFS or an expiring CI cache as the only copy. Old revisions keep
their old snapshots through ordinary Git history.

The database's `modified` timestamp describes its contents. It is not a
wall-clock expiry time. A passing pinned scan only covers the retained advisory
set; review an update regularly and before a release. See
[`docs/security-input-retention.md`](../../docs/security-input-retention.md) for
the design, alternatives, and verification requirements.
