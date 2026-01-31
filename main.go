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
	"time"

	"github.com/minio/selfupdate"
)

var (
	version   = "0.0.0"
	gitCommit = ""
	buildDate = ""
	goVersion = ""
	updateURL = ""
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
}

type BookInfo struct {
	URL         string
	DisplayName string
	SeriesIndex string
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
	maxRetries   = 20
	retryDelay   = 2 * time.Second
	maxIdleConns = 10
)

var (
	jsonRegex      = regexp.MustCompile(`BookController\.enter\((.*?)\);`)
	descRegex      = regexp.MustCompile(`bookDescription\">(.+?)</div>`)
	forbiddenChars = regexp.MustCompile(`[<>:"/\\|?*]`)

	bookItemRegex  = regexp.MustCompile(`(?s)<div class="bookitem">(.*?)</div>\s*</div>`)
	bookURLRegex   = regexp.MustCompile(`(?s)class="bookitem_cover"\s+href="/(book/[^"]+)"`)
	bookIndexRegex = regexp.MustCompile(`(?s)<span class="bookitem_serie_index">\s*([\d\.]+)\.\s*</span>`)
	bookTitleRegex = regexp.MustCompile(`(?s)<span class="bookitem_serie_index">\s*[\d\.]+\.\s*</span>\s*([^<]+)`)

	customDialer = &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	httpClient = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:    maxIdleConns,
			IdleConnTimeout: 60 * time.Second,

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
		} else {

			updated, err := checkAndApplyUpdate()
			if err != nil {
				if verboseMode {
					fmt.Printf("⚠️ Update check failed: %v\n", err)
				}
			} else if updated {
				fmt.Println("✅ Application updated. Exiting to restart.")
				os.Exit(0)
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
	partsA := strings.Split(a, ".")
	partsB := strings.Split(b, ".")

	for i := 0; i < len(partsA) && i < len(partsB); i++ {
		numA, _ := strconv.Atoi(partsA[i])
		numB, _ := strconv.Atoi(partsB[i])

		if numA != numB {
			return numA < numB
		}
	}

	return len(partsA) < len(partsB)
}

func processDownloads(outputDir string, books []BookInfo, state *DownloadState) []DownloadResult {
	var wg sync.WaitGroup
	bookChan := make(chan BookInfo, len(books))
	resultChan := make(chan DownloadResult, len(books))

	// Control book concurrency with flag (Default: 1 - sequential)
	workers := bookConcurrency
	if workers < 1 {
		workers = 1
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for book := range bookChan {
				// Skip if already completed from a previous run
				if state.GetStatus(book.DisplayName) == StatusCompleted {
					fmt.Printf("⏭️  Skipping %s (Already completed)\n", book.DisplayName)
					resultChan <- DownloadResult{URL: book.URL, BookName: book.DisplayName, Path: filepath.Join(outputDir, sanitizePath(book.DisplayName))}
					continue
				}

				state.UpdateStatus(book.DisplayName, StatusDownloading)
				result := downloadBook(book.URL, outputDir, book.DisplayName)

				if len(result.Errors) == 0 {
					state.UpdateStatus(book.DisplayName, StatusCompleted)
				} else {
					state.UpdateStatus(book.DisplayName, StatusFailed)
				}
				resultChan <- result
			}
		}()
	}

	for i, book := range books {
		fmt.Printf("📝 Queuing %d/%d: %s\n", i+1, len(books), book.DisplayName)
		// Mark as pending if not already tracked
		if state.GetStatus(book.DisplayName) == "" {
			state.UpdateStatus(book.DisplayName, StatusPending)
		}
		bookChan <- book
	}
	close(bookChan)

	go func() {
		wg.Wait()
		close(resultChan)
	}()

	results := make([]DownloadResult, 0, len(books))
	for res := range resultChan {
		results = append(results, res)
	}

	sort.Slice(results, func(i, j int) bool {
		idxI := extractIndex(results[i].BookName)
		idxJ := extractIndex(results[j].BookName)
		return compareIndices(idxI, idxJ)
	})

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

	// ENSURE FOLDER EXISTS
	bookDir := filepath.Join(outputDir, sanitizePath(displayName))
	if err := os.MkdirAll(bookDir, 0755); err != nil {
		result.addError("create directory", err)
		return result
	}

	result.BookName = displayName
	result.Path = bookDir

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

	reserved := []string{"CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9"}

	for _, r := range reserved {
		if strings.EqualFold(safe, r) {
			return safe + "_"
		}
	}

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

	// Use trackConcurrency flag (Default: 5). If set to 1, loads strictly track by track.
	workers := trackConcurrency
	if workers < 1 {
		workers = 1
	}
	sem := make(chan struct{}, workers)

	for i, track := range tracks {
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

			filePath := filepath.Join(bookDir, sanitizePath(t.Title)+ext)

			// If file already exists, it is done.
			if _, err := os.Stat(filePath); err == nil {
				return
			}

			// Clean up orphan .part files from previous runs
			partFile := filePath + ".part"
			os.Remove(partFile)

			var lastErr error
			for attempt := 1; attempt <= maxRetries; attempt++ {

				// Use robust download (saves to .part first)
				if err := downloadFileVerbose(t.URL, filePath); err == nil {
					return
				} else {
					lastErr = err
					if idx == 0 {
						debugLog("[%s] Track 1 Retry %d/%d failed: %v", sourceType, attempt, maxRetries, err)
					}
					time.Sleep(retryDelay)
				}
			}

			select {
			case errChan <- fmt.Errorf("track '%s' failed: %w", t.Title, lastErr):
				cancel()
			default:
			}
		}(i, track)
	}

	go func() {
		wg.Wait()
		close(errChan)
	}()

	if err := <-errChan; err != nil {
		return err
	}
	return nil
}

// downloadFileVerbose downloads safely to a .part file to prevent corruptions if a VM thread is killed.
func downloadFileVerbose(url, finalFilePath string) error {
	partFilePath := finalFilePath + ".part"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("req build error: %w", err)
	}

	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", "https://knigavuhe.org/")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d (%s)", resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	if resp.ContentLength == 0 {
		return fmt.Errorf("HTTP body is empty (Content-Length: 0)")
	}

	// Write to .part file
	file, err := os.Create(partFilePath)
	if err != nil {
		return fmt.Errorf("file create error: %w", err)
	}

	n, err := io.Copy(file, resp.Body)
	file.Close() // Close immediately after copy so we can rename

	if err != nil {
		os.Remove(partFilePath)
		return fmt.Errorf("io.Copy error: %w", err)
	}

	if n == 0 {
		os.Remove(partFilePath)
		return fmt.Errorf("wrote 0 bytes")
	}

	// Success! Rename .part to .mp3
	if err := os.Rename(partFilePath, finalFilePath); err != nil {
		return fmt.Errorf("failed to rename part file: %w", err)
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
