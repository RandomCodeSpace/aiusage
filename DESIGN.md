---
name: aiusage
description: A terminal comparison workbench for local AI usage.
colors:
  accent-light: "#9C36B5"
  accent-dark: "#EB99E3"
  now-light: "#B5780A"
  now-dark: "#F2B441"
  positive-light: "#1A7F37"
  positive-dark: "#56D364"
  warn-light: "#C0362C"
  warn-dark: "#E5534B"
  border-light: "#788596"
  border-dark: "#65758B"
  muted-light: "#526276"
  muted-dark: "#9AA9BD"
  faint-light: "#9AA3AE"
  faint-dark: "#4A535F"
---

# Design System: aiusage

## Overview

Keep comparison controls visible and give usage rows the available reading space. The terminal supplies the background and base text color. Text, alignment, and state markers carry meaning when color is unavailable.

The working priorities are:

- Persistent range, group, sort, chart, and row-filter controls.
- Comparison rows before secondary charts and suggestions.
- Explicit cost coverage and a compact machine-status line.

## Colors

Use the adaptive light or dark foreground from `internal/tui/theme.go`. Magenta marks interaction, selection, and the wordmark. Amber marks live/scrub information and known-cost trends. Green and red carry status. Muted text, faint rules, and the border separate supporting information from data.

Input, output, and cache use the terminal's ANSI palette. Tool colors and stable tool glyphs remain defined in `theme.go`; do not substitute arbitrary chart colors. Background and base text inherit the terminal, so neither has a fixed color token.

## Typography

The terminal owner chooses the monospace font and cell size. aiusage has no font-family or pixel-size scale. Bold text distinguishes headings, metric labels and values, selected rows, and active controls. Muted text carries labels; summary footnotes use italic text where the terminal supports it.

## Layout

Dimensions below are terminal cells and refer to the workspace area after outer chrome unless specified otherwise.

- Range, Group, Sort, Chart, and Filter occupy persistent rows. Choices wrap as needed, with continuation rows aligned after the control label.
- With fewer than 7 rows left after controls, show scope cost when it fits and an operable Compare row that opens the full comparison.
- With fewer than 22 workspace rows, summaries use 3 rows. Otherwise, they use 4 columns at width 84 or above, 2 at width 38 or above, and 1 below 38.
- Trend and suggestions appear alongside comparisons only at workspace width 110 or above, height 22 or above, and at least 13 remaining body rows. The left column receives three fifths of the space after a one-column gap.
- Comparison rows determine table height. Details receive the remaining space when at least 9 body rows are available; shorter bodies prioritize the table.
- Tables show token columns at table width 65 or above; narrower tables retain the group name and cost.
- Machine status occupies one row where the workspace body fits. At outer terminal heights below 18, the overview hides the header and breadcrumbs to retain controls.

## Elevation & Depth

All four existing elevation levels inherit the same terminal background. Use spacing, titled rules, bold text, and markers for hierarchy. There are no shadows or painted background tiers.

## Shapes

The application owns the outer frame. Workspace sections use headings and thin rules; control selection uses square brackets. Shared padded panels retain one row above and below and two columns on each side. Overlays may use their existing border.

## Components

- **Choice strips.** Brackets identify the applied value. A leading `›` identifies keyboard focus while selecting. Choices remain in place before, during, and after selection; mouse targets cover the choices.
- **Row filter.** Show the current value or `all rows`. `/` enters text input; a nonempty filter exposes Clear.
- **Comparison table.** A `›` marker, bold weight, and accent color identify the selected row. Preserve the visible row count and scroll window.
- **Usage summary.** Input, output, cache, and cost stay labeled. Large summaries add trend rows and cost coverage; compact summaries retain the four values.
- **Trend.** Use continuous terminal lines with axes when space permits. Cost is labeled `known USD`; empty, unpriced, and recorded-zero data have distinct text states.
- **Machine status.** Keep CPU, memory, and disk on one line; unavailable measurements read `unknown`.
- **Navigation.** Use the existing top tabs or compact glyph row. Focus markers and labels preserve meaning without color.

## Do's and Don'ts

- Do preserve keyboard and mouse access to the visible controls.
- Do retain text and glyph markers when color is stripped.
- Do distinguish unknown, estimated, partial, and reported cost.
- Don't assign a synthetic background or a fixed terminal font.
- Don't hide frequent comparison choices in a dropdown.
- Don't turn unavailable cost into a zero-valued trend.
