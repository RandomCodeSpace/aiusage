# Product

<!-- impeccable:product-schema 1 -->

## Platform

Terminal application for Linux and macOS, built with Go and Bubble Tea.

## Purpose and users

aiusage collects local usage records from AI coding tools into a searchable
SQLite history. The dashboard helps a developer compare token use, cost,
models, harnesses, projects, sessions, and recorded activity. This audience is
inferred from the existing README and CLI, not from new user research.

## Constraints

- Go 1.26.5 is the maximum toolchain version for this work.
- Source artifacts and the TUI's database connection are read-only.
- Unknown, estimated, partial, and reported costs remain distinguishable.
- Frequent comparison controls stay visible and support keyboard and mouse.
- Terminal colors, monochrome operation, reduced motion, and small windows
  remain supported.

## Current design brief

The user requested an autonomous TUI redesign informed by Dribbble references,
with particular emphasis on removing hidden filter and sort choices. Premium
quality here means legible comparisons, direct controls, stable selection,
responsive interaction, and honest data states.

## Evidence

README.md and CONTEXT.md describe product behavior and accounting terms.
internal/tui contains the current dashboard and interaction tests. Screenshots
for this work must use current code and explicitly synthetic usage data.
