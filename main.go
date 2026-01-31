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

var (
	version, gitCommit, buildDate, goVersion, updateURL string
	verboseMode                                         bool
	bookConcurrency, trackConcurrency                   int
)

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

// --- State & Globals ---
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

func (s *DownloadState) UpdateStatus(book string, status BookStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Books[book] = status
	data, _ := json.MarshalIndent(s, "", "  ")
	os.WriteFile(s.path, data, 0644)
}

func (s *DownloadState) GetStatus(book string) BookStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Books[book]
}

const (
	userAgent  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36 Edg/135.0.0.0"
	maxRetries = 20
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
	singleIndexRegex = regexp.MustCompile(`class="book_info_line_serie_index">\(([\d\.]+)\)`)
	singleTitleRegex = regexp.MustCompile(`<h2 class="book_title"[^>]*>(.*?)</h2>`)

	httpClient = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns: 10,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, _ := net.SplitHostPort(addr)
				if net.ParseIP(host) != nil {
					return (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, network, addr)
				}
				resolvers := []*net.Resolver{
					net.DefaultResolver,
					{PreferGo: true, Dial: func(ctx context.Context, n, a string) (net.Conn, error) { return net.Dial("udp", "8.8.8.8:53") }},
					{PreferGo: true, Dial: func(ctx context.Context, n, a string) (net.Conn, error) { return net.Dial("udp", "1.1.1.1:53") }},
				}
				for _, r := range resolvers {
					if ips, err := r.LookupHost(ctx, host); err == nil && len(ips) > 0 {
						target := ips[0]
						if strings.Contains(target, ":") {
							target = "[" + target + "]"
						}
						return (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, network, target+":"+port)
					}
				}
				return nil, fmt.Errorf("DNS resolution failed for %s", host)
			},
		},
	}
)

func main() {
	if runtime.GOOS == "windows" {
		os.Remove(os.Args[0] + ".old")
	}

	var showVersion, waitUpdate bool
	flag.BoolVar(&verboseMode, "verbose", false, "Show detailed technical logs")
	flag.BoolVar(&showVersion, "version", false, "Print version")
	flag.BoolVar(&waitUpdate, "wait-update", false, "Wait for update")
	flag.IntVar(&bookConcurrency, "book-workers", 1, "Book concurrency")
	flag.IntVar(&trackConcurrency, "track-workers", 5, "Track concurrency")
	flag.Parse()

	if showVersion {
		fmt.Printf("Ver: %s, Commit: %s, Date: %s, Go: %s\n", version, gitCommit, buildDate, goVersion)
		os.Exit(0)
	}

	if updateURL != "" {
		for {
			updated, err := checkAndApplyUpdate()
			if updated {
				fmt.Println("✅ Updated. Restarting.")
				os.Exit(0)
			}
			if err != nil && verboseMode {
				fmt.Printf("Update check error: %v\n", err)
			}
			if !waitUpdate {
				break
			}
			time.Sleep(5 * time.Second)
		}
	} else if waitUpdate {
		log.Fatal("❌ No update URL injected.")
	}

	args := flag.Args()
	if len(args) < 2 {
		fmt.Println("Usage: go-knigavuhe [flags] <output-dir> <url-or-file>")
		os.Exit(1)
	}
	outputDir, inputArg := args[0], args[1]
	os.MkdirAll(outputDir, 0755)

	statePath := filepath.Join(outputDir, "state.json")
	state := &DownloadState{Books: make(map[string]BookStatus), path: statePath}
	if data, err := os.ReadFile(statePath); err == nil {
		json.Unmarshal(data, state)
	}

	var books []BookInfo
	if strings.HasSuffix(inputArg, ".txt") || isFile(inputArg) {
		lines, _ := readLines(inputArg)
		for _, line := range lines {
			if strings.Contains(line, "/serie/") {
				if b, err := extractBooksFromSeries(line); err == nil {
					books = append(books, b...)
				}
			} else {
				books = append(books, BookInfo{URL: line, DisplayName: "Unknown", SeriesIndex: ""})
			}
		}
	} else if strings.Contains(inputArg, "/serie/") {
		if b, err := extractBooksFromSeries(inputArg); err == nil {
			books = append(books, b...)
		}
	} else {
		books = append(books, BookInfo{URL: inputArg, DisplayName: "Unknown", SeriesIndex: ""})
	}

	if len(books) == 0 {
		log.Fatal("❌ No books found.")
	}

	var wg sync.WaitGroup
	bookChan := make(chan BookInfo, len(books))
	workers := bookConcurrency
	if workers < 1 {
		workers = 1
	}

	fmt.Printf("🚀 Starting processing for %d books...\n", len(books))

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for book := range bookChan {
				if book.DisplayName != "Unknown" && state.GetStatus(book.DisplayName) == StatusCompleted {
					fmt.Printf("⏭️  Skipping %s\n", book.DisplayName)
					continue
				}

				res := downloadBook(book.URL, outputDir, book.DisplayName, state)

				status := StatusCompleted
				if len(res.Errors) > 0 {
					status = StatusFailed
					// Force newline if progress bar was active
					if bookConcurrency == 1 {
						fmt.Println()
					}
					fmt.Printf("❌ %s failed: %v\n", res.BookName, res.Errors)
				} else {
					if bookConcurrency == 1 {
						fmt.Println()
					} // Newline after progress bar
					// Just mark completion, downloadBook handles the "Finished" log
				}
				state.UpdateStatus(res.BookName, status)
			}
		}()
	}

	for _, book := range books {
		bookChan <- book
	}
	close(bookChan)
	wg.Wait()

	allDone := true
	for _, s := range state.Books {
		if s != StatusCompleted {
			allDone = false
			break
		}
	}
	if allDone {
		os.Remove(statePath)
	}
}

func checkAndApplyUpdate() (bool, error) {
	req, _ := http.NewRequest("GET", strings.TrimRight(updateURL, "/")+"/version.txt", nil)
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	remoteVer, _ := io.ReadAll(resp.Body)
	if string(bytes.TrimSpace(remoteVer)) == version {
		return false, nil
	}

	binName := fmt.Sprintf("go-knigavuhe-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	resp, err = httpClient.Get(strings.TrimRight(updateURL, "/") + "/" + binName)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	return true, selfupdate.Apply(resp.Body, selfupdate.Options{})
}

func isFile(f string) bool { _, err := os.Stat(f); return err == nil }
func readLines(f string) ([]string, error) {
	file, err := os.Open(f)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if t := strings.TrimSpace(scanner.Text()); t != "" {
			lines = append(lines, t)
		}
	}
	return lines, nil
}

func extractBooksFromSeries(url string) ([]BookInfo, error) {
	html, err := downloadPage(url)
	if err != nil {
		return nil, err
	}
	var books []BookInfo
	matches := bookItemRegex.FindAllStringSubmatch(html, -1)
	for _, match := range matches {
		content := match[1]
		u := bookURLRegex.FindStringSubmatch(content)
		idx := bookIndexRegex.FindStringSubmatch(content)
		t := bookTitleRegex.FindStringSubmatch(content)
		if len(u) > 1 && len(idx) > 1 && len(t) > 1 {
			books = append(books, BookInfo{
				URL:         "https://knigavuhe.org/" + u[1],
				DisplayName: strings.TrimSpace(idx[1]) + "_" + strings.TrimSpace(t[1]),
				SeriesIndex: strings.TrimSpace(idx[1]),
			})
		}
	}
	sort.Slice(books, func(i, j int) bool { return compareIndices(books[i].SeriesIndex, books[j].SeriesIndex) })
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

type DownloadResult struct {
	URL, BookName, Path string
	Errors              []string
}

func downloadBook(bookURL, outputDir, providedName string, state *DownloadState) DownloadResult {
	res := DownloadResult{URL: bookURL}
	u, _ := url.Parse(bookURL)
	if u.Host == "m.knigavuhe.org" {
		u.Host = "knigavuhe.org"
	}
	u.Scheme = "https"

	html, err := downloadPage(u.String())
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
		return res
	}

	// --- Resolve Name and Index ---
	finalName := providedName
	idxMatch := singleIndexRegex.FindStringSubmatch(html)
	titleMatch := singleTitleRegex.FindStringSubmatch(html)

	if finalName == "Unknown" || finalName == "" {
		if len(titleMatch) > 1 {
			finalName = strings.TrimSpace(titleMatch[1])
			if len(idxMatch) > 1 {
				finalName = strings.TrimSpace(idxMatch[1]) + "_" + finalName
			}
		} else {
			parts := strings.Split(strings.TrimSuffix(u.Path, "/"), "/")
			if len(parts) > 0 {
				finalName = parts[len(parts)-1]
			}
		}
	} else if len(idxMatch) > 1 && !strings.Contains(providedName, "_") {
		finalName = strings.TrimSpace(idxMatch[1]) + "_" + finalName
	}

	res.BookName = finalName

	if state.GetStatus(finalName) == StatusCompleted {
		fmt.Printf("⏭️  Skipping %s (Already completed)\n", finalName)
		res.Path = filepath.Join(outputDir, sanitizePath(finalName))
		return res
	}

	state.UpdateStatus(finalName, StatusDownloading)

	matches := jsonRegex.FindStringSubmatch(html)
	if len(matches) < 2 {
		res.Errors = append(res.Errors, "No JSON")
		return res
	}

	data := &BookData{}
	if err := json.Unmarshal([]byte(matches[1]), data); err != nil {
		res.Errors = append(res.Errors, err.Error())
		return res
	}
	data.CoverAlt = strings.Split(data.Book.Cover, "-")[0] + ".jpg"
	if d := descRegex.FindStringSubmatch(html); len(d) > 1 {
		data.Description = regexp.MustCompile(`<[^>]*>`).ReplaceAllString(d[1], "")
	}

	bookDir := filepath.Join(outputDir, sanitizePath(finalName))
	os.MkdirAll(bookDir, 0755)
	res.Path = bookDir

	downloadFileWithFallback([]string{data.CoverAlt, data.Book.Cover}, filepath.Join(bookDir, "cover.jpg"))
	saveDescription(bookDir, data)

	if bookConcurrency > 1 {
		fmt.Printf("⬇️  Starting %s (%d tracks)\n", finalName, len(data.Playlist))
	} else {
		// Just a newline to separate from previous logs
		fmt.Printf("\n📂 %s (%d tracks)\n", finalName, len(data.Playlist))
	}

	if err := downloadTracks(bookDir, data.Playlist, finalName); err != nil {
		if err := downloadTracks(bookDir, data.MergedPlaylist, finalName); err != nil {
			res.Errors = append(res.Errors, "All download methods failed")
		}
	}
	return res
}

func downloadPage(u string) (string, error) {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Cookie", "new_design=1")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	return string(b), err
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

func downloadFileWithFallback(urls []string, dest string) {
	for _, u := range urls {
		if u == "" {
			continue
		}
		if err := downloadFile(u, dest); err == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func downloadTracks(dir string, tracks []Audio, bookName string) error {
	if len(tracks) == 0 {
		return fmt.Errorf("empty playlist")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wc := trackConcurrency
	if wc < 1 {
		wc = 1
	}
	sem := make(chan struct{}, wc)
	errChan := make(chan error, 1)
	var wg sync.WaitGroup

	var completed int32 = 0
	total := int32(len(tracks))

	// Only show progress bar if strictly sequential books
	showProgress := bookConcurrency == 1

	if showProgress {
		fmt.Printf("\r⏳ [%s] 0/%d", bookName, total)
	}

	for _, t := range tracks {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		wg.Add(1)
		go func(t Audio) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			ext := filepath.Ext(strings.Split(t.URL, "?")[0])
			if ext == "" {
				ext = ".mp3"
			}
			dest := filepath.Join(dir, sanitizePath(t.Title)+strings.ToLower(ext))

			success := false
			if _, err := os.Stat(dest); err == nil {
				success = true
			} else {
				for i := 0; i < maxRetries; i++ {
					if err := downloadFile(t.URL, dest); err == nil {
						success = true
						break
					}
					time.Sleep(retryDelay)
				}
			}

			if success {
				if showProgress {
					c := atomic.AddInt32(&completed, 1)
					p := float64(c) / float64(total) * 100
					// %-70s pads the string to 70 chars with spaces to wipe artifacts
					status := fmt.Sprintf("⏳ [%s] %d/%d (%.0f%%)", bookName, c, total, p)
					fmt.Printf("\r%-80s", status)
				}
			} else {
				select {
				case errChan <- fmt.Errorf("failed: %s", t.Title):
					cancel()
				default:
				}
			}
		}(t)
	}
	wg.Wait()
	close(errChan)

	if showProgress {
		// Clean finish line
		fmt.Printf("\r✅ [%s] Completed %d tracks.%-40s", bookName, total, "")
	}

	return <-errChan
}

func downloadFile(url, dest string) error {
	part := dest + ".part"
	req, _ := http.NewRequest("GET", url, nil)
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
	n, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil || n == 0 {
		os.Remove(part)
		return err
	}
	return os.Rename(part, dest)
}
