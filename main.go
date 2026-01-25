package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
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
)

// Person represents author/reader
type Person struct {
	Name string `json:"name"`
}

// Book represents book metadata
type Book struct {
	Name    string      `json:"name"`
	Authors interface{} `json:"authors"` // Flexible type: map or slice
	Readers interface{} `json:"readers"` // Flexible type: map or slice
	Cover   string      `json:"cover"`
	URL     string      `json:"url"`
}

// Audio represents an audio track
type Audio struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// BookData contains all book information
type BookData struct {
	Book           Book    `json:"book"`
	Playlist       []Audio `json:"playlist"`
	MergedPlaylist []Audio `json:"merged_playlist"`
	CoverAlt       string  `json:"-"`
	Description    string  `json:"-"`
}

// BookInfo holds series book information
type BookInfo struct {
	URL         string
	DisplayName string // Includes series index (e.g., "1. Book Title")
	SeriesIndex string // Now string to handle "6.1"
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
	// Updated regex to handle indices like "6.1"
	bookItemRegex  = regexp.MustCompile(`(?s)<div class="bookitem">(.*?)</div>\s*</div>`)
	bookURLRegex   = regexp.MustCompile(`(?s)class="bookitem_cover"\s+href="/(book/[^"]+)"`)
	bookIndexRegex = regexp.MustCompile(`(?s)<span class="bookitem_serie_index">\s*([\d\.]+)\.\s*</span>`)
	bookTitleRegex = regexp.MustCompile(`(?s)<span class="bookitem_serie_index">\s*[\d\.]+\.\s*</span>\s*([^<]+)`)
	httpClient     = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:    maxIdleConns,
			IdleConnTimeout: 60 * time.Second,
		},
	}
)
var verboseMode bool

func debugLog(format string, v ...interface{}) {
	if verboseMode {
		log.Printf("[DEBUG] "+format, v...)
	}
}

func main() {
	// 1. Setup Flags
	flag.BoolVar(&verboseMode, "verbose", false, "Show detailed technical logs")
	flag.Parse()

	// 2. Validate Arguments (use flag.Args() instead of os.Args)
	args := flag.Args()
	if len(args) < 2 {
		fmt.Println("Usage:")
		fmt.Println("  For series: audiobook-downloader <output-dir> <series-url> [-verbose]")
		fmt.Println("  For file:   audiobook-downloader <output-dir> <url-file.txt> [-verbose]")
		fmt.Println("  <output-dir>   : Directory to save audiobooks")
		os.Exit(1)
	}

	outputDir := args[0]
	inputArg := args[1]

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		log.Fatalf("Error creating output directory: %v", err)
	}

	var results []DownloadResult
	// Check if input is a file
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

			// Check if line is a series URL
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
				// Treat as individual book URL
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
		results = processDownloads(outputDir, allBooks)
	} else {
		// Treat as series URL
		fmt.Printf("🔍 Extracting books from series: %s\n", inputArg)
		books, err := extractBooksFromSeries(inputArg)
		if err != nil {
			log.Fatalf("Error extracting books from series: %v", err)
		}

		if len(books) == 0 {
			log.Fatal("❌ No books found in series. Run with DEBUG=1 for more info.")
		}

		fmt.Printf("✅ Found %d books\n", len(books))
		results = processDownloads(outputDir, books)
	}

	// Print summary
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
		lastPart = strings.TrimSuffix(lastPart, "/") // <- fix for S1017

		// Remove "book/123-" prefix if present
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

	for i, match := range matches {
		content := match[1]

		urlMatch := bookURLRegex.FindStringSubmatch(content)
		if len(urlMatch) < 2 {
			if os.Getenv("DEBUG") == "1" {
				fmt.Printf("Debug: Item %d - No URL match\n", i+1)
			}
			continue
		}

		indexMatch := bookIndexRegex.FindStringSubmatch(content)
		if len(indexMatch) < 2 {
			if os.Getenv("DEBUG") == "1" {
				fmt.Printf("Debug: Item %d - No index match\n", i+1)
			}
			continue
		}
		index := strings.TrimSpace(indexMatch[1])

		titleMatch := bookTitleRegex.FindStringSubmatch(content)
		if len(titleMatch) < 2 {
			if os.Getenv("DEBUG") == "1" {
				fmt.Printf("Debug: Item %d - No title match\n", i+1)
			}
			continue
		}
		title := strings.TrimSpace(titleMatch[1])

		displayName := fmt.Sprintf("%s. %s", index, title)
		books = append(books, BookInfo{
			URL:         "https://knigavuhe.org/" + urlMatch[1],
			DisplayName: displayName,
			SeriesIndex: index,
		})
	}

	// Sort by series index using version number comparison
	sort.Slice(books, func(i, j int) bool {
		return compareIndices(books[i].SeriesIndex, books[j].SeriesIndex)
	})

	return books, nil
}

// compareIndices compares version-like indices (e.g., "1", "2", "6.1", "6.10")
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

func processDownloads(outputDir string, books []BookInfo) []DownloadResult {
	var wg sync.WaitGroup
	bookChan := make(chan BookInfo, len(books))
	resultChan := make(chan DownloadResult, len(books))

	workers := runtime.NumCPU() * 2
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for book := range bookChan {
				result := downloadBook(book.URL, outputDir, book.DisplayName)
				resultChan <- result
			}
		}()
	}

	for i, book := range books {
		fmt.Printf("📝 Queuing %d/%d: %s\n", i+1, len(books), book.DisplayName)
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

	// Sort results by series index
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
	safe = strings.Trim(safe, " .")

	reserved := []string{"CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9"}

	for _, r := range reserved {
		if strings.EqualFold(safe, r) {
			return safe + "_"
		}
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
	// DEBUG: Inspect the data structure
	debugLog("Playlist Check for: %s", bookData.Book.Name)
	debugLog(" > Standard Tracks: %d", len(bookData.Playlist))
	debugLog(" > Merged Tracks:   %d", len(bookData.MergedPlaylist))

	if len(bookData.Playlist) > 0 {
		debugLog(" > Sample Track URL: %s", bookData.Playlist[0].URL)
	}

	// Attempt 1: Standard Playlist
	err := downloadTracks(bookDir, bookData.Playlist, "STANDARD")
	if err == nil {
		return nil
	}

	// Log technical error only in verbose mode
	debugLog("🔴 Standard Playlist Failed: %v", err)

	// Inform user cleanly that we are falling back
	fmt.Printf("   ⚠️  Standard download failed, trying merged file...\n")

	// Attempt 2: Merged Playlist
	if err := downloadTracks(bookDir, bookData.MergedPlaylist, "MERGED"); err != nil {
		// Only add error to result if BOTH fail
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
	sem := make(chan struct{}, 5) // Concurrency limit

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

			// --- FIX: Remove query parameters like ?1 from URL ---
			cleanURL := strings.Split(t.URL, "?")[0]
			ext := filepath.Ext(cleanURL)
			if ext == "" {
				ext = ".mp3"
			}

			// Sanitize filename
			filePath := filepath.Join(bookDir, sanitizePath(t.Title)+ext)

			if _, err := os.Stat(filePath); err == nil {
				return // File exists
			}

			var lastErr error
			for attempt := 1; attempt <= maxRetries; attempt++ {
				// We use the verbose downloader helper
				if err := downloadFileVerbose(t.URL, filePath); err == nil {
					return
				} else {
					lastErr = err
					// Only log retries in verbose mode
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

func downloadFileVerbose(url, filePath string) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("req build error: %w", err)
	}

	req.Header.Set("User-Agent", userAgent)
	// Some sites require Referer to allow direct mp3 downloads
	req.Header.Set("Referer", "https://knigavuhe.org/")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// DEBUG 4: Capture the HTTP Code
		return fmt.Errorf("HTTP %d (%s)", resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	// Check for zero-byte files (common soft-fail)
	if resp.ContentLength == 0 {
		return fmt.Errorf("HTTP body is empty (Content-Length: 0)")
	}

	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("file create error: %w", err)
	}
	defer file.Close()

	n, err := io.Copy(file, resp.Body)
	if err != nil {
		return fmt.Errorf("io.Copy error: %w", err)
	}

	if n == 0 {
		os.Remove(filePath) // Cleanup empty file
		return fmt.Errorf("wrote 0 bytes")
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

// DownloadResult contains download status
type DownloadResult struct {
	URL      string
	BookName string
	Path     string
	Errors   []string
}

func (r *DownloadResult) addError(context string, err error) {
	r.Errors = append(r.Errors, fmt.Sprintf("%s: %v", context, err))
}
