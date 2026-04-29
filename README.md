# Bulk-Asset-Migrator 📦

Concurrent downloader for site-migration asset dumps. One static binary, one menu, no SaaS in the middle.

## ⚡ Why this exists

You're moving away from a hosted CMS — a portfolio off Squarespace, a store off Shopify, a blog off Wix. The platform's "export" hands you HTML and JSON, but every image, video, and PDF still lives on the host's CDN, scattered across thousands of URLs you don't control. The browser dev-tools approach buckles after the first hundred files. A `curl` loop runs all night, forgets which ones succeeded, and writes half-finished files when the connection blips. The paid migration services want full admin access to your account.

Bulk-Asset-Migrator is the small, sharp tool for that exact pain. You give it a list of URLs — from your export file, from a sitemap, from a CSV column. It downloads them in parallel, retries when the CDN times out, preserves the folder structure, and writes one file per URL into a directory you can hand straight to the next host. Run it again with skip-existing and it picks up where it left off.

## 🚀 Setup

```bash
git clone https://github.com/Krainium/Bulk-Asset-Migrator
cd Bulk-Asset-Migrator
go build -o bulk-asset-migrator
./bulk-asset-migrator
```

That's it. Requires Go 1.22 or newer. The binary is statically linked, around 5 MB, and runs anywhere Go targets.

## 🖥️ Two ways to run

### Interactive menu (default)

Run `./bulk-asset-migrator` with no arguments and the menu appears. Numbers and headings are colorized in supported terminals:

```
═══════════════════════════════════════════════════════════
   Bulk-Asset-Migrator  v0.2.0
   concurrent downloader for site-migration asset dumps
═══════════════════════════════════════════════════════════
Current settings:
  output dir   : ./downloaded_assets
  concurrency  : 10
  retries      : 3
  timeout      : 1m0s
  path mapping : preserve full URL path
  skip existing: no
  base URL     : (none)
  headers      : 0 set

What would you like to do?
  1) Download from a URL list (text file)
  2) Download from a CSV column
  3) Paste URLs now (one per line, blank line to finish)
  4) Resume a previous run (skip files already on disk)
  5) Adjust settings
  6) Help / flag reference
  0) Quit

Choose [0-6]:
```

Pick a number, answer the follow-up prompts, watch the downloads stream. The settings menu lets you tune concurrency, add auth headers, switch path-mapping mode, set a base URL for relative paths, and so on — without ever leaving the program.

### Direct mode (for scripts and cron)

Pass a source file plus flags and the tool runs once and exits:

```bash
./bulk-asset-migrator urls.txt
./bulk-asset-migrator --csv image_url -o ./media -c 20 export.csv
./bulk-asset-migrator --skip-existing urls.txt
echo "/wp-content/uploads/2024/05/hero.jpg" | ./bulk-asset-migrator -u https://oldsite.com -
```

Exit code is `0` on a clean run and `1` when any download failed, so you can chain it safely.

## 📺 Live output (real run, 14 URLs)

Live capture from a real run against GitHub raw + httpbin endpoints. Successes appear in **green**, failures in **red**, skipped files in **yellow**, summary counts highlighted:

```
→ 14 URLs, concurrency=8, output=out_demo
OK    out_demo/torvalds/linux/master/README
OK    out_demo/golang/go/master/README.md
OK    out_demo/golang/go/master/CONTRIBUTING.md
OK    out_demo/torvalds/linux/master/COPYING
OK    out_demo/rust-lang/rust/master/LICENSE-MIT
OK    out_demo/rust-lang/rust/master/README.md
OK    out_demo/golang/go/master/LICENSE
OK    out_demo/rust-lang/rust/master/LICENSE-APACHE
OK    out_demo/bytes/4096
OK    out_demo/bytes/16384
OK    out_demo/bytes/1024
FAIL  https://httpbin.org/status/404  (HTTP 404)
OK    out_demo/bytes/65536
FAIL  https://httpbin.org/status/500  (HTTP 500)

ok=12  failed=2  skipped=0  total=14  elapsed=1.053s
```

Re-run with `--skip-existing` and the already-downloaded files are detected and skipped:

```
ok=0  failed=2  skipped=12  total=14  elapsed=342ms
```

Colors auto-disable when stdout is piped to a file or another process, when `NO_COLOR=1` is set, when `TERM=dumb`, or when you pass `--no-color`. So your logs and `grep` pipelines stay clean.

## 🎛️ Flags (direct mode)

| Flag | Default | Purpose |
|---|---|---|
| `-o DIR` | `./downloaded_assets` | output directory |
| `-c N` | `10` | parallel downloads |
| `-r N` | `3` | retry count per file (exponential backoff, 250ms → 5s) |
| `-t DUR` | `60s` | per-request timeout |
| `-u URL` |  | base URL for resolving relative inputs |
| `-H "K: V"` |  | extra HTTP header, repeatable |
| `--csv FIELD` |  | parse source as CSV, take URLs from this column |
| `--strip N` | `0` | strip N leading path components when mapping URL → file |
| `--flatten` | `false` | drop directory structure, save by basename |
| `--skip-existing` | `false` | skip files that already exist on disk |
| `--no-color` | `false` | disable ANSI color output |
| `-q` | `false` | only print summary |
| `--version` |  | print version |

## 📂 How URLs become file paths

By default, the URL path becomes the local path under your output directory.

```
https://cdn.example.com/2024/05/post/cover.webp
                       └────────────┬────────────┘
                                    └─→ ./downloaded_assets/2024/05/post/cover.webp
```

Switch path-mapping mode in the settings menu (or pass `--strip N` / `--flatten` in direct mode):

- **Preserve** keeps the full path
- **Strip N** removes the first N components (drop `/v1/cdn/` prefixes)
- **Flatten** keeps only the filename

## 🛟 Behavior worth knowing

- Downloads stream to a `.part` file and rename on success, so you never get half-written files in your output tree
- Failed downloads are retried with exponential backoff (250ms, 500ms, 1s, 2s, 4s, capped at 5s)
- HTTP 4xx and 5xx are treated as errors and counted toward the retry budget
- Ctrl-C cancels in-flight requests cleanly via context
- Stdin EOF in interactive mode exits cleanly instead of looping
- The default user agent is `Bulk-Asset-Migrator/<version>`. Override it with `-H "User-Agent: ..."`

## 🛠️ Built with

Standard library only. No third-party dependencies. Single source file, single binary, around 5 MB. Runs anywhere Go targets.
