// Bulk-Asset-Migrator — concurrent bulk asset downloader.
// Single-file Go program. Run with no arguments for an interactive menu,
// or pass a source file plus flags for direct (scriptable) mode.
package main

import (
        "bufio"
        "context"
        "encoding/csv"
        "errors"
        "flag"
        "fmt"
        "io"
        "net/http"
        "net/url"
        "os"
        "os/signal"
        "path"
        "path/filepath"
        "strconv"
        "strings"
        "sync"
        "sync/atomic"
        "syscall"
        "time"
)

const version = "0.2.0"

// =====================================================================
// ANSI color helpers — auto-disable on non-TTY, NO_COLOR, or TERM=dumb
// =====================================================================

var colorEnabled = func() bool {
        if os.Getenv("NO_COLOR") != "" {
                return false
        }
        if t := os.Getenv("TERM"); t == "" || t == "dumb" {
                return false
        }
        fi, err := os.Stdout.Stat()
        if err != nil {
                return false
        }
        return (fi.Mode() & os.ModeCharDevice) != 0
}()

func paint(code, s string) string {
        if !colorEnabled {
                return s
        }
        return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func cBold(s string) string    { return paint("1", s) }
func cDim(s string) string     { return paint("2", s) }
func cRed(s string) string     { return paint("31", s) }
func cGreen(s string) string   { return paint("32", s) }
func cYellow(s string) string  { return paint("33", s) }
func cBlue(s string) string    { return paint("34", s) }
func cMagenta(s string) string { return paint("35", s) }
func cCyan(s string) string    { return paint("36", s) }
func cBoldCyan(s string) string { return paint("1;36", s) }
func cBoldGreen(s string) string { return paint("1;32", s) }
func cBoldRed(s string) string   { return paint("1;31", s) }

// =====================================================================
// Configuration and statistics
// =====================================================================

type Config struct {
        OutputDir    string
        Concurrency  int
        Retries      int
        Timeout      time.Duration
        Headers      []string
        StripN       int
        Flatten      bool
        SkipExisting bool
        BaseURL      string
        Quiet        bool
        Client       *http.Client
}

func defaultConfig() *Config {
        return &Config{
                OutputDir:   "./downloaded_assets",
                Concurrency: 10,
                Retries:     3,
                Timeout:     60 * time.Second,
                Client:      &http.Client{Timeout: 60 * time.Second},
        }
}

type Stats struct {
        OK      int64
        Failed  int64
        Skipped int64
        Total   int
        Elapsed time.Duration
}

// =====================================================================
// Core download engine
// =====================================================================

func Run(ctx context.Context, urls []string, cfg Config) Stats {
        if cfg.Concurrency < 1 {
                cfg.Concurrency = 1
        }
        if cfg.Client == nil {
                cfg.Client = &http.Client{Timeout: cfg.Timeout}
        }
        start := time.Now()
        sem := make(chan struct{}, cfg.Concurrency)
        var wg sync.WaitGroup
        var ok, failed, skipped int64
        var printMu sync.Mutex
        logf := func(format string, a ...any) {
                if cfg.Quiet {
                        return
                }
                printMu.Lock()
                defer printMu.Unlock()
                fmt.Printf(format+"\n", a...)
        }

        for _, u := range urls {
                if ctx.Err() != nil {
                        break
                }
                wg.Add(1)
                sem <- struct{}{}
                go func(u string) {
                        defer wg.Done()
                        defer func() { <-sem }()

                        dest, err := MapPath(u, cfg.OutputDir, cfg.StripN, cfg.Flatten)
                        if err != nil {
                                atomic.AddInt64(&failed, 1)
                                logf("%s  %s  %s", cBoldRed("FAIL"), u, cDim(fmt.Sprintf("(path: %v)", err)))
                                return
                        }
                        if cfg.SkipExisting {
                                if _, err := os.Stat(dest); err == nil {
                                        atomic.AddInt64(&skipped, 1)
                                        logf("%s  %s", cYellow("SKIP"), cDim(dest))
                                        return
                                }
                        }
                        err = WithRetry(cfg.Retries, func(attempt int) error {
                                return downloadOne(ctx, cfg.Client, u, dest, cfg.Headers)
                        })
                        if err != nil {
                                atomic.AddInt64(&failed, 1)
                                logf("%s  %s  %s", cBoldRed("FAIL"), u, cDim(fmt.Sprintf("(%v)", err)))
                                return
                        }
                        atomic.AddInt64(&ok, 1)
                        logf("%s    %s", cBoldGreen("OK"), dest)
                }(u)
        }
        wg.Wait()

        return Stats{
                OK:      ok,
                Failed:  failed,
                Skipped: skipped,
                Total:   len(urls),
                Elapsed: time.Since(start),
        }
}

func downloadOne(ctx context.Context, c *http.Client, rawURL, dest string, headers []string) error {
        req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
        if err != nil {
                return err
        }
        for _, h := range headers {
                k, v, ok := splitHeader(h)
                if ok {
                        req.Header.Add(k, v)
                }
        }
        if req.Header.Get("User-Agent") == "" {
                req.Header.Set("User-Agent", "Bulk-Asset-Migrator/"+version)
        }
        resp, err := c.Do(req)
        if err != nil {
                return err
        }
        defer resp.Body.Close()
        if resp.StatusCode >= 400 {
                return fmt.Errorf("HTTP %d", resp.StatusCode)
        }
        if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
                return err
        }
        tmp := dest + ".part"
        f, err := os.Create(tmp)
        if err != nil {
                return err
        }
        if _, err := io.Copy(f, resp.Body); err != nil {
                f.Close()
                os.Remove(tmp)
                return err
        }
        if err := f.Close(); err != nil {
                os.Remove(tmp)
                return err
        }
        return os.Rename(tmp, dest)
}

// =====================================================================
// Retry with exponential backoff (250ms → 500ms → 1s → 2s → 4s, capped at 5s)
// =====================================================================

func WithRetry(maxRetries int, fn func(attempt int) error) error {
        if maxRetries < 0 {
                maxRetries = 0
        }
        var last error
        for attempt := 0; attempt <= maxRetries; attempt++ {
                if attempt > 0 {
                        delay := time.Duration(1<<uint(attempt-1)) * 250 * time.Millisecond
                        if delay > 5*time.Second {
                                delay = 5 * time.Second
                        }
                        time.Sleep(delay)
                }
                err := fn(attempt)
                if err == nil {
                        return nil
                }
                last = err
        }
        if last == nil {
                last = errors.New("retry exhausted")
        }
        return last
}

// =====================================================================
// URL → local file path mapping
// =====================================================================

func MapPath(rawURL, outputDir string, strip int, flatten bool) (string, error) {
        u, err := url.Parse(rawURL)
        if err != nil {
                return "", err
        }
        if u.Path == "" || u.Path == "/" {
                return "", errors.New("URL has no path component")
        }
        clean := path.Clean(u.Path)
        if flatten {
                base := filepath.Base(clean)
                if base == "." || base == "/" {
                        return "", errors.New("URL has no filename")
                }
                return filepath.Join(outputDir, base), nil
        }
        parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
        if strip > 0 && strip < len(parts) {
                parts = parts[strip:]
        }
        full := append([]string{outputDir}, parts...)
        return filepath.Join(full...), nil
}

// =====================================================================
// Source readers (text file, stdin, CSV column)
// =====================================================================

func readSource(src, csvField, baseURL string) ([]string, error) {
        var r io.ReadCloser
        if src == "-" {
                r = io.NopCloser(os.Stdin)
        } else {
                f, err := os.Open(src)
                if err != nil {
                        return nil, err
                }
                r = f
        }
        defer r.Close()

        var raw []string
        if csvField != "" {
                var err error
                raw, err = readCSV(r, csvField)
                if err != nil {
                        return nil, err
                }
        } else {
                raw = readLines(r)
        }
        return resolveURLs(raw, baseURL)
}

func resolveURLs(raw []string, baseURL string) ([]string, error) {
        var base *url.URL
        if baseURL != "" {
                b, err := url.Parse(baseURL)
                if err != nil {
                        return nil, fmt.Errorf("invalid base URL: %w", err)
                }
                base = b
        }
        out := make([]string, 0, len(raw))
        for _, s := range raw {
                s = strings.TrimSpace(s)
                if s == "" || strings.HasPrefix(s, "#") {
                        continue
                }
                u, err := url.Parse(s)
                if err != nil {
                        continue
                }
                if !u.IsAbs() {
                        if base == nil {
                                return nil, fmt.Errorf("relative URL %q but no base URL set", s)
                        }
                        u = base.ResolveReference(u)
                }
                out = append(out, u.String())
        }
        return out, nil
}

func readLines(r io.Reader) []string {
        var lines []string
        sc := bufio.NewScanner(r)
        sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
        for sc.Scan() {
                lines = append(lines, sc.Text())
        }
        return lines
}

func readCSV(r io.Reader, field string) ([]string, error) {
        cr := csv.NewReader(r)
        cr.FieldsPerRecord = -1
        header, err := cr.Read()
        if err != nil {
                return nil, fmt.Errorf("csv header: %w", err)
        }
        col := -1
        for i, h := range header {
                if strings.EqualFold(strings.TrimSpace(h), field) {
                        col = i
                        break
                }
        }
        if col < 0 {
                return nil, errors.New("csv column not found: " + field)
        }
        var out []string
        for {
                row, err := cr.Read()
                if err == io.EOF {
                        break
                }
                if err != nil {
                        continue
                }
                if col < len(row) {
                        out = append(out, row[col])
                }
        }
        return out, nil
}

func splitHeader(h string) (string, string, bool) {
        idx := strings.IndexByte(h, ':')
        if idx < 0 {
                return "", "", false
        }
        return strings.TrimSpace(h[:idx]), strings.TrimSpace(h[idx+1:]), true
}

// =====================================================================
// Interactive menu
// =====================================================================

var banner = cBoldCyan(`
═══════════════════════════════════════════════════════════
   Bulk-Asset-Migrator  v`+version+`
   concurrent downloader for site-migration asset dumps
═══════════════════════════════════════════════════════════`)

func runMenu(cfg *Config) {
        in := bufio.NewReader(os.Stdin)
        fmt.Println(banner)
        for {
                showSettings(cfg)
                fmt.Println(cBold("What would you like to do?"))
                fmt.Printf("  %s) Download from a URL list (text file)\n", cBoldCyan("1"))
                fmt.Printf("  %s) Download from a CSV column\n", cBoldCyan("2"))
                fmt.Printf("  %s) Paste URLs now (one per line, blank line to finish)\n", cBoldCyan("3"))
                fmt.Printf("  %s) Resume a previous run (skip files already on disk)\n", cBoldCyan("4"))
                fmt.Printf("  %s) Adjust settings\n", cBoldCyan("5"))
                fmt.Printf("  %s) Help / flag reference\n", cBoldCyan("6"))
                fmt.Printf("  %s) Quit\n", cBoldCyan("0"))
                choice := prompt(in, "\n"+cBold("Choose [0-6]: "))
                fmt.Println()
                switch choice {
                case "1":
                        menuDownloadList(in, cfg)
                case "2":
                        menuDownloadCSV(in, cfg)
                case "3":
                        menuPasteURLs(in, cfg)
                case "4":
                        menuResume(in, cfg)
                case "5":
                        menuSettings(in, cfg)
                case "6":
                        showHelp()
                case "0", "q", "quit", "exit":
                        fmt.Println(cDim("bye."))
                        return
                default:
                        fmt.Println(cYellow("invalid choice. pick 0-6."))
                }
                fmt.Println()
        }
}

func showSettings(cfg *Config) {
        pathMode := "preserve full URL path"
        if cfg.Flatten {
                pathMode = "flatten (basename only)"
        } else if cfg.StripN > 0 {
                pathMode = fmt.Sprintf("strip %d leading components", cfg.StripN)
        }
        skip := "no"
        if cfg.SkipExisting {
                skip = "yes"
        }
        base := cfg.BaseURL
        if base == "" {
                base = "(none)"
        }
        fmt.Println(cBold("Current settings:"))
        fmt.Printf("  %s: %s\n", cDim("output dir   "), cCyan(cfg.OutputDir))
        fmt.Printf("  %s: %s\n", cDim("concurrency  "), cCyan(fmt.Sprintf("%d", cfg.Concurrency)))
        fmt.Printf("  %s: %s\n", cDim("retries      "), cCyan(fmt.Sprintf("%d", cfg.Retries)))
        fmt.Printf("  %s: %s\n", cDim("timeout      "), cCyan(cfg.Timeout.String()))
        fmt.Printf("  %s: %s\n", cDim("path mapping "), cCyan(pathMode))
        fmt.Printf("  %s: %s\n", cDim("skip existing"), cCyan(skip))
        fmt.Printf("  %s: %s\n", cDim("base URL     "), cCyan(base))
        fmt.Printf("  %s: %s\n\n", cDim("headers      "), cCyan(fmt.Sprintf("%d set", len(cfg.Headers))))
}

func menuDownloadList(in *bufio.Reader, cfg *Config) {
        src := prompt(in, "Path to URL list file (one per line, # for comments): ")
        if src == "" {
                fmt.Println(cYellow("cancelled."))
                return
        }
        urls, err := readSource(src, "", cfg.BaseURL)
        if err != nil {
                fmt.Println(cRed("error:"), err)
                return
        }
        executeRun(urls, cfg)
}

func menuDownloadCSV(in *bufio.Reader, cfg *Config) {
        src := prompt(in, "Path to CSV file: ")
        if src == "" {
                fmt.Println(cYellow("cancelled."))
                return
        }
        field := prompt(in, "Column name containing URLs: ")
        if field == "" {
                fmt.Println(cYellow("cancelled."))
                return
        }
        urls, err := readSource(src, field, cfg.BaseURL)
        if err != nil {
                fmt.Println(cRed("error:"), err)
                return
        }
        executeRun(urls, cfg)
}

func menuPasteURLs(in *bufio.Reader, cfg *Config) {
        fmt.Println(cBold("Paste URLs (one per line). Empty line to finish:"))
        var lines []string
        for {
                line, err := in.ReadString('\n')
                line = strings.TrimRight(line, "\r\n")
                if line == "" {
                        break
                }
                lines = append(lines, line)
                if err != nil {
                        break
                }
        }
        urls, err := resolveURLs(lines, cfg.BaseURL)
        if err != nil {
                fmt.Println(cRed("error:"), err)
                return
        }
        executeRun(urls, cfg)
}

func menuResume(in *bufio.Reader, cfg *Config) {
        src := prompt(in, "Path to URL list (text or CSV): ")
        if src == "" {
                fmt.Println(cYellow("cancelled."))
                return
        }
        field := prompt(in, "CSV column name (leave blank if plain text): ")
        prev := cfg.SkipExisting
        cfg.SkipExisting = true
        urls, err := readSource(src, field, cfg.BaseURL)
        if err != nil {
                cfg.SkipExisting = prev
                fmt.Println(cRed("error:"), err)
                return
        }
        executeRun(urls, cfg)
        cfg.SkipExisting = prev
}

func menuSettings(in *bufio.Reader, cfg *Config) {
        for {
                fmt.Println(cBold("Settings:"))
                fmt.Printf("  %s) Output directory       [%s]\n", cBoldCyan("1"), cCyan(cfg.OutputDir))
                fmt.Printf("  %s) Concurrency            [%s]\n", cBoldCyan("2"), cCyan(fmt.Sprintf("%d", cfg.Concurrency)))
                fmt.Printf("  %s) Retries                [%s]\n", cBoldCyan("3"), cCyan(fmt.Sprintf("%d", cfg.Retries)))
                fmt.Printf("  %s) Timeout                [%s]\n", cBoldCyan("4"), cCyan(cfg.Timeout.String()))
                fmt.Printf("  %s) Add HTTP header        (current: %s)\n", cBoldCyan("5"), cCyan(fmt.Sprintf("%d", len(cfg.Headers))))
                fmt.Printf("  %s) Clear all headers\n", cBoldCyan("6"))
                fmt.Printf("  %s) Toggle skip-existing   [%s]\n", cBoldCyan("7"), cCyan(fmt.Sprintf("%v", cfg.SkipExisting)))
                fmt.Printf("  %s) Path mapping mode      [strip=%d flatten=%v]\n", cBoldCyan("8"), cfg.StripN, cfg.Flatten)
                fmt.Printf("  %s) Base URL for relative  [%s]\n", cBoldCyan("9"), cCyan(orNone(cfg.BaseURL)))
                fmt.Printf("  %s) Back\n", cBoldCyan("0"))
                choice := prompt(in, cBold("Choose [0-9]: "))
                fmt.Println()
                switch choice {
                case "1":
                        cfg.OutputDir = promptStr(in, "Output directory", cfg.OutputDir)
                case "2":
                        cfg.Concurrency = promptInt(in, "Concurrency", cfg.Concurrency)
                case "3":
                        cfg.Retries = promptInt(in, "Retries", cfg.Retries)
                case "4":
                        cfg.Timeout = promptDuration(in, "Timeout", cfg.Timeout)
                        cfg.Client = &http.Client{Timeout: cfg.Timeout}
                case "5":
                        h := prompt(in, "Header (Name: value): ")
                        if _, _, ok := splitHeader(h); ok {
                                cfg.Headers = append(cfg.Headers, h)
                                fmt.Println(cGreen("added."))
                        } else if h != "" {
                                fmt.Println(cYellow("invalid header (need 'Name: value')."))
                        }
                case "6":
                        cfg.Headers = nil
                        fmt.Println(cGreen("headers cleared."))
                case "7":
                        cfg.SkipExisting = !cfg.SkipExisting
                        fmt.Printf("skip-existing = %s\n", cCyan(fmt.Sprintf("%v", cfg.SkipExisting)))
                case "8":
                        menuPathMode(in, cfg)
                case "9":
                        cfg.BaseURL = promptStr(in, "Base URL for resolving relative paths (blank = none)", cfg.BaseURL)
                case "0", "":
                        return
                default:
                        fmt.Println(cYellow("invalid choice."))
                }
                fmt.Println()
        }
}

func menuPathMode(in *bufio.Reader, cfg *Config) {
        fmt.Println(cBold("Path mapping mode:"))
        fmt.Printf("  %s) Preserve full URL path\n", cBoldCyan("1"))
        fmt.Printf("  %s) Strip N leading components\n", cBoldCyan("2"))
        fmt.Printf("  %s) Flatten (basename only)\n", cBoldCyan("3"))
        c := prompt(in, cBold("Choose [1-3]: "))
        switch c {
        case "1":
                cfg.StripN = 0
                cfg.Flatten = false
        case "2":
                cfg.Flatten = false
                cfg.StripN = promptInt(in, "How many leading components to strip", cfg.StripN)
        case "3":
                cfg.Flatten = true
                cfg.StripN = 0
        default:
                fmt.Println(cYellow("kept current setting."))
        }
}

func executeRun(urls []string, cfg *Config) {
        if len(urls) == 0 {
                fmt.Println(cYellow("no URLs to download."))
                return
        }
        fmt.Printf("%s %s URLs queued, starting...\n\n", cBoldCyan("→"), cBold(fmt.Sprintf("%d", len(urls))))
        ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
        defer cancel()
        stats := Run(ctx, urls, *cfg)
        fmt.Printf("\n%s  %s  %s  %s  elapsed=%s\n",
                cBoldGreen(fmt.Sprintf("ok=%d", stats.OK)),
                cBoldRed(fmt.Sprintf("failed=%d", stats.Failed)),
                cYellow(fmt.Sprintf("skipped=%d", stats.Skipped)),
                cBold(fmt.Sprintf("total=%d", stats.Total)),
                cCyan(stats.Elapsed.Truncate(time.Millisecond).String()))
}

func showHelp() {
        fmt.Println(cBold("Direct (scriptable) mode:"))
        fmt.Println("  " + cCyan("bulk-asset-migrator [flags] <source>"))
        fmt.Println()
        fmt.Println("  " + cDim("source: '-' for stdin, otherwise a path to a text or CSV file."))
        fmt.Println()
        fmt.Println(cBold("Flags:"))
        fmt.Println("  " + cCyan("-o DIR") + "             output directory")
        fmt.Println("  " + cCyan("-c N") + "               parallel downloads")
        fmt.Println("  " + cCyan("-r N") + "               retry count per file")
        fmt.Println("  " + cCyan("-t DUR") + "             per-request timeout (e.g. 30s)")
        fmt.Println("  " + cCyan("-u URL") + "             base URL for relative inputs")
        fmt.Println("  " + cCyan("-H \"Name: value\"") + "   extra HTTP header (repeatable)")
        fmt.Println("  " + cCyan("--csv FIELD") + "        read URLs from named CSV column")
        fmt.Println("  " + cCyan("--strip N") + "          strip N leading path components")
        fmt.Println("  " + cCyan("--flatten") + "          drop directory structure")
        fmt.Println("  " + cCyan("--skip-existing") + "    skip files that already exist")
        fmt.Println("  " + cCyan("--no-color") + "         disable ANSI colors")
        fmt.Println("  " + cCyan("-q") + "                 only print summary")
        fmt.Println("  " + cCyan("--version") + "          print version")
}

func prompt(in *bufio.Reader, msg string) string {
        fmt.Print(msg)
        line, err := in.ReadString('\n')
        line = strings.TrimRight(line, "\r\n ")
        if errors.Is(err, io.EOF) && line == "" {
                fmt.Println(cDim("\nEOF — bye."))
                os.Exit(0)
        }
        return line
}

func promptStr(in *bufio.Reader, msg, current string) string {
        v := prompt(in, fmt.Sprintf("%s [%s]: ", msg, current))
        if v == "" {
                return current
        }
        return v
}

func promptInt(in *bufio.Reader, msg string, current int) int {
        v := prompt(in, fmt.Sprintf("%s [%d]: ", msg, current))
        if v == "" {
                return current
        }
        n, err := strconv.Atoi(v)
        if err != nil {
                fmt.Println("not a number, kept current.")
                return current
        }
        return n
}

func promptDuration(in *bufio.Reader, msg string, current time.Duration) time.Duration {
        v := prompt(in, fmt.Sprintf("%s [%s]: ", msg, current))
        if v == "" {
                return current
        }
        d, err := time.ParseDuration(v)
        if err != nil {
                fmt.Println("invalid duration (use e.g. 30s, 2m), kept current.")
                return current
        }
        return d
}

func orNone(s string) string {
        if s == "" {
                return "(none)"
        }
        return s
}

// =====================================================================
// Direct (flag) mode entry — preserved for scripting / cron / CI
// =====================================================================

type headerFlag []string

func (h *headerFlag) String() string { return strings.Join(*h, ", ") }
func (h *headerFlag) Set(v string) error {
        if !strings.Contains(v, ":") {
                return fmt.Errorf("header must be in 'Name: value' form, got %q", v)
        }
        *h = append(*h, v)
        return nil
}

func main() {
        var (
                outputDir   = flag.String("o", "./downloaded_assets", "output directory")
                concurrency = flag.Int("c", 10, "parallel downloads")
                retries     = flag.Int("r", 3, "retry count per file")
                timeout     = flag.Duration("t", 60*time.Second, "per-request timeout")
                baseURL     = flag.String("u", "", "base URL for resolving relative inputs")
                csvField    = flag.String("csv", "", "read URLs from named column in a CSV file")
                stripN      = flag.Int("strip", 0, "strip N leading path components")
                flatten     = flag.Bool("flatten", false, "drop directory structure, save by basename")
                skipExist   = flag.Bool("skip-existing", false, "skip files that already exist on disk")
                quiet       = flag.Bool("q", false, "only print summary")
                noColor     = flag.Bool("no-color", false, "disable ANSI color output")
                showVersion = flag.Bool("version", false, "print version and exit")
                headers     headerFlag
        )
        flag.Var(&headers, "H", "extra HTTP header (repeatable; e.g. -H 'Cookie: x=y')")
        flag.Usage = func() {
                fmt.Fprintf(os.Stderr, `Bulk-Asset-Migrator %s — concurrent downloader for site-migration asset dumps

Usage:
  bulk-asset-migrator                       launch the interactive menu
  bulk-asset-migrator [flags] <source>      direct mode (script-friendly)

  source: '-' for stdin, otherwise a path to a text file with one URL per line.
          Lines starting with '#' are ignored.
          With --csv FIELD, the file is parsed as CSV and URLs come from that column.

Flags:
`, version)
                flag.PrintDefaults()
        }
        flag.Parse()

        if *noColor {
                colorEnabled = false
                banner = `
═══════════════════════════════════════════════════════════
   Bulk-Asset-Migrator  v` + version + `
   concurrent downloader for site-migration asset dumps
═══════════════════════════════════════════════════════════`
        }

        if *showVersion {
                fmt.Println("Bulk-Asset-Migrator", version)
                return
        }

        args := flag.Args()
        if len(args) == 0 {
                // No source argument → interactive menu.
                cfg := defaultConfig()
                runMenu(cfg)
                return
        }
        if len(args) != 1 {
                flag.Usage()
                os.Exit(2)
        }

        urls, err := readSource(args[0], *csvField, *baseURL)
        if err != nil {
                fmt.Fprintln(os.Stderr, "source error:", err)
                os.Exit(1)
        }
        if len(urls) == 0 {
                fmt.Fprintln(os.Stderr, "no URLs to download")
                os.Exit(1)
        }

        ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
        defer cancel()

        cfg := Config{
                OutputDir:    *outputDir,
                Concurrency:  *concurrency,
                Retries:      *retries,
                Timeout:      *timeout,
                Headers:      headers,
                StripN:       *stripN,
                Flatten:      *flatten,
                SkipExisting: *skipExist,
                Quiet:        *quiet,
                Client:       &http.Client{Timeout: *timeout},
        }

        if !*quiet {
                fmt.Printf("%s %s URLs, concurrency=%s, output=%s\n",
                        cBoldCyan("→"), cBold(fmt.Sprintf("%d", len(urls))),
                        cCyan(fmt.Sprintf("%d", *concurrency)), cCyan(*outputDir))
        }

        stats := Run(ctx, urls, cfg)

        if !*quiet {
                fmt.Println()
        }
        fmt.Printf("%s  %s  %s  %s  elapsed=%s\n",
                cBoldGreen(fmt.Sprintf("ok=%d", stats.OK)),
                cBoldRed(fmt.Sprintf("failed=%d", stats.Failed)),
                cYellow(fmt.Sprintf("skipped=%d", stats.Skipped)),
                cBold(fmt.Sprintf("total=%d", stats.Total)),
                cCyan(stats.Elapsed.Truncate(time.Millisecond).String()))

        if stats.Failed > 0 {
                os.Exit(1)
        }
}
