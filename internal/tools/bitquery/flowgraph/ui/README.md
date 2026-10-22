# Flow graph viewer (`flow_graph/1`)

`flow-graph.html` is one self-contained page that draws a `flow_graph/1` document as a graph, a Sankey
and a table. Every script and style is inline: it fetches nothing, loads no font and sends no analytics.
It lives at `internal/bitqueryresources/static/flow-graph.html`, embedded in the server with `go:embed`
(only the page goes in that folder: the whole folder is embedded), and is meant to be served two ways:

- as the MCP Apps view `ui://bitquery/flow-graph`, `mimeType: text/html;profile=mcp-app`;
- as the hosted read-only page behind `view_url`.

| file | what it is |
|---|---|
| `flow-graph.html` (in `internal/bitqueryresources/static/`) | the page (about 760 KB, of which vis-network is 652 KB) |
| `fixture-eth.json` | a synthetic `flow_graph/1` document that exercises every path (below) |

## Data modes

The page picks its mode at start-up, in this order.

1. **Hosted page.** If the document contains `<script type="application/json" id="flow-graph-data">…</script>`,
   the page renders that document and never uses `postMessage`. The comment marked "hosted data" near
   the end of `<body>` is the intended place for it.
   - Escape `<`, `>` and `&` in the JSON (Go's `encoding/json` does this by default), so the data cannot
     close the script element.
   - Expand is hidden. `data_as_of` is shown in a large box at the top. CSV and JSON exports download as files.
2. **MCP Apps view** (extension `io.modelcontextprotocol/ui`, specification version 2026-01-26), when the
   page runs in a frame. The view speaks JSON-RPC 2.0 over `postMessage` and only accepts messages whose
   `source` is `window.parent`.
   - **Handshake:** request `ui/initialize` with `{appInfo, appCapabilities: {availableDisplayModes: ["inline", "fullscreen"]}, protocolVersion: "2026-01-26"}`,
     then notification `ui/notifications/initialized`. The view takes `theme`, `styles.variables`,
     `displayMode` and `containerDimensions` from `hostContext`. It does not load `styles.css.fonts`,
     because that would fetch fonts from outside.
   - **Input:** `ui/notifications/tool-input` shows a loading line. `ui/notifications/tool-result` renders
     `structuredContent`; when that is missing, a `content` text block holding the JSON is used. An `isError`
     result is shown as an error. `ui/notifications/tool-cancelled` is reported, and
     `ui/notifications/host-context-changed` is merged into the context.
   - **Requests from the host:** `ping` and `ui/resource-teardown` are answered with `{}`. Any other request
     gets error -32601.
   - **Expand:** `tools/call` with `name: "flow_graph_expand"`. The arguments are `chain`, `address` (one
     node id) and every field of the original `params` except `address` and `hops`.
     - The button is shown only when `hostCapabilities.serverTools` is present, and only on stopped,
       not-expanded and last-hop leaf nodes. An error response or an `isError` result turns Expand off for
       that view, and a banner says why.
     - The result is merged on node `id` and on (`from`, `to`, `token`); the incoming edge wins.
     - Incoming hops are relative to the expanded node, so they are rebased, and a node keeps its smallest hop.
       The expanded node becomes a hop node. Other nodes keep their `in_usd` / `out_usd`, because the expand
       result counts only its own edges.
   - **After each expand:** request `ui/update-model-context` with the new totals (`content` text plus
     `structuredContent`: nodes, edges, priced USD, unpriced edges, expanded ids). Each update replaces the
     previous one.
   - **Other requests:** `ui/open-link` for `view_url` (`https` only; when the host has no `openLinks`, the
     link is copied instead), `ui/request-display-mode` for full screen (only when the host lists
     `fullscreen`), and `ui/notifications/size-changed` after every render and on resize.
   - **Exports are copied, not downloaded,** because the sandbox grants no downloads. The view tries
     `document.execCommand('copy')`, then `navigator.clipboard.writeText`. If both fail, it shows a text box
     for manual copying. The resource should declare `_meta.ui.permissions.clipboardWrite: {}`. It needs no
     `_meta.ui.csp` domains.
3. **Standalone:** opened directly with no data, the page offers a file picker for a `flow_graph/1` JSON file.

## What it shows

- **Graph** (vis-network): columns left to right by `hop`.
  - **Node colour** follows the label group. When a node has several labels, the first group in this order
    wins:
    1. sanctions (type contains `sanction`, or `ofac`);
    2. mixer;
    3. exchange (`cex-*`);
    4. bridge;
    5. other label;
    6. no label.
  - The colours come from a palette checked for colour-blind separation. Each node also prints its group
    as text, so colour is never the only cue.
  - **Node style:** seeds have a thick border. Stopped nodes are square-cornered with a heavy border and
    `STOP: <reason>`. Not-expanded nodes have a dashed border.
  - **Edges:** one arrow per (from, to) pair. Width is the pair's priced USD on a log scale. Unpriced pairs
    are thin and dashed. `change_unknown` edges are orange and labelled `change?`.
  - **Clicking a node** lights up everything upstream of it, back to the seeds, and everything downstream,
    and dims the rest. A walk does not pass through another seed.
- **Sankey** (d3-sankey layout, links drawn by the page): widths in USD.
  - USD widths are offered only when at least 90 % of the shown edges have `usd_priced_share` = 1.
    Partly priced links are hatched.
  - Below 90 %, a one-token Sankey is drawn in that token's units.
  - A Sankey cannot draw loops. Self-loops and edges to a lower hop are dropped. Edges within one hop are
    kept in USD order unless they would close a cycle. Everything dropped, and every unpriced edge left
    out, is listed under the chart.
- **Table:** every edge, sortable by any column.
- **Filters:** token (by token id, since symbols are not unique) and minimum USD, with "keep unpriced".
  They apply to all three views and to both exports. Seeds stay visible.
- **Banners:** truncated (with the `edges_dropped` / `usd_dropped` totals), partial, `change_unknown`,
  the xrp / xlm note that in and out USD need not match, look-alike (non-ASCII) symbols, and data problems.
  The window from `params` and `data_as_of` are in the header.

## Libraries

Each one is inlined between `<!-- BEGIN LIB name -->` and `<!-- END LIB name -->` as
`<script data-lib="name@version">…</script>`. The text is the published file unchanged, except that its
trailing `//# sourceMappingURL=` line is removed. The header comment lists each library's version and
integrity, and the comment at the end of the file holds the licence texts.

| library | version | file in the npm tarball | tarball integrity (`dist.integrity`) | licence |
|---|---|---|---|---|
| d3-array | 1.2.4 | `dist/d3-array.min.js` | `sha512-KHW6M86R+FUPYGb3R5XiYjXPq7VzwxZ22buHhAEVG5ztoEcZZMLov530mmccaqA1GghZArjQV46fuc8kUqhhHw==` | BSD-3-Clause |
| d3-sankey | 0.12.3 | `dist/d3-sankey.min.js` | `sha512-nQhsBRmM19Ax5xEIPLMY9ZmJ/cDvd1BG3UVvt5h3WRxKg5zGRbvnteTyWAbzeSvlh3tW7ZEmq4VwR5mB3tutmQ==` | BSD-3-Clause |
| vis-network | 10.1.2 | `standalone/umd/vis-network.min.js` | `sha512-1Kj7GD7XNjygliqRWXv/VPT1X/2eKz2e09PvwDtETHwuvKJ0emid7/PmWoFqCMypun8rLbjgmalSfPkiVZ8ZMg==` | Apache-2.0 OR MIT |

Two notes on the libraries:

- d3-sankey also depends on d3-shape, but only for `sankeyLinkHorizontal`, which the page does not call.
- The vis-network standalone build bundles its peer dependencies (as listed in its package.json) and core-js polyfills. The notices it carries, for Hammer.JS and core-js, stay in place.

Load order matters: d3-array before d3-sankey.

### Regenerating a library block

1. Download `https://registry.npmjs.org/<name>/-/<name>-<version>.tgz` into an empty directory.
2. Check it: `openssl dgst -sha512 -binary <name>-<version>.tgz | base64 -w0` must equal the integrity
   above without its `sha512-` prefix, and the registry's `dist.integrity` for that version.
3. Extract it without running anything: `tar -xzf <name>-<version>.tgz`. Do not use `npm install`;
   no package script is needed.
4. Remove the trailing `//# sourceMappingURL=…` line from the file listed above.
5. Make sure the text contains neither `</script` nor `<!--`.
6. Replace everything between the library's two markers with
   `<script data-lib="<name>@<version>">`, a newline, the file text, a newline and `</script>`.
7. Update the version and integrity in the header comment and in this table, and the licence text at the
   end of the file if it changed.
8. Run the checks below again. LF line endings only.

## The fixture

`fixture-eth.json` is synthetic. Its addresses are hashes of made-up names; only the USDT, USDC, DAI and
WETH contract addresses are real. Labels and amounts are invented. It has:

- 2 seeds, `hops` 2, 30 nodes and 58 edges;
- one loop: seed → hop 1 → hop 1 → back to the seed;
- all six label groups;
- an unpriced token, a partly priced edge (share 0.62) and a look-alike `USDТ` with a Cyrillic letter;
- a hub-stopped node and a not-expanded node;
- `truncated: true`, with one node reporting `edges_dropped` and `usd_dropped`.

## What was tested, and what was not

Tested:

- **Logic, under Node 22 with no DOM** (jsdom is not installed): 46 checks of the page's own data code
  together with the inlined d3-array and d3-sankey. They cover normalising, the filters, the label groups,
  the 90 % rule, cycle breaking and the Sankey layout, path highlighting, the expand merge (including
  merging the same result twice), the model-context totals, CSV escaping with the formula guard, the
  JSON export, and formatting.
- **Hosted mode, in desktop Chrome** (the fixture inlined as hosted data):
  - all three views, the filters and the 90 % fallback;
  - path highlighting and tooltips;
  - the page requests nothing but itself;
  - no console errors;
  - at 375 px wide, no horizontal page scroll.
- **MCP Apps mode, in desktop Chrome**, through a test host page that loads the view in an
  `<iframe sandbox="allow-scripts" srcdoc>` and plays the host side of the messages above:
  - the handshake, tool input and result;
  - Expand, the merge and the model-context update;
  - `ui/open-link`, `ping`, `ui/resource-teardown`, a theme change and full screen;
  - size reports that grow and shrink;
  - a host without `serverTools` (Expand hidden), a failing expand (Expand turned off), an error result;
  - the `change_unknown` and xrp banners.

Not tested:

- any real MCP host;
- the Go side (resource registration, `_meta.ui`, the app-only tool);
- Firefox and Safari;
- graphs much larger than the fixture;
- the hosted page behind a real server and its headers;
- the manual-copy fallback box (copying succeeded in the test host);
- file downloads (the button was not clicked);
- screen readers.
