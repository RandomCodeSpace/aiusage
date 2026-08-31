# Compatibility promise

This document is for Go programs that import aiusage and for maintainers
reviewing a change to a public contract. After reading it, a consumer should be
able to upgrade without guessing whether a package, method, error, or machine
format changed meaning.

## Baseline and support window

`v0.5.0` is the first tagged baseline for the public Go packages. It is also
the baseline for the JSON and CSV formats that existed before that tag.

aiusage keeps these contracts source-compatible and behavior-compatible from
`v0.5.0` through every v1 release. `v1.0.0` declares the proven contract stable.
It does not redesign the API. A future incompatible Go API needs a `/v2`
module. Existing version 1 machine formats remain available.

`v0.5.0` and `v1.0.0` require Go `1.25.13` or newer.

## Covered Go packages

The promise covers every package outside `internal` that ships in `v0.5.0`:

- `model`
- `adapter` and `adapter/all`
- every built-in package below `adapter`
- `collect`
- `pricing`
- `store`

Pi and OpenClaw remain separate tool identities even though one package exposes
both adapters.

The three buildable examples are contract consumers. They cover one collection
pass, an external adapter, and read-only reporting.

## What compatible means

Existing imports and keyed struct literals continue to compile. Exported
names, package paths, signatures, constant meanings, zero values, error
identities, and documented behavior keep working.

The following behavior is part of the contract:

- `errors.Is` and `errors.As` keep recognizing documented errors.
- Collection remains partial-success. One bad source does not discard good
  observations from other sources.
- Adapters remain read-only over harness-owned files and databases.
- Usage, activity, and turn context remain append-only after insertion.
- Read-only store handles do not create, migrate, repair, or change a database.
- Live reporting can read while collection writes.
- Unknown cost stays unknown. It never turns into zero.

Existing consumer-implemented interfaces do not gain methods. This includes
`adapter.Adapter`, `collect.Store`, `collect.Pricer`, and `collect.Refresher`.
New optional behavior uses another interface, function, type, or option.

A minor release may add an exported declaration. It may append a field to a
public struct only when the zero value keeps old behavior, the type stays
comparable if it was comparable before, and project examples use keyed
literals. Existing fields are not removed, renamed, reordered, or retyped.

Tool, model, provider, service-tier, price-source, adapter, and dimension values
are open vocabularies. Consumers must accept a value they do not recognize.

## Release rules

Patch releases contain compatible fixes only. A patch does not add exported
API, change a machine schema, raise the minimum Go version, or advance the
database schema.

Minor releases may add API, commands, flags, adapters, dimensions, and opt-in
machine formats. They do not remove or reinterpret existing behavior. A minor
release may advance the database schema under the migration rules below.

A behavior fix may change a value that violated the documented accounting
rules. The fix needs a regression fixture and a release-note entry. That is not
permission to reinterpret a valid value.

## Deprecation

A deprecated declaration keeps working through v1. Its documentation starts
with `Deprecated:`, names the replacement, and links the change in release
notes. Parity tests keep the old and new paths aligned. Removal waits for `/v2`.

## Database compatibility

Every database created by a tagged release remains forward-migratable. Writable
open applies migrations one transaction at a time and records the new schema
version last. Read-only open does not migrate. An older binary refuses a newer
database, so downgrading a database in place is unsupported.

Before a minor release advances `store.SchemaVersion`, aiusage creates and
verifies a full pre-migration backup, retains the complete migration chain, and
tests every tagged schema lineage. Migrations preserve authoritative usage,
activity, turn-context, accumulator, and checkpoint data. Derived rollups may
be rebuilt from the ledger.

## Machine output

`summary-json/v1`, `events-json/v1`, and `events-csv/v1` have fixed field names,
types, null rules, timestamp meaning, column order, raw-data opt-in, and empty
output behavior. Read the [machine output contract](./export-contract.md)
before parsing them.

Human tables and terminal rendering may change spacing, color, wrapping, and
wording. Their information and interactions remain supported, but scripts
should use JSON or CSV.

## How the project checks compatibility

GitHub CI compares the candidate module API with the baseline using a pinned
`apidiff`. CI also compiles the external examples and runs exact machine-output
fixtures. The release workflow repeats those checks for one captured source
commit and refuses a tag when the evidence belongs to another commit.

The exported value of `store.SchemaVersion` is the only planned API-diff
exception. CI accepts that value change only with the matching migration and
backup evidence. It does not accept a general compatibility allowlist.
