# README dashboard capture

`ccdad-tui.png` shows the v0.24.0 TUI renderer running in a 116-column,
28-row pseudoterminal with the dark palette and Unicode glyphs. The capture uses
six example accounts, including an inactive Claude subscription and a serving
Codex account. Quota cells are produced by the normal view model.

The input is an in-memory snapshot: no real credentials, account details, or
network requests are involved. Terminal output was captured with its ANSI styles,
decoded into terminal cells, and rasterized using Menlo. Empty terminal rows were
cropped and a small terminal title bar was added. The dashboard's text, colors,
layout, and controls are rendered by ccdad itself.
