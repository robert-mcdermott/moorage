# The Moorage website

A product site with the whole documentation, published from the `gh-pages` branch at
<https://robert-mcdermott.github.io/moorage/>. It is plain HTML, CSS and a little JavaScript, and its generator needs nothing
beyond Node 22.

```text
docs/site/
  build.mjs          the generator: pages from the guides, the menu, links, pictures, the search, and the link check
  pages/index.html   the home page
  assets/            site.css and site.js, and the picture link previews show
docs/*.md            the guides: every page of the documentation is one of them, or some of its sections
docs/images/         the pictures, <name>-light and <name>-dark
docs/capture/        what takes the screenshots and draws the diagrams
site/                the built site (git ignores it)
```

## Building it

```bash
node docs/site/build.mjs
```

It builds into `site/`, or a folder given after it, and checks every local link, anchor and picture. It exits with an error and
lists the problems if any are broken, and CI runs it. To look at it:

```bash
python3 -m http.server 8817 --directory site
```

Then open <http://127.0.0.1:8817>.

## Writing the documentation

Edit the guides in `docs/`, and not the site: the build makes its pages from them, and GitHub shows the same files.

- **Pages.** `DOCS` at the top of `build.mjs` lists the pages in reading order, in groups, which are the menu. Each is a whole guide,
  or some of a guide's `##` sections (`intro: true` adds the text before its first section). A page of one section takes the
  section's heading as its title. Every section of a guide must be on a page, and the build says so if one is not. A new guide
  needs a line there.
- **Links.** Write links as GitHub reads them, to a guide and its anchor, such as `install.md#how-big-a-machine`: the build sends each to
  the page that holds that anchor. Links to other files in the repository go to GitHub.
- **Pictures.** A paragraph that is only a picture, `![What it shows](images/<name>-light.webp "The caption")`, becomes a figure with
  both themes' pictures, and GitHub shows the light one. Say in the alternative text what the picture shows.
- **The home page** is `pages/index.html`: HTML, with `{{shot <name> "alt" "caption" [window] [eager] [phone]}}` for pictures, and
  `{{version}}`, `{{github}}` and `{{raw}}`.
- **The command reference,** `docs/cli.md`, is made from the commands: `UPDATE_CLI_DOC=1 go test ./internal/cli -run TestCommandReference`.
- **The API reference** is made from `docs/openapi.json` as the site is built.

The Markdown the build understands is what the guides use: headings, paragraphs, lists (nested, loose or tight, with code inside),
fenced code, tables, block quotes, and inline code, emphasis, links and autolinks.

## Pictures

Screenshots are of a real server, and nothing in them is drawn. This builds `moor`, starts a server of its own in a folder of its
own, claims it, deploys the examples, makes requests for 15 minutes so that there are charts and logs to show, and opens each
scene in headless Chrome, in both themes. It leaves any other server on the machine alone:

```bash
node docs/capture/capture.mjs
```

`--reuse DIR` keeps the demo in that folder, and uses it again on later runs, and names choose some scenes. Pages that show the
last few minutes need `--traffic N` for a while first:

```bash
node docs/capture/capture.mjs logs metrics --theme dark --reuse /tmp/moorage-demo --traffic 15
```

Each scene is a line in `capture.mjs`: a page, and sometimes how tall to make it. Most pictures show a page's content without the menu, so
that they can be read at the width of a column of text.

The diagrams, and the picture link previews show, come from `diagrams.mjs`. Each diagram is plain SVG, with its colours from one table:

```bash
node docs/capture/diagrams.mjs
```

## Publishing

The site is what is committed on `gh-pages`, and GitHub Pages serves that branch as it is. Make a checkout of it once, beside the
repository, with its own worktree:

```bash
git worktree add ../moorage-site gh-pages
```

Then build into it, and commit there. The build writes into that checkout only with `--publish`:

```bash
node docs/site/build.mjs --publish
```

```bash
git -C ../moorage-site add -A
```

```bash
git -C ../moorage-site commit -m "The website, from main"
```

```bash
git -C ../moorage-site push origin gh-pages
```

Publish for each release, and whenever the guides have changed on `main` in a way that readers should have. The site says which version
it describes, from `internal/buildinfo`.

## For each release

1. Take the screenshots again if the console changed, and commit them.
2. Check the guides against what changed, and the highlight and "Where it stands" on the home page.
3. Build it, and look at what changed in both themes and at a phone's width.
4. Publish it once the release is out.
