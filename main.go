package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/minio/selfupdate"
)

// --- Configuration & Globals ---

var (
	version, gitCommit, buildDate, goVersion, updateURL string

	// Flags
	verboseMode      bool
	updateFlag       bool
	versionFlag      bool
	bookConcurrency  int
	trackConcurrency int

	// Regex Patterns
	jsonRegex        = regexp.MustCompile(`BookController\.enter\((.*?)\);`)
	descRegex        = regexp.MustCompile(`bookDescription\">(.+?)</div>`)
	forbiddenChars   = regexp.MustCompile(`[<>:"/\\|?*]`)
	bookItemRegex    = regexp.MustCompile(`(?s)<div class="bookitem">(.*?)</div>\s*</div>`)
	bookURLRegex     = regexp.MustCompile(`(?s)class="bookitem_cover"\s+href="/(book/[^"]+)"`)
	bookIndexRegex   = regexp.MustCompile(`(?s)<span class="bookitem_serie_index">\s*([\d\.]+)\.\s*</span>`)
	bookTitleRegex   = regexp.MustCompile(`(?s)<span class="bookitem_serie_index">\s*[\d\.]+\.\s*</span>\s*([^<]+)`)
	singleIndexRegex = regexp.MustCompile(`class="book_info_line_serie_index">\(([\d\.]+)\)`)
	singleTitleRegex = regexp.MustCompile(`<h2 class="book_title"[^>]*>(.*?)</h2>`)
)

const (
	userAgent     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36 Edg/135.0.0.0"
	maxRetries    = 15
	retryDelay    = 2 * time.Second
)

// --- HTTP Client with Custom DNS ---

var httpClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        10,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, _ := net.SplitHostPort(addr)
			// Try direct IP first
			if net.ParseIP(host) != nil {
				return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
			}
			// Custom DNS Fallback (Google, Cloudflare)
			resolvers := []*net.Resolver{
				net.DefaultResolver,
				{PreferGo: true, Dial: func(ctx context.Context, n, a string) (net.Conn, error) { return net.Dial("udp", "8.8.8.8:53") }},
				{PreferGo: true, Dial: func(ctx context.Context, n, a string) (net.Conn, error) { return net.Dial("udp", "1.1.1.1:53") }},
			}
			for _, r := range resolvers {
				if ips, err := r.LookupHost(ctx, host); err == nil && len(ips) > 0 {
					target := ips[0]
					if strings.Contains(target, ":") {
						target = "[" + target + "]" // IPv6
					}
					return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, target+":"+port)
				}
			}
			return nil, fmt.Errorf("DNS resolution failed for %s", host)
		},
	},
}

// --- Data Structures ---

type Book struct {
	Name    string      `json:"name"`
	Authors interface{} `json:"authors"`
	Readers interface{} `json:"readers"`
	Cover   string      `json:"cover"`
	URL     string      `json:"url"`
}

type Audio struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type BookData struct {
	Book           Book    `json:"book"`
	Playlist       []Audio `json:"playlist"`
	MergedPlaylist []Audio `json:"merged_playlist"`
	CoverAlt       string  `json:"-"`
	Description    string  `json:"-"`
}

type BookInfo struct {
	URL, DisplayName, SeriesIndex string
}

type BookStatus string

const (
	StatusPending     BookStatus = "pending"
	StatusDownloading BookStatus = "downloading"
	StatusCompleted   BookStatus = "completed"
	StatusFailed      BookStatus = "failed"
)

type DownloadState struct {
	Books map[string]BookStatus `json:"books"`
	mu    sync.Mutex            `json:"-"`
	path  string                `json:"-"`
}

func (s *DownloadState) Update(book string, status BookStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Books[book] = status
	// Atomic write style to prevent corruption
	data, _ := json.MarshalIndent(s, "", "  ")
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err == nil {
		os.Rename(tmp, s.path)
	}
}

func (s *DownloadState) Get(book string) BookStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if val, ok := s.Books[book]; ok {
		return val
	}
	return StatusPending
}

// --- Logger Helper ---

func debug(format string, v ...interface{}) {
	if verboseMode {
		log.Printf("[DEBUG] "+format, v...)
	}
}

// --- Main ---

func main() {
	setupFlags()

	if versionFlag {
		fmt.Printf("Ver: %s, Commit: %s, Date: %s, Go: %s\n", version, gitCommit, buildDate, goVersion)
		os.Exit(0)
	}

	if updateFlag {
		handleUpdate()
		os.Exit(0)
	}

	args := flag.Args()
	if len(args) < 2 {
		fmt.Println("Usage: go-knigavuhe [flags] <output-dir> <url-or-file>")
		fmt.Println("Flags:")
		flag.PrintDefaults()
		os.Exit(1)
	}

	outputDir, inputArg := args[0], args[1]
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		log.Fatalf("❌ Failed to create output directory: %v", err)
	}

	// Initialize State
	statePath := filepath.Join(outputDir, "state.json")
	state := &DownloadState{Books: make(map[string]BookStatus), path: statePath}
	if data, err := os.ReadFile(statePath); err == nil {
		json.Unmarshal(data, state)
	}

	// Parse Input
	books := parseInput(inputArg)
	if len(books) == 0 {
		log.Fatal("❌ No books found in input.")
	}

	fmt.Printf("🚀 Processing %d books using %d book-workers and %d track-workers\n", len(books), bookConcurrency, trackConcurrency)

	// Worker Pool
	var wg sync.WaitGroup
	bookChan := make(chan BookInfo, len(books))

	workers := bookConcurrency
	if workers < 1 {
		workers = 1
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for book := range bookChan {
				processBook(book, outputDir, state)
			}
		}(i)
	}

	for _, book := range books {
		bookChan <- book
	}
	close(bookChan)
	wg.Wait()

	// Clean up state if everything is clean
	cleanState(state, statePath)
}

func setupFlags() {
	if runtime.GOOS == "windows" {
		// Cleanup old binary from previous update
		os.Remove("." + os.Args[0] + ".old")
	}

	flag.BoolVar(&verboseMode, "verbose", false, "Show detailed debug logs")
	flag.BoolVar(&versionFlag, "version", false, "Print version")
	flag.BoolVar(&updateFlag, "update", false, "Check for updates, apply if available, and exit")
	flag.IntVar(&bookConcurrency, "book-workers", 1, "Number of books to download in parallel")
	flag.IntVar(&trackConcurrency, "track-workers", 5, "Number of tracks to download in parallel per book")
	flag.Parse()
}

// --- Update Logic ---

func handleUpdate() {
	if updateURL == "" {
		log.Fatal("❌ Update URL not configured in build.")
	}

	fmt.Println("🔍 Checking for updates...")

	// Check version file
	verURL := strings.TrimRight(updateURL, "/") + "/version.txt"
	verResp, err := fetchURL(verURL)
	if err != nil {
		log.Fatalf("❌ Failed to check version: %v", err)
	}
	remoteVer := string(bytes.TrimSpace(verResp))

	if remoteVer == version {
		fmt.Println("✅ Already up to date.")
		return
	}

	fmt.Printf("⬇️  Found new version %s (current: %s). Downloading...\n", remoteVer, version)

	binName := fmt.Sprintf("go-knigavuhe-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binURL := strings.TrimRight(updateURL, "/") + "/" + binName

	resp, err := httpClient.Get(binURL)
	if err != nil {
		log.Fatalf("❌ Download failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Fatalf("❌ Update server returned HTTP %d", resp.StatusCode)
	}

	err = selfupdate.Apply(resp.Body, selfupdate.Options{})
	if err != nil {
		log.Fatalf("❌ Apply update failed: %v", err)
	}

	fmt.Println("✅ Update applied successfully. Please restart.")
}

// --- Processing Logic ---

func parseInput(input string) []BookInfo {
	var books []BookInfo
	var lines []string

	// Check if file
	if stat, err := os.Stat(input); err == nil && !stat.IsDir() {
		f, _ := os.Open(input)
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			if t := strings.TrimSpace(scanner.Text()); t != "" {
				lines = append(lines, t)
			}
		}
	} else {
		lines = append(lines, input)
	}

	for _, line := range lines {
		if strings.Contains(line, "/serie/") {
			if seriesBooks, err := extractBooksFromSeries(line); err == nil {
				books = append(books, seriesBooks...)
			} else {
				fmt.Printf("⚠️  Failed to parse series %s: %v\n", line, err)
			}
		} else {
			books = append(books, BookInfo{URL: line, DisplayName: "Unknown", SeriesIndex: ""})
		}
	}
	return books
}

func processBook(book BookInfo, outputDir string, state *DownloadState) {
	// If we don't know the name yet, we might need to fetch the page to check status,
	// but strictly speaking, we check status after resolving the name.

	// Fast check if we already have the name from series parsing
	if book.DisplayName != "Unknown" && state.Get(book.DisplayName) == StatusCompleted {
		fmt.Printf("⏭️  Skipping %s (Done)\n", book.DisplayName)
		return
	}

	err := downloadBook(book.URL, outputDir, book.DisplayName, state)
	if err != nil {
		name := book.DisplayName
		if name == "Unknown" {
			name = book.URL
		}
		state.Update(name, StatusFailed)
		// Explicit newline if single worker to break progress bars
		if bookConcurrency == 1 && !verboseMode {
			fmt.Println()
		}
		fmt.Printf("❌ %s failed: %v\n", name, err)
	}
}

func downloadBook(bookURL, outputDir, providedName string, state *DownloadState) error {
	debug("Fetching book page: %s", bookURL)

	htmlBytes, err := fetchURL(bookURL)
	if err != nil {
		return fmt.Errorf("page load error: %w", err)
	}
	html := string(htmlBytes)

	// 1. Resolve Name
	finalName := resolveBookName(providedName, html, bookURL)
	debug("Resolved name: %s", finalName)

	if state.Get(finalName) == StatusCompleted {
		fmt.Printf("⏭️  Skipping %s\n", finalName)
		return nil
	}

	state.Update(finalName, StatusDownloading)

	// 2. Parse Metadata (JSON)
	matches := jsonRegex.FindStringSubmatch(html)
	if len(matches) < 2 {
		return fmt.Errorf("metadata parsing failed (JSON signature not found - site layout changed?)")
	}

	data := &BookData{}
	if err := json.Unmarshal([]byte(matches[1]), data); err != nil {
		return fmt.Errorf("metadata JSON decode error: %w", err)
	}

	// 3. Setup Directory
	bookDir := filepath.Join(outputDir, sanitizePath(finalName))
	if err := os.MkdirAll(bookDir, 0755); err != nil {
		return err
	}

	// 4. Save Metadata/Cover
	data.CoverAlt = strings.Split(data.Book.Cover, "-")[0] + ".jpg"
	if d := descRegex.FindStringSubmatch(html); len(d) > 1 {
		data.Description = regexp.MustCompile(`<[^>]*>`).ReplaceAllString(d[1], "")
	}
	saveDescription(bookDir, data)
	downloadFileWithFallback([]string{data.CoverAlt, data.Book.Cover}, filepath.Join(bookDir, "cover.jpg"))

	// 5. Download Tracks
	trackCount := len(data.Playlist)
	if bookConcurrency > 1 || verboseMode {
		fmt.Printf("⬇️  Starting %s (%d tracks)\n", finalName, trackCount)
	} else {
		// Clean header for progress bar
		fmt.Printf("\n📂 %s (%d tracks)\n", finalName, trackCount)
	}

	// Strategy: Try standard playlist, if fail, try merged
	errStandard := downloadTracks(bookDir, data.Playlist, finalName)
	if errStandard == nil {
		state.Update(finalName, StatusCompleted)
		return nil
	}

	debug("Standard playlist failed for %s: %v. Attempting merged playlist...", finalName, errStandard)

	if len(data.MergedPlaylist) > 0 {
		errMerged := downloadTracks(bookDir, data.MergedPlaylist, finalName)
		if errMerged == nil {
			state.Update(finalName, StatusCompleted)
			return nil
		}
		// Consolidate errors
		return fmt.Errorf("[All download methods failed] Standard: %v | Merged: %v", errStandard, errMerged)
	}

	return fmt.Errorf("[All download methods failed] Standard: %v | No merged playlist available", errStandard)
}

func downloadTracks(dir string, tracks []Audio, bookName string) error {
	if len(tracks) == 0 {
		return fmt.Errorf("empty playlist")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sem := make(chan struct{}, trackConcurrency)
	errChan := make(chan error, len(tracks)) // buffer to prevent blocking
	var wg sync.WaitGroup

	var completed int32 = 0
	total := int32(len(tracks))

	// Only show progress bar if not verbose and not parallel books
	showProgress := !verboseMode && bookConcurrency == 1

	if showProgress {
		fmt.Printf("\r⏳ [%s] 0/%d", bookName, total)
	}

	for _, t := range tracks {
		wg.Add(1)
		go func(track Audio) {
			defer wg.Done()

			// Check if already failed
			select {
			case <-ctx.Done():
				return
			case sem <- struct{}{}: // Acquire token
			}
			defer func() { <-sem }() // Release token

			// Determine Filename
			ext := filepath.Ext(strings.Split(track.URL, "?")[0])
			if ext == "" {
				ext = ".mp3"
			}
			fname := sanitizePath(track.Title) + strings.ToLower(ext)
			dest := filepath.Join(dir, fname)

			// Download Loop
			var lastErr error
			success := false

			// Skip if exists
			if s, err := os.Stat(dest); err == nil && s.Size() > 0 {
				success = true
			} else {
				for i := 0; i < maxRetries; i++ {
					if err := downloadFile(track.URL, dest); err == nil {
						success = true
						break
					} else {
						lastErr = err
						debug("Retry %d/%d for '%s': %v", i+1, maxRetries, track.Title, err)
						time.Sleep(retryDelay)
					}
				}
			}

			if success {
				if showProgress {
					c := atomic.AddInt32(&completed, 1)
					p := float64(c) / float64(total) * 100
					fmt.Printf("\r⏳ [%s] %d/%d (%.0f%%)", bookName, c, total, p)
				}
			} else {
				// Report failure
				select {
				case errChan <- fmt.Errorf("track '%s': %v", track.Title, lastErr):
					cancel() // Stop other downloads for this book
				default:
				}
			}
		}(t)
	}

	wg.Wait()
	close(errChan)

	// Check if any error occurred
	if err, ok := <-errChan; ok {
		return err
	}

	if showProgress {
		fmt.Printf("\r✅ [%s] Completed %d tracks.          \n", bookName, total)
	} else if bookConcurrency > 1 {
		fmt.Printf("✅ Completed %s\n", bookName)
	}
	return nil
}

func downloadFile(url string, dest string) error {
	part := dest + ".part"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	f, err := os.Create(part)
	if err != nil {
		return err
	}

	_, err = io.Copy(f, resp.Body)
	f.Close()

	if err != nil {
		os.Remove(part)
		return err
	}

	return os.Rename(part, dest)
}

func downloadFileWithFallback(urls []string, dest string) {
	for _, u := range urls {
		if u == "" {
			continue
		}
		if err := downloadFile(u, dest); err == nil {
			debug("Downloaded aux file: %s", dest)
			return
		}
	}
	debug("Failed to download aux file: %s", dest)
}

// --- Helpers ---

func fetchURL(u string) ([]byte, error) {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Cookie", "new_design=1") // Force new design for consistent regex

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func resolveBookName(providedName, html, urlPath string) string {
	if providedName != "Unknown" && providedName != "" {
		if !strings.Contains(providedName, "_") {
			// Try to append index if provided name lacks it
			if idxMatch := singleIndexRegex.FindStringSubmatch(html); len(idxMatch) > 1 {
				return strings.TrimSpace(idxMatch[1]) + "_" + providedName
			}
		}
		return providedName
	}

	// Parsing page for title
	idxMatch := singleIndexRegex.FindStringSubmatch(html)
	titleMatch := singleTitleRegex.FindStringSubmatch(html)

	if len(titleMatch) > 1 {
		name := strings.TrimSpace(titleMatch[1])
		if len(idxMatch) > 1 {
			return strings.TrimSpace(idxMatch[1]) + "_" + name
		}
		return name
	}

	// Fallback to URL path
	u, _ := url.Parse(urlPath)
	parts := strings.Split(strings.TrimSuffix(u.Path, "/"), "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return "unknown_book"
}

func extractBooksFromSeries(seriesURL string) ([]BookInfo, error) {
	debug("Parsing series: %s", seriesURL)
	htmlBytes, err := fetchURL(seriesURL)
	if err != nil {
		return nil, err
	}
	html := string(htmlBytes)

	var books []BookInfo
	matches := bookItemRegex.FindAllStringSubmatch(html, -1)

	for _, match := range matches {
		content := match[1]
		u := bookURLRegex.FindStringSubmatch(content)
		idx := bookIndexRegex.FindStringSubmatch(content)
		t := bookTitleRegex.FindStringSubmatch(content)

		if len(u) > 1 && len(idx) > 1 && len(t) > 1 {
			b := BookInfo{
				URL:         "https://knigavuhe.org/" + u[1],
				DisplayName: strings.TrimSpace(idx[1]) + "_" + strings.TrimSpace(t[1]),
				SeriesIndex: strings.TrimSpace(idx[1]),
			}
			books = append(books, b)
		}
	}

	// Sort by index (handling 1.1, 1.2, 2, 10 correctly)
	sort.Slice(books, func(i, j int) bool {
		return compareIndices(books[i].SeriesIndex, books[j].SeriesIndex)
	})

	debug("Found %d books in series", len(books))
	return books, nil
}

func compareIndices(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, _ := strconv.Atoi(pa[i])
		nb, _ := strconv.Atoi(pb[i])
		if na != nb {
			return na < nb
		}
	}
	return len(pa) < len(pb)
}

func sanitizePath(p string) string {
	safe := strings.Trim(forbiddenChars.ReplaceAllString(p, "_"), " ._")
	if safe == "" {
		return "audiobook"
	}
	return safe
}

func saveDescription(dir string, d *BookData) {
	getName := func(v interface{}) string {
		var names []string
		if list, ok := v.([]interface{}); ok {
			for _, i := range list {
				if m, ok := i.(map[string]interface{}); ok {
					names = append(names, fmt.Sprint(m["name"]))
				}
			}
		} else if m, ok := v.(map[string]interface{}); ok {
			for _, i := range m {
				if p, ok := i.(map[string]interface{}); ok {
					names = append(names, fmt.Sprint(p["name"]))
				}
			}
		}
		return strings.Join(names, ", ")
	}

	desc := fmt.Sprintf("Название: %s\nАвтор: %s\nЧтец: %s\n\n%s\nURL: %s",
		d.Book.Name, getName(d.Book.Authors), getName(d.Book.Readers), d.Description, d.Book.URL)
	os.WriteFile(filepath.Join(dir, "Description.txt"), []byte(desc), 0644)
}

func cleanState(state *DownloadState, path string) {
	state.mu.Lock()
	defer state.mu.Unlock()

	allDone := true
	for _, s := range state.Books {
		if s != StatusCompleted {
			allDone = false
			break
		}
	}
	if allDone {
		os.Remove(path)
	}
}