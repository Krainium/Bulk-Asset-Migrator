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
        "strings"
        "sync"
        "sync/atomic"
        "syscall"
        "time"
)

const version = "0.2.0"

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
                                logf("FAIL  %s  (path: %v)", u, err)
                                return
                        }
                        if cfg.SkipExisting {
                                if _, err := os.Stat(dest); err == nil {
                                        atomic.AddInt64(&skipped, 1)
                                        logf("SKIP  %s", dest)
                                        return
                                }
                        }
                        err = WithRetry(cfg.Retries, func(attempt int) error {
                                return downloadOne(ctx, cfg.Client, u, dest, cfg.Headers)
                        })
                        if err != nil {
                                atomic.AddInt64(&failed, 1)
                                logf("FAIL  %s  (%v)", u, err)
                                return
                        }
                        atomic.AddInt64(&ok, 1)
                        logf("OK    %s", dest)
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
  // Direct (flag) mode entry — single CLI surface for v1
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
  		showVersion = flag.Bool("version", false, "print version and exit")
  		headers     headerFlag
  	)
  	flag.Var(&headers, "H", "extra HTTP header (repeatable; e.g. -H 'Cookie: x=y')")
  	flag.Usage = func() {
  		fmt.Fprintf(os.Stderr, `Bulk-Asset-Migrator %s — concurrent downloader for site-migration asset dumps

  Usage:
    bulk-asset-migrator [flags] <source>

    source: '-' for stdin, otherwise a path to a text file with one URL per line.
            Lines starting with '#' are ignored.
            With --csv FIELD, the file is parsed as CSV and URLs come from that column.

  Flags:
  `, version)
  		flag.PrintDefaults()
  	}
  	flag.Parse()

  	if *showVersion {
  		fmt.Println("Bulk-Asset-Migrator", version)
  		return
  	}

  	args := flag.Args()
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
  		fmt.Printf("→ %d URLs, concurrency=%d, output=%s\n", len(urls), *concurrency, *outputDir)
  	}

  	stats := Run(ctx, urls, cfg)

  	if !*quiet {
  		fmt.Println()
  	}
  	fmt.Printf("ok=%d  failed=%d  skipped=%d  total=%d  elapsed=%s\n",
  		stats.OK, stats.Failed, stats.Skipped, stats.Total, stats.Elapsed.Truncate(time.Millisecond))

  	if stats.Failed > 0 {
  		os.Exit(1)
  	}
  }
  