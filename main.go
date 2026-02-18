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
	version, gitCommit, buildDate, goVersion, updateURL string
)
var (
	verboseMode      bool
	bookConcurrency  int
	trackConcurrency int
)

type Person struct {
	Name string `json:"name"`
}
type Book struct {
	Name    string `json:"name"`
	Authors any    `json:"authors"`
	Readers any    `json:"readers"`
	Cover   string `json:"cover"`
	URL     string `json:"url"`
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
	userAgent  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36 Edg/135.0.0.0"
	cookie     = "new_design=1"
	maxRetries = 5
	retryDelay = 2 * time.Second
)

var (
	jsonRegex        = regexp.MustCompile(`BookController\.enter\((.*?)\);`)
	descRegex        = regexp.MustCompile(`bookDescription\">(.+?)</div>`)
	forbiddenChars   = regexp.MustCompile(`[<>:"/\\|?*]`)
	bookItemRegex    = regexp.MustCompile(`(?s)<div class="bookitem">(.*?)</div>\s*</div>`)
	bookURLRegex     = regexp.MustCompile(`(?s)class="bookitem_cover"\s+href="/(book/[^"]+)"`)
	bookIndexRegex   = regexp.MustCompile(`(?s)<span class="bookitem_serie_index">\s*([\d\.]+)\.\s*</span>`)
	bookTitleRegex   = regexp.MustCompile(`(?s)<span class="bookitem_serie_index">\s*[\d\.]+\.\s*</span>\s*([^<]+)`)
	seriesBlockRegex = regexp.MustCompile(`(?s)class="book_info_line icon_serie"(.*?)</div>\s*</div>`)
	seriesNameRegex  = regexp.MustCompile(`(?s)href="/serie/[^"]+">([^<]+)</a>`)
	seriesIndexRegex = regexp.MustCompile(`(?s)class="book_info_line_serie_index">\s*\(([\d\.]+)\)`)
	rangeRegex       = regexp.MustCompile(`^\[(\d+)-(\d+)\]\s*(.+)$`)
	customDialer     = &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	httpClient = &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			MaxIdleConns:          -1,
			DisableKeepAlives:     true,
			IdleConnTimeout:       1 * time.Second,
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

func debugLog(format string, v ...any) {
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
func cleanupOldBinary() {
	if runtime.GOOS == "windows" {
		exe, err := os.Executable()
		if err == nil {
			oldExe := exe + ".old"
			if _, err := os.Stat(oldExe); err == nil {
				_ = os.Remove(oldExe)
			}
		}
	}
}
func main() {
	cleanupOldBinary()
	var showVersion bool
	var waitUpdate bool
	flag.BoolVar(&verboseMode, "verbose", false, "Show detailed technical logs")
	flag.BoolVar(&showVersion, "version", false, "Print version information and exit")
	flag.BoolVar(&waitUpdate, "update", false, "Wait for a new version to be available, update, and then exit")
	flag.IntVar(&bookConcurrency, "book-workers", 1, "Number of books to download in parallel (default: 1 for sequential)")
	flag.IntVar(&trackConcurrency, "track-workers", 3, "Number of tracks to download in parallel per book (default: 3)")
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
					fmt.Printf("\n❌ Update Failed: %v\n", err)
				} else if updated {
					fmt.Println("\n✅ Update applied successfully. Exiting to restart.")
					os.Exit(0)
				}
				time.Sleep(5 * time.Second)
			}
		}
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
		fmt.Println("  Series Range example in file: [1-5] https://knigavuhe.org/serie/...")
		os.Exit(1)
	}
	outputDir := args[0]
	inputArg := args[1]
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		log.Fatalf("Error creating output directory: %v", err)
	}
	statePath := filepath.Join(outputDir, "state.json")
	state := &DownloadState{
		Books: make(map[string]BookStatus),
		path:  statePath,
	}
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
			var minIdx, maxIdx int = -1, -1
			cleanURL := line
			if match := rangeRegex.FindStringSubmatch(line); len(match) == 4 {
				minIdx, _ = strconv.Atoi(match[1])
				maxIdx, _ = strconv.Atoi(match[2])
				cleanURL = strings.TrimSpace(match[3])
				fmt.Printf("🎯 Series filter applied: [%d to %d] for %s\n", minIdx, maxIdx, cleanURL)
			}
			if strings.Contains(cleanURL, "/serie/") {
				fmt.Printf("🔍 Extracting books from series: %s\n", cleanURL)
				books, err := extractBooksFromSeries(cleanURL, minIdx, maxIdx)
				if err != nil {
					fmt.Printf("⚠️ Error extracting series %s: %v\n", cleanURL, err)
					continue
				}
				allBooks = append(allBooks, books...)
				fmt.Printf("✅ Found %d books in series range\n", len(books))
			} else {
				displayName := extractBookNameFromURL(cleanURL)
				allBooks = append(allBooks, BookInfo{
					URL:         cleanURL,
					DisplayName: displayName,
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
		if strings.Contains(inputArg, "/serie/") {
			fmt.Printf("🔍 Extracting books from series: %s\n", inputArg)
			books, err := extractBooksFromSeries(inputArg, -1, -1)
			if err != nil {
				log.Fatalf("Error extracting books from series: %v", err)
			}
			if len(books) == 0 {
				log.Fatal("❌ No books found in series.")
			}
			fmt.Printf("✅ Found %d books\n", len(books))
			results = processDownloads(outputDir, books, state)
		} else {
			b := BookInfo{
				URL:         inputArg,
				DisplayName: extractBookNameFromURL(inputArg),
				SeriesIndex: "1",
			}
			results = processDownloads(outputDir, []BookInfo{b}, state)
		}
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
	if idx := strings.Index(bookURL, "?"); idx != -1 {
		bookURL = bookURL[:idx]
	}
	bookURL = strings.TrimRight(bookURL, "/")
	parts := strings.Split(bookURL, "/")
	if len(parts) > 0 {
		lastPart := parts[len(parts)-1]
		if _, err := strconv.Atoi(lastPart); err == nil && len(parts) > 1 {
			lastPart = parts[len(parts)-2]
		}
		if idx := strings.Index(lastPart, "-"); idx > 0 {
			if _, err := strconv.Atoi(lastPart[:idx]); err == nil {
				lastPart = lastPart[idx+1:]
			}
		}
		name := strings.ReplaceAll(lastPart, "-", " ")
		name = strings.TrimSpace(name)
		if name != "" {
			return name
		}
	}
	return fmt.Sprintf("Book_%d", time.Now().UnixNano())
}
func extractBooksFromSeries(seriesURL string, minIdx, maxIdx int) ([]BookInfo, error) {
	html, err := downloadPage(seriesURL)
	if err != nil {
		return nil, fmt.Errorf("failed to download series page: %w", err)
	}
	var books []BookInfo
	matches := bookItemRegex.FindAllStringSubmatch(html, -1)
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
		indexStr := strings.TrimSpace(indexMatch[1])
		indexStr = strings.TrimRight(indexStr, ".")
		if minIdx != -1 && maxIdx != -1 {
			val, err := strconv.ParseFloat(indexStr, 64)
			// If it's a valid number, apply the filter
			if err == nil {
				if val < float64(minIdx) || val > float64(maxIdx) {
					continue
				}
			} else {
				// If we can't parse the index (rare), but a filter is active,
				continue
			}
		}
		titleMatch := bookTitleRegex.FindStringSubmatch(content)
		title := ""
		if len(titleMatch) >= 2 {
			title = strings.TrimSpace(titleMatch[1])
		}
		if title == "" {
			title = extractBookNameFromURL(urlMatch[1])
		}
		displayName := fmt.Sprintf("%s. %s", indexStr, title)
		books = append(books, BookInfo{
			URL:         "https://knigavuhe.org/" + urlMatch[1],
			DisplayName: displayName,
			SeriesIndex: indexStr,
		})
	}
	sort.Slice(books, func(i, j int) bool {
		return compareIndices(books[i].SeriesIndex, books[j].SeriesIndex)
	})
	return books, nil
}
func compareIndices(i1, i2 string) bool {
	// Clean string just in case
	i1 = strings.TrimRight(i1, ".")
	i2 = strings.TrimRight(i2, ".")

	v1, err1 := strconv.ParseFloat(i1, 64)
	v2, err2 := strconv.ParseFloat(i2, 64)

	// If both are numbers, compare mathematically
	if err1 == nil && err2 == nil {
		return v1 < v2
	}

	// If one is not a number, put numbers first
	if err1 == nil {
		return true
	}
	if err2 == nil {
		return false
	}

	// Fallback to text comparison
	return i1 < i2
}
func processDownloads(outputDir string, books []BookInfo, state *DownloadState) []DownloadResult {
	sort.Slice(books, func(i, j int) bool {
		return compareIndices(books[i].SeriesIndex, books[j].SeriesIndex)
	})
	fmt.Println("📚 Processing queue:")
	for _, b := range books {
		status := state.GetStatus(b.DisplayName)
		if status == "" {
			status = "Ready"
		}
		fmt.Printf("   [%s] %s\n", status, b.DisplayName)
	}
	fmt.Println(strings.Repeat("-", 40))
	var results []DownloadResult
	for i, book := range books {
		fmt.Printf("\n🚀 Processing Book %d/%d: %s\n", i+1, len(books), book.DisplayName)
		currentStatus := state.GetStatus(book.DisplayName)
		if currentStatus == StatusCompleted {
			fmt.Printf("⏭️  Skipping (Already completed)\n")
			safeName := sanitizePath(book.DisplayName)
			results = append(results, DownloadResult{
				URL:      book.URL,
				BookName: book.DisplayName,
				Path:     filepath.Join(outputDir, safeName),
			})
			continue
		}
		state.UpdateStatus(book.DisplayName, StatusDownloading)
		result := downloadBook(book.URL, outputDir, book.DisplayName)
		if len(result.Errors) == 0 {
			state.UpdateStatus(book.DisplayName, StatusCompleted)
			fmt.Printf("✅ Book Completed: %s\n", result.BookName)
		} else {
			state.UpdateStatus(book.DisplayName, StatusFailed)
			fmt.Printf("❌ Book Failed: %s\n", result.BookName)
		}
		results = append(results, result)
		time.Sleep(1 * time.Second)
	}
	return results
}
func getAuthorsString(authorsData any) string {
	var names []string
	switch v := authorsData.(type) {
	case map[string]any:
		for _, item := range v {
			if person, ok := item.(map[string]any); ok {
				if name, ok := person["name"].(string); ok {
					names = append(names, name)
				}
			}
		}
	case []any:
		for _, item := range v {
			if person, ok := item.(map[string]any); ok {
				if name, ok := person["name"].(string); ok {
					names = append(names, name)
				}
			}
		}
	}
	if len(names) > 0 {
		return strings.Join(names, ", ")
	}
	return ""
}
func downloadBook(bookURL, outputDir, initialDisplayName string) DownloadResult {
	result := DownloadResult{URL: bookURL, BookName: initialDisplayName}
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
	var folderName string
	realBookName := strings.TrimSpace(bookData.Book.Name)
	if realBookName == "" {
		realBookName = initialDisplayName
	}
	result.BookName = realBookName
	if bookData.SeriesName != "" && bookData.SeriesIndex != "" {
		folderName = fmt.Sprintf("%s. %s", bookData.SeriesIndex, realBookName)
	} else {
		author := getAuthorsString(bookData.Book.Authors)
		if author != "" {
			folderName = fmt.Sprintf("%s - %s", author, realBookName)
		} else {
			folderName = realBookName
		}
	}
	safeFolderName := sanitizePath(folderName)
	bookDir := filepath.Join(outputDir, safeFolderName)
	if err := os.MkdirAll(bookDir, 0755); err != nil {
		result.addError("create directory", err)
		return result
	}
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
	if seriesMatch := seriesBlockRegex.FindStringSubmatch(html); len(seriesMatch) > 1 {
		blockContent := seriesMatch[1]
		if nameMatch := seriesNameRegex.FindStringSubmatch(blockContent); len(nameMatch) > 1 {
			bookData.SeriesName = strings.TrimSpace(nameMatch[1])
		}
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
	safe = whitespace.ReplaceAllString(safe, " ")
	safe = strings.Trim(safe, " ._")
	if safe == "" {
		safe = fmt.Sprintf("Book_Unknown_%d", time.Now().Unix())
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
	getNames := func(data any) string {
		return getAuthorsString(data)
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
	sortTracksNaturally(tracks)
	totalTracks := len(tracks)
	var completedTracks int = 0
	if !verboseMode {
		fmt.Println()
	}
	for _, track := range tracks {
		cleanURL := strings.Split(track.URL, "?")[0]
		ext := filepath.Ext(cleanURL)
		if ext == "" {
			ext = ".mp3"
		}
		ext = strings.ToLower(ext)
		safeTitle := sanitizePath(track.Title)
		filePath := filepath.Join(bookDir, safeTitle+ext)
		partFile := filePath + ".part"
		if !verboseMode {
			percent := float64(completedTracks) / float64(totalTracks) * 100
			displayTitle := safeTitle
			if len(displayTitle) > 25 {
				displayTitle = displayTitle[:22] + "..."
			}
			fmt.Printf("\r⬇️  [%s] %3.0f%% (%d/%d): %-30s", sourceType, percent, completedTracks+1, totalTracks, displayTitle)
		} else {
			fmt.Printf("⬇️  Starting: %s\n", safeTitle)
		}
		if info, err := os.Stat(filePath); err == nil && info.Size() > 0 {
			completedTracks++
			continue
		}
		success := false
		var lastErr error
		for attempt := 1; attempt <= maxRetries; attempt++ {
			err := downloadFileResumable(track.URL, filePath)
			if err == nil {
				success = true
				break
			}
			lastErr = err
			if verboseMode {
				log.Printf("⚠️  Retry %d/%d for '%s': %v", attempt, maxRetries, safeTitle, err)
			}
			time.Sleep(time.Duration(attempt) * retryDelay)
		}
		if !success {
			os.Remove(partFile)
			if !verboseMode {
				fmt.Println()
			}
			return fmt.Errorf("track '%s' failed: %w", track.Title, lastErr)
		}
		completedTracks++
		time.Sleep(500 * time.Millisecond)
	}
	if !verboseMode {
		fmt.Printf("\r✅ [%s] 100%% (%d/%d) Complete.                     \n", sourceType, totalTracks, totalTracks)
	}
	return nil
}
func downloadFileResumable(downloadUrl, finalFilePath string) error {
	partFilePath := finalFilePath + ".part"
	var startByte int64 = 0
	if info, err := os.Stat(partFilePath); err == nil {
		startByte = info.Size()
	}
	req, err := http.NewRequest("GET", downloadUrl, nil)
	if err != nil {
		return fmt.Errorf("req build error: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", "https://knigavuhe.org/")
	if startByte > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", startByte))
		if verboseMode {
			debugLog("🔄 Resuming download from byte %d", startByte)
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		if strings.Contains(err.Error(), "unexpected EOF") {
			return fmt.Errorf("connection cut by server (EOF): %w", err)
		}
		return fmt.Errorf("network failure: %w", err)
	}
	defer resp.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	switch resp.StatusCode {
	case http.StatusOK:
		if startByte > 0 && verboseMode {
			debugLog("⚠️ Server ignored Range header. Restarting from 0.")
		}
		flags |= os.O_TRUNC
		startByte = 0
	case http.StatusPartialContent:
		flags |= os.O_APPEND
	case http.StatusRequestedRangeNotSatisfiable:
		return fmt.Errorf("invalid range (file might be done or changed on server)")
	default:
		return fmt.Errorf("server HTTP %d (%s)", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	file, err := os.OpenFile(partFilePath, flags, 0644)
	if err != nil {
		return fmt.Errorf("file open error: %w", err)
	}
	n, err := io.Copy(file, resp.Body)
	file.Close()
	if err != nil {
		if err == io.ErrUnexpectedEOF {
			return fmt.Errorf("stream cut mid-download (saved %d bytes): %w", n, err)
		}
		return fmt.Errorf("write error: %w", err)
	}
	if resp.StatusCode == http.StatusOK && resp.ContentLength > 0 {
		if n != resp.ContentLength {
			return fmt.Errorf("incomplete (200 OK): got %d / %d", n, resp.ContentLength)
		}
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
func sortTracksNaturally(tracks []Audio) {
	sort.Slice(tracks, func(i, j int) bool {
		return compareNatural(tracks[i].Title, tracks[j].Title)
	})
}
func compareNatural(a, b string) bool {
	tokensA := tokenize(a)
	tokensB := tokenize(b)
	lenA, lenB := len(tokensA), len(tokensB)
	limit := min(lenB, lenA)
	for k := range limit {
		valA, errA := strconv.Atoi(tokensA[k])
		valB, errB := strconv.Atoi(tokensB[k])
		if errA == nil && errB == nil {
			if valA != valB {
				return valA < valB
			}
		} else {
			if strings.EqualFold(strings.ToLower(tokensA[k]), strings.ToLower(tokensB[k])) {
				return strings.ToLower(tokensA[k]) < strings.ToLower(tokensB[k])
			}
		}
	}
	return lenA < lenB
}
func tokenize(s string) []string {
	var tokens []string
	var currentToken strings.Builder
	isDigit := false
	for _, r := range s {
		newIsDigit := r >= '0' && r <= '9'
		if currentToken.Len() > 0 && newIsDigit != isDigit {
			tokens = append(tokens, currentToken.String())
			currentToken.Reset()
		}
		isDigit = newIsDigit
		currentToken.WriteRune(r)
	}
	if currentToken.Len() > 0 {
		tokens = append(tokens, currentToken.String())
	}
	return tokens
}
