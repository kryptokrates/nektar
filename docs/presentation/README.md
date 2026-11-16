# Presentation: Die Mautstelle und die offene Straße

German slide deck for
[`../SOVEREIGN-INTERNET-VS-AGENTIC-WALLETS.md`](../SOVEREIGN-INTERNET-VS-AGENTIC-WALLETS.md),
built with [Marp](https://marp.app/): the slides are one Markdown file.

| File           | What it is                                              |
| -------------- | ------------------------------------------------------- |
| `slides.de.md` | All slides. Edit this.                                  |
| `theme.css`    | Colours, fonts, slide types. Slides contain no styling. |
| `img/*.svg`    | Pictures. Plain SVG with German labels, edit as text.   |
| `.marprc.yml`  | Marp settings (registers the theme).                    |

## Preview and export

Run from this folder. Nothing is installed into the repo; `npx` fetches a
pinned Marp CLI.

```sh
# live preview with reload on save: open http://localhost:8080/slides.de.md
npx --yes @marp-team/marp-cli@4.5.1 -s .

# PDF and PowerPoint (need Edge or Chrome installed)
npx --yes @marp-team/marp-cli@4.5.1 slides.de.md --pdf  -o dist/slides.de.pdf
npx --yes @marp-team/marp-cli@4.5.1 slides.de.md --pptx -o dist/slides.de.pptx
```

`dist/` is gitignored. The "Marp for VS Code" extension also previews the file
directly; add `docs/presentation/theme.css` to its `markdown.marp.themes`
setting so it picks up the theme.

## Editing rules

- A line with only `---` starts a new slide.
- `## ` is the small kicker above the title, `# ` is the title.
- `> ` is the highlighted take-away box. One per slide.
- Slide type: `<!-- _class: NAME -->` at the top of a slide.
  - `lead` — dark section divider
  - `beleg` — "this happened" list; start every bullet with `**year**`
  - `zitat` — one big quotation
  - `dicht` — smaller type, for tables and long lists
- Picture beside the text: `![bg right:36% contain](img/NAME.svg)`.
  Picture inside the text: `![w:1060](img/NAME.svg)`.
- Any other `<!-- comment -->` is a speaker note. Each example's source lives
  in the note of its slide, so a fact and its source are edited together.

## Content rule

Every claim is backed by events that actually happened and that an audience
accepts without argument. When adding an example, add its source to the
slide's speaker note. BSV is not mentioned before Part 7 on purpose.
