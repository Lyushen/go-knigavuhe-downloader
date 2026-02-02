package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

var (
	version, gitCommit, buildDate, goVersion, updateURL string
)

// Global flags
var (
	verboseMode      bool
	bookConcurrency  int
	trackConcurrency int
)

type Person struct {
	Name string `json:"name"`
}

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
	SeriesName     string  `json:"-"`
	SeriesIndex    string  `json:"-"`
}

type BookInfo struct {
	URL, DisplayName, SeriesIndex string
}

// State tracking structures
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

func (s *DownloadState) UpdateStatus(bookName string, status BookStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Books[bookName] = status
	s.save()
}

func (s *DownloadState) GetStatus(bookName string) BookStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Books[bookName]
}

func (s *DownloadState) save() {
	data, err := json.MarshalIndent(s, "", "  ")
	if err == nil {
		os.WriteFile(s.path, data, 0644)
	}
}

func (s *DownloadState) DeleteIfComplete() {
	s.mu.Lock()
	defer s.mu.Unlock()
	allComplete := true
	for _, status := range s.Books {
		if status != StatusCompleted {
			allComplete = false
			break
		}
	}
	if allComplete {
		os.Remove(s.path)
		fmt.Println("✅ All downloads completed successfully. State file cleaned up.")
	}
}

const (
	userAgent    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36 Edg/135.0.0.0"
	cookie       = "new_design=1"
	maxRetries   = 5
	retryDelay   = 2 * time.Second
	maxIdleConns = 1
)

var (
	jsonRegex      = regexp.MustCompile(`BookController\.enter\((.*?)\);`)
	descRegex      = regexp.MustCompile(`bookDescription\">(.+?)</div>`)
	forbiddenChars = regexp.MustCompile(`[<>:"/\\|?*]`)

	bookItemRegex    = regexp.MustCompile(`(?s)<div class="bookitem">(.*?)</div>\s*</div>`)
	bookURLRegex     = regexp.MustCompile(`(?s)class="bookitem_cover"\s+href="/(book/[^"]+)"`)
	bookIndexRegex   = regexp.MustCompile(`(?s)<span class="bookitem_serie_index">\s*([\d\.]+)\.\s*</span>`)
	bookTitleRegex   = regexp.MustCompile(`(?s)<span class="bookitem_serie_index">\s*[\d\.]+\.\s*</span>\s*([^<]+)`)
	seriesBlockRegex = regexp.MustCompile(`(?s)class="book_info_line icon_serie"(.*?)</div>\s*</div>`)
	seriesNameRegex  = regexp.MustCompile(`(?s)href="/serie/[^"]+">([^<]+)</a>`)
	seriesIndexRegex = regexp.MustCompile(`(?s)class="book_info_line_serie_index">\s*\(([\d\.]+)\)`)

	customDialer = &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	httpClient = &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			MaxIdleConns:          maxIdleConns,
			IdleConnTimeout:       60 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,

			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}

				if net.ParseIP(host) != nil {
					return customDialer.DialContext(ctx, network, addr)
				}

				ips, err := lookupIPWithFallback(ctx, host)
				if err != nil {
					return nil, err
				}

				firstIP := ips[0]
				if strings.Contains(firstIP, ":") {
					firstIP = "[" + firstIP + "]"
				}

				return customDialer.DialContext(ctx, network, firstIP+":"+port)
			},
		},
	}
)

func debugLog(format string, v ...interface{}) {
	if verboseMode {
		log.Printf("[DEBUG] "+format, v...)
	}
}

func lookupIPWithFallback(ctx context.Context, host string) ([]string, error) {

	dnsProviders := []struct {
		name     string
		resolver *net.Resolver
	}{
		{
			name:     "System",
			resolver: net.DefaultResolver,
		},
		{
			name: "Google (8.8.8.8)",
			resolver: &net.Resolver{
				PreferGo: true,
				Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
					d := net.Dialer{Timeout: 2 * time.Second}
					return d.DialContext(ctx, "udp", "8.8.8.8:53")
				},
			},
		},
		{
			name: "Cloudflare (1.1.1.1)",
			resolver: &net.Resolver{
				PreferGo: true,
				Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
					d := net.Dialer{Timeout: 2 * time.Second}
					return d.DialContext(ctx, "udp", "1.1.1.1:53")
				},
			},
		},
	}

	var lastErr error

	for _, provider := range dnsProviders {

		ips, err := provider.resolver.LookupHost(ctx, host)
		if err == nil && len(ips) > 0 {
			if verboseMode && provider.name != "System" {

				fmt.Printf("DEBUG: Resolved %s using %s\n", host, provider.name)
			}
			return ips, nil
		}
		lastErr = err
	}

	return nil, fmt.Errorf("all DNS resolvers failed: %v", lastErr)
}

// Windows Fix: Clean up .old files from previous updates
func cleanupOldBinary() {
	if runtime.GOOS == "windows" {
		exe, err := os.Executable()
		if err == nil {
			oldExe := exe + ".old"
			if _, err := os.Stat(oldExe); err == nil {
				// Attempt to remove the old file. If it fails, we can't do much,
				// but usually, the process is dead by now.
				_ = os.Remove(oldExe)
			}
		}
	}
}

func main() {
	// Attempt to clean up artifacts from previous updates immediately
	cleanupOldBinary()

	var showVersion bool
	var waitUpdate bool

	flag.BoolVar(&verboseMode, "verbose", false, "Show detailed technical logs")
	flag.BoolVar(&showVersion, "version", false, "Print version information and exit")
	flag.BoolVar(&waitUpdate, "wait-update", false, "Wait for a new version to be available, update, and then exit")
	flag.IntVar(&bookConcurrency, "book-workers", 1, "Number of books to download in parallel (default: 1 for sequential)")
	flag.IntVar(&trackConcurrency, "track-workers", 5, "Number of tracks to download in parallel per book (default: 5)")
	flag.Parse()

	if showVersion {
		fmt.Printf("Go Knigavuhe Downloader\n")
		fmt.Printf("Version:    %s\n", version)
		fmt.Printf("Git Commit: %s\n", gitCommit)
		fmt.Printf("Build Date: %s\n", buildDate)
		fmt.Printf("Go Version: %s\n", goVersion)
		if updateURL != "" {
			fmt.Printf("Update URL: %s\n", updateURL)
		}
		os.Exit(0)
	}

	if updateURL != "" {
		if waitUpdate {
			fmt.Printf("⏳ Waiting for update from %s...\n", updateURL)
			for {
				updated, err := checkAndApplyUpdate()
				fmt.Printf(".")
				if err != nil {
					// Print detailed error so user knows why it failed
					fmt.Printf("\n❌ Update Failed: %v\n", err)
				} else if updated {
					fmt.Println("\n✅ Update applied successfully. Exiting to restart.")
					os.Exit(0)
				}
				time.Sleep(5 * time.Second)
			}
		}
	} else if waitUpdate {
		log.Fatal("❌ Cannot wait for update: No update URL injected at build time.")
	}

	args := flag.Args()
	if len(args) < 2 {
		fmt.Println("Usage:")
		fmt.Println("  Flags:")
		fmt.Println("    -book-workers  : Book concurrency (default: 1)")
		fmt.Println("    -track-workers : Track concurrency per book (default: 5)")
		fmt.Println("    -wait-update   : Loop and wait for update before exiting")
		fmt.Println("    -version       : Show version info")
		fmt.Println("    -verbose       : Show detailed output")
		fmt.Println("  For series: go-knigavuhe [flags] <output-dir> <series-url>")
		fmt.Println("  For file:   go-knigavuhe [flags] <output-dir> <url-file.txt>")
		os.Exit(1)
	}

	outputDir := args[0]
	inputArg := args[1]

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		log.Fatalf("Error creating output directory: %v", err)
	}

	// Initialize State
	statePath := filepath.Join(outputDir, "state.json")
	state := &DownloadState{
		Books: make(map[string]BookStatus),
		path:  statePath,
	}

	// Load existing state if available
	if stateData, err := os.ReadFile(statePath); err == nil {
		json.Unmarshal(stateData, state)
		fmt.Println("🔄 Resuming from previous state...")
	}

	var results []DownloadResult

	if strings.HasSuffix(strings.ToLower(inputArg), ".txt") || fileExists(inputArg) {
		fmt.Printf("📄 Reading URLs from file: %s\n", inputArg)
		lines, err := readLinesFromFile(inputArg)
		if err != nil {
			log.Fatalf("Error reading file: %v", err)
		}

		var allBooks []BookInfo
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}

			if strings.Contains(line, "/serie/") {
				fmt.Printf("🔍 Extracting books from series: %s\n", line)
				books, err := extractBooksFromSeries(line)
				if err != nil {
					fmt.Printf("⚠️ Error extracting series %s: %v\n", line, err)
					continue
				}
				allBooks = append(allBooks, books...)
				fmt.Printf("✅ Found %d books\n", len(books))
			} else {

				allBooks = append(allBooks, BookInfo{
					URL:         line,
					DisplayName: extractBookNameFromURL(line),
					SeriesIndex: fmt.Sprintf("%d", len(allBooks)+1),
				})
			}
		}

		if len(allBooks) == 0 {
			log.Fatal("❌ No valid URLs or series found in file")
		}

		fmt.Printf("🚀 Processing %d total books\n", len(allBooks))
		results = processDownloads(outputDir, allBooks, state)
	} else {

		fmt.Printf("🔍 Extracting books from series: %s\n", inputArg)
		books, err := extractBooksFromSeries(inputArg)
		if err != nil {
			log.Fatalf("Error extracting books from series: %v", err)
		}

		if len(books) == 0 {
			log.Fatal("❌ No books found in series. Run with DEBUG=1 for more info.")
		}

		fmt.Printf("✅ Found %d books\n", len(books))
		results = processDownloads(outputDir, books, state)
	}

	fmt.Println("\n" + strings.Repeat("=", 60))
	fmt.Println("📊 DOWNLOAD SUMMARY")
	fmt.Println(strings.Repeat("=", 60))
	for _, res := range results {
		if len(res.Errors) == 0 {
			fmt.Printf("✅ %s\n   📁 %s\n", res.BookName, res.Path)
		} else {
			fmt.Printf("❌ %s\n   🚨 Errors:\n", res.BookName)
			for _, e := range res.Errors {
				fmt.Printf("      • %s\n", e)
			}
		}
	}
	fmt.Println(strings.Repeat("=", 60))
	state.DeleteIfComplete()
}
func checkAndApplyUpdate() (bool, error) {
	versionURL := strings.TrimRight(updateURL, "/") + "/version.txt"
	req, err := http.NewRequest("GET", versionURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("version check HTTP %d", resp.StatusCode)
	}

	remoteVersionBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	remoteVersion := string(bytes.TrimSpace(remoteVersionBytes))

	if remoteVersion == "" || remoteVersion == version {
		return false, nil
	}

	fmt.Printf("⬆️  New version found: %s (Current: %s). Updating...\n", remoteVersion, version)
	binName := fmt.Sprintf("go-knigavuhe-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binURL := strings.TrimRight(updateURL, "/") + "/" + binName
	binReq, err := http.NewRequest("GET", binURL, nil)
	if err != nil {
		return false, err
	}
	binReq.Header.Set("User-Agent", userAgent)
	binResp, err := httpClient.Do(binReq)
	if err != nil {
		return false, err
	}
	defer binResp.Body.Close()

	if binResp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("binary download HTTP %d from %s", binResp.StatusCode, binURL)
	}
	err = selfupdate.Apply(binResp.Body, selfupdate.Options{})
	if err != nil {
		return false, fmt.Errorf("failed to apply update: %w", err)
	}

	return true, nil
}
func fileExists(filename string) bool {
	_, err := os.Stat(filename)
	return err == nil
}

func readLinesFromFile(filename string) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, scanner.Err()
}

func extractBookNameFromURL(bookURL string) string {
	parts := strings.Split(bookURL, "/")
	if len(parts) > 0 {
		lastPart := parts[len(parts)-1]
		lastPart = strings.TrimSuffix(lastPart, "/")

		if idx := strings.Index(lastPart, "-"); idx > 0 {
			lastPart = lastPart[idx+1:]
		}
		return strings.ReplaceAll(lastPart, "-", " ")
	}
	return "Unknown Book"
}

func extractBooksFromSeries(seriesURL string) ([]BookInfo, error) {
	html, err := downloadPage(seriesURL)
	if err != nil {
		return nil, fmt.Errorf("failed to download series page: %w", err)
	}

	if os.Getenv("DEBUG") == "1" {
		debugFile := "debug_series.html"
		os.WriteFile(debugFile, []byte(html), 0644)
		fmt.Printf("Debug: Saved HTML to %s\n", debugFile)
	}

	var books []BookInfo
	matches := bookItemRegex.FindAllStringSubmatch(html, -1)

	if os.Getenv("DEBUG") == "1" {
		fmt.Printf("Debug: Found %d raw book items\n", len(matches))
	}

	for _, match := range matches {
		content := match[1]

		urlMatch := bookURLRegex.FindStringSubmatch(content)
		if len(urlMatch) < 2 {
			continue
		}

		indexMatch := bookIndexRegex.FindStringSubmatch(content)
		if len(indexMatch) < 2 {
			continue
		}
		index := strings.TrimSpace(indexMatch[1])

		titleMatch := bookTitleRegex.FindStringSubmatch(content)
		if len(titleMatch) < 2 {
			continue
		}
		title := strings.TrimSpace(titleMatch[1])

		displayName := fmt.Sprintf("%s_%s", index, title)
		books = append(books, BookInfo{
			URL:         "https://knigavuhe.org/" + urlMatch[1],
			DisplayName: displayName,
			SeriesIndex: index,
		})
	}

	sort.Slice(books, func(i, j int) bool {
		return compareIndices(books[i].SeriesIndex, books[j].SeriesIndex)
	})

	return books, nil
}

func compareIndices(a, b string) bool {
	// Remove parens if present "(1)" -> "1"
	a = strings.Trim(a, "()")
	b = strings.Trim(b, "()")

	partsA := strings.Split(a, ".")
	partsB := strings.Split(b, ".")

	for i := 0; i < len(partsA) && i < len(partsB); i++ {
		numA, errA := strconv.Atoi(partsA[i])
		numB, errB := strconv.Atoi(partsB[i])

		// If both are numbers, compare numerically
		if errA == nil && errB == nil {
			if numA != numB {
				return numA < numB
			}
		} else {
			// Fallback to string comparison
			if partsA[i] != partsB[i] {
				return partsA[i] < partsB[i]
			}
		}
	}

	return len(partsA) < len(partsB)
}

func processDownloads(outputDir string, books []BookInfo, state *DownloadState) []DownloadResult {
	// 1. Sort books by Series Index (Numeric sort: 1, 2, 10 instead of 1, 10, 2)
	sort.Slice(books, func(i, j int) bool {
		return compareIndices(books[i].SeriesIndex, books[j].SeriesIndex)
	})

	fmt.Println("📚 Sorted processing queue:")
	for _, b := range books {
		status := state.GetStatus(b.DisplayName)
		if status == "" {
			status = "Ready"
		}
		fmt.Printf("   [%s] %s (Index: %s)\n", status, b.DisplayName, b.SeriesIndex)
	}
	fmt.Println(strings.Repeat("-", 40))

	var results []DownloadResult

	// 2. Strict Sequential Processing
	for i, book := range books {
		// Calculate overall progress
		fmt.Printf("\n🚀 Processing Book %d/%d: %s\n", i+1, len(books), book.DisplayName)

		// Check Status
		currentStatus := state.GetStatus(book.DisplayName)

		// If completed, skip
		if currentStatus == StatusCompleted {
			fmt.Printf("⏭️  Skipping (Already completed)\n")
			safeName := sanitizePath(book.DisplayName)
			// We attempt to guess the path to return a valid result even if skipped
			results = append(results, DownloadResult{
				URL:      book.URL,
				BookName: book.DisplayName,
				Path:     filepath.Join(outputDir, safeName),
			})
			continue
		}

		// Update State to Downloading
		state.UpdateStatus(book.DisplayName, StatusDownloading)

		// Download
		result := downloadBook(book.URL, outputDir, book.DisplayName)

		if len(result.Errors) == 0 {
			state.UpdateStatus(book.DisplayName, StatusCompleted)
			fmt.Printf("✅ Book Completed: %s\n", result.BookName)
		} else {
			state.UpdateStatus(book.DisplayName, StatusFailed)
			fmt.Printf("❌ Book Failed: %s\n", result.BookName)
		}

		results = append(results, result)

		// Optional: Small pause between books to be gentle on the server
		time.Sleep(1 * time.Second)
	}

	return results
}
func extractIndex(bookName string) string {
	parts := strings.SplitN(bookName, ".", 2)
	if len(parts) > 0 {
		return parts[0]
	}
	return "999"
}

func downloadBook(bookURL, outputDir, displayName string) DownloadResult {
	result := DownloadResult{URL: bookURL}

	normalizedURL, err := normalizeURL(bookURL)
	if err != nil {
		result.addError("URL normalization", err)
		return result
	}

	bookData, err := fetchBookData(normalizedURL)
	if err != nil {
		result.addError("fetch book data", err)
		return result
	}

	// NEW: Determine folder name logic
	// If series data exists, format as "Index_SeriesName". Otherwise use Book Name.
	folderName := displayName
	if bookData.SeriesName != "" && bookData.SeriesIndex != "" {
		// Example: 1_Legendary_Moon_Sculptor
		folderName = fmt.Sprintf("%s_%s", bookData.SeriesIndex, bookData.SeriesName)
	} else if bookData.SeriesName != "" {
		folderName = bookData.SeriesName
	} else {
		folderName = bookData.Book.Name
	}

	// Apply snake_case sanitization
	safeFolderName := sanitizePath(folderName)

	// Update displayName to reflect the actual folder being used for logging
	displayName = safeFolderName

	// ENSURE FOLDER EXISTS
	bookDir := filepath.Join(outputDir, safeFolderName)
	if err := os.MkdirAll(bookDir, 0755); err != nil {
		result.addError("create directory", err)
		return result
	}

	result.BookName = displayName
	result.Path = bookDir

	fmt.Printf("📂 Target Folder: %s\n", safeFolderName)

	downloadAssets(bookDir, bookData, &result)
	return result
}

func fetchBookData(bookURL string) (*BookData, error) {
	html, err := downloadPage(bookURL)
	if err != nil {
		return nil, fmt.Errorf("page download failed: %w", err)
	}

	jsonData, err := extractJSON(html)
	if err != nil {
		return nil, fmt.Errorf("JSON extraction failed: %w", err)
	}

	bookData, err := parseBookJSON(jsonData)
	if err != nil {
		return nil, fmt.Errorf("JSON parsing failed: %w", err)
	}

	bookData.CoverAlt = strings.Split(bookData.Book.Cover, "-")[0] + ".jpg"

	if desc, err := extractDescription(html); err == nil {
		bookData.Description = desc
	}

	// NEW: Extract Series Information from HTML
	if seriesMatch := seriesBlockRegex.FindStringSubmatch(html); len(seriesMatch) > 1 {
		blockContent := seriesMatch[1]

		// Extract Name
		if nameMatch := seriesNameRegex.FindStringSubmatch(blockContent); len(nameMatch) > 1 {
			bookData.SeriesName = strings.TrimSpace(nameMatch[1])
		}

		// Extract Index
		if indexMatch := seriesIndexRegex.FindStringSubmatch(blockContent); len(indexMatch) > 1 {
			bookData.SeriesIndex = strings.TrimSpace(indexMatch[1])
		}
	}

	return bookData, nil
}

func downloadPage(url string) (string, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Cookie", cookie)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(body), nil
}

func extractJSON(html string) (string, error) {
	matches := jsonRegex.FindStringSubmatch(html)
	if len(matches) < 2 {
		return "", errors.New("JSON data not found in HTML")
	}
	return matches[1], nil
}

func parseBookJSON(jsonStr string) (*BookData, error) {
	var data BookData
	if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
		return nil, err
	}
	return &data, nil
}

func extractDescription(html string) (string, error) {
	matches := descRegex.FindStringSubmatch(html)
	if len(matches) < 2 {
		return "", errors.New("description not found")
	}

	text := regexp.MustCompile(`<[^>]*>`).ReplaceAllString(matches[1], "")
	return text, nil
}

func sanitizePath(path string) string {
	safe := forbiddenChars.ReplaceAllString(path, "_")

	whitespace := regexp.MustCompile(`\s+`)
	safe = whitespace.ReplaceAllString(safe, "_")

	multiUnderscore := regexp.MustCompile(`_+`)
	safe = multiUnderscore.ReplaceAllString(safe, "_")

	safe = strings.Trim(safe, " ._")

	if safe == "" {
		safe = "audiobook_file"
	}

	return safe
}

func downloadAssets(bookDir string, bookData *BookData, result *DownloadResult) {
	if err := downloadWithFallback(
		[]string{bookData.CoverAlt, bookData.Book.Cover},
		filepath.Join(bookDir, "cover.jpg"),
	); err != nil {
		result.addError("cover download", err)
	}

	if err := saveDescription(bookDir, bookData); err != nil {
		result.addError("save description", err)
	}

	if err := downloadPlaylist(bookDir, bookData, result); err != nil {
		result.addError("playlist download", err)
	}
}

func downloadWithFallback(urls []string, filePath string) error {
	for _, url := range urls {
		if url == "" {
			continue
		}
		if err := downloadFile(url, filePath); err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("all download attempts failed")
}

func saveDescription(bookDir string, bookData *BookData) error {
	getNames := func(data interface{}) string {
		var names []string
		switch v := data.(type) {
		case map[string]interface{}:
			for _, item := range v {
				if person, ok := item.(map[string]interface{}); ok {
					if name, ok := person["name"].(string); ok {
						names = append(names, name)
					}
				}
			}
		case []interface{}:
			for _, item := range v {
				if person, ok := item.(map[string]interface{}); ok {
					if name, ok := person["name"].(string); ok {
						names = append(names, name)
					}
				}
			}
		}
		return strings.Join(names, ", ")
	}

	desc := fmt.Sprintf(
		"Название: %s\nАвтор(ы): %s\nЧтец(ы): %s\n\nОписание:\n%s\n\nURL: %s",
		bookData.Book.Name,
		getNames(bookData.Book.Authors),
		getNames(bookData.Book.Readers),
		bookData.Description,
		bookData.Book.URL,
	)

	return os.WriteFile(filepath.Join(bookDir, "Description.txt"), []byte(desc), 0644)
}

func downloadPlaylist(bookDir string, bookData *BookData, result *DownloadResult) error {

	debugLog("Playlist Check for: %s", bookData.Book.Name)
	debugLog(" > Standard Tracks: %d", len(bookData.Playlist))
	debugLog(" > Merged Tracks:   %d", len(bookData.MergedPlaylist))

	err := downloadTracks(bookDir, bookData.Playlist, "STANDARD")
	if err == nil {
		return nil
	}

	debugLog("🔴 Standard Playlist Failed: %v", err)
	fmt.Printf("   ⚠️  Standard download failed, trying merged file...\n")

	if err := downloadTracks(bookDir, bookData.MergedPlaylist, "MERGED"); err != nil {
		result.addError("playlist download", fmt.Errorf("ALL methods failed. Merged error: %v", err))
		return err
	}

	return nil
}

func downloadTracks(bookDir string, tracks []Audio, sourceType string) error {
	if len(tracks) == 0 {
		return fmt.Errorf("track list is empty")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	errChan := make(chan error, 1)

	// Use trackConcurrency flag (Default: 5).
	workers := trackConcurrency
	if workers < 1 {
		workers = 1
	}
	sem := make(chan struct{}, workers)

	totalTracks := len(tracks)
	var completedTracks int32

	// Newline before starting to prevent overwriting previous logs
	if !verboseMode {
		fmt.Println()
	}

	for i, track := range tracks {
		// Check context before queuing
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		wg.Add(1)
		go func(idx int, t Audio) {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			cleanURL := strings.Split(t.URL, "?")[0]
			ext := filepath.Ext(cleanURL)
			if ext == "" {
				ext = ".mp3"
			}
			ext = strings.ToLower(ext)

			// Sanitize filename to snake_case
			safeTitle := sanitizePath(t.Title)
			filePath := filepath.Join(bookDir, safeTitle+ext)

			// Progress Update (Visual)
			if !verboseMode {
				// Calculate percentage
				current := atomic.AddInt32(&completedTracks, 0) // Read current
				percent := float64(current) / float64(totalTracks) * 100

				// Truncate title for display
				displayTitle := safeTitle
				if len(displayTitle) > 20 {
					displayTitle = displayTitle[:17] + "..."
				}

				// \r moves cursor to start of line to overwrite
				fmt.Printf("\r⬇️  [%s] %3.0f%% (%d/%d): %-25s", sourceType, percent, current, totalTracks, displayTitle)
			} else {
				fmt.Printf("⬇️  Starting: %s\n", safeTitle)
			}

			// If file already exists and has size, skip
			if info, err := os.Stat(filePath); err == nil && info.Size() > 0 {
				atomic.AddInt32(&completedTracks, 1)
				return
			}

			// Clean up orphan .part files
			partFile := filePath + ".part"
			os.Remove(partFile)

			var lastErr error
			success := false

			for attempt := 1; attempt <= maxRetries; attempt++ {
				// Check context again before retry
				if ctx.Err() != nil {
					return
				}

				if err := downloadFileVerbose(t.URL, filePath); err == nil {
					success = true
					break
				} else {
					lastErr = err
					if verboseMode {
						log.Printf("⚠️  Retry %d/%d for '%s': %v", attempt, maxRetries, safeTitle, err)
					}
					// Backoff
					time.Sleep(time.Duration(attempt) * retryDelay)
				}
			}

			if !success {
				// Signal error to stop other workers if strictly required,
				// or just log it. Here we abort on failure.
				select {
				case errChan <- fmt.Errorf("track '%s' failed after %d attempts: %w", t.Title, maxRetries, lastErr):
					cancel() // Stop other downloads
				default:
				}
			} else {
				atomic.AddInt32(&completedTracks, 1)
			}
		}(i, track)
	}

	go func() {
		wg.Wait()
		close(errChan)
	}()

	if err := <-errChan; err != nil {
		if !verboseMode {
			fmt.Println() // Move to next line after progress bar
		}
		return err
	}

	if !verboseMode {
		fmt.Printf("\r✅ [%s] 100%% (%d/%d) Complete.                     \n", sourceType, totalTracks, totalTracks)
	}
	return nil
}

func downloadFileVerbose(downloadUrl, finalFilePath string) error {
	partFilePath := finalFilePath + ".part"

	req, err := http.NewRequest("GET", downloadUrl, nil)
	if err != nil {
		return fmt.Errorf("request build error: %w", err)
	}

	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", "https://knigavuhe.org/")

	// FIX: Disable Keep-Alive.
	// This forces a new TCP connection for every file.
	// Helps prevent "Unexpected EOF" caused by the server closing idle connections.
	req.Close = true

	resp, err := httpClient.Do(req)
	if err != nil {
		if urlErr, ok := err.(*url.Error); ok {
			if urlErr.Timeout() {
				return fmt.Errorf("connection timed out: %w", err)
			}
			if opErr, ok := urlErr.Err.(*net.OpError); ok {
				return fmt.Errorf("net op error (%s): %w", opErr.Net, err)
			}
		}
		return fmt.Errorf("network failure: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned HTTP %d (%s)", resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	if resp.ContentLength == 0 {
		return fmt.Errorf("server sent empty body (Content-Length: 0)")
	}

	file, err := os.Create(partFilePath)
	if err != nil {
		return fmt.Errorf("filesystem error (create): %w", err)
	}

	n, err := io.Copy(file, resp.Body)
	file.Close()

	if err != nil {
		os.Remove(partFilePath)
		if err == io.ErrUnexpectedEOF {
			return fmt.Errorf("connection dropped during download (EOF): %w", err)
		}
		return fmt.Errorf("write error: %w", err)
	}

	if resp.ContentLength > 0 && n != resp.ContentLength {
		os.Remove(partFilePath)
		return fmt.Errorf("incomplete download: expected %d bytes, got %d", resp.ContentLength, n)
	}

	if n == 0 {
		os.Remove(partFilePath)
		return fmt.Errorf("downloaded 0 bytes")
	}

	if err := os.Rename(partFilePath, finalFilePath); err != nil {
		return fmt.Errorf("rename failed: %w", err)
	}

	return nil
}

func downloadFile(url, filePath string) error {
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

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP status %d", resp.StatusCode)
	}

	file, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = io.Copy(file, resp.Body)
	return err
}

func normalizeURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}

	if u.Host == "m.knigavuhe.org" {
		u.Host = "knigavuhe.org"
	}
	u.Scheme = "https"

	return u.String(), nil
}

type DownloadResult struct {
	URL      string
	BookName string
	Path     string
	Errors   []string
}

func (r *DownloadResult) addError(context string, err error) {
	r.Errors = append(r.Errors, fmt.Sprintf("%s: %v", context, err))
}
