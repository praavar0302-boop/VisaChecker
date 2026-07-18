package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/schollz/progressbar/v3"
)

const (
	workers = 6
	delay   = 100 * time.Millisecond
)

// visaInfo holds the normalized visa requirement for a country pair.
type visaInfo struct {
	Status string `json:"status"`
	Days   int    `json:"days,omitempty"`
}

// getURLSlug returns the URL name used on passportindex.org.
func getURLSlug(iso2 string) string {
	overrides := map[string]string{
		"cd": "congo-(dem.-rep.)",
		"ci": "cote-d'ivoire-(ivory-coast)",
		"sz": "eswatini",
		"ps": "palestinian-territories",
		"ru": "russian-federation",
		"vn": "viet-nam",
		"va": "vatican-city",
		"us": "united-states-of-america",
		"kp": "north-korea",
		"kr": "south-korea",
		"mk": "north-macedonia",
		"ss": "south-sudan",
		"cv": "cape-verde",
		"tl": "timor-leste",
		"tt": "trinidad-and-tobago",
		"tr": "türkiye",
		"vc": "st.-vincent-and-the-grenadines",
	}

	if slug, ok := overrides[iso2]; ok {
		return slug
	}

	name := iso2ToName[iso2]
	slug := strings.ToLower(name)
	slug = strings.ReplaceAll(slug, " and ", "-and-")
	slug = strings.ReplaceAll(slug, " ", "-")
	slug = strings.ReplaceAll(slug, "'", "")
	slug = strings.ReplaceAll(slug, ",", "")
	slug = strings.ReplaceAll(slug, ".", "")
	return slug
}

var (
	reTR     = regexp.MustCompile(`(?s)<tr[^>]*class="[^"]*show-tr[^"]*"[^>]*>.*?</tr>`)
	reClass  = regexp.MustCompile(`class="([^"]*)"`)
	reCode   = regexp.MustCompile(`flag-icon-([a-z]{2})`)
	reVrules = regexp.MustCompile(`class=['"]vrules['"]>(.*?)</span>`)
	reVdays  = regexp.MustCompile(`class=['"]vdays['"]>(?:\d+)</span>`)
)

func parseHTML(htmlContent string) (map[string]visaInfo, error) {
	trMatches := reTR.FindAllString(htmlContent, -1)
	if len(trMatches) == 0 {
		return nil, fmt.Errorf("no rows found")
	}

	result := make(map[string]visaInfo)
	for _, tr := range trMatches {
		classMatch := reClass.FindStringSubmatch(tr)
		codeMatch := reCode.FindStringSubmatch(tr)
		vrulesMatch := reVrules.FindStringSubmatch(tr)

		if len(classMatch) < 2 || len(codeMatch) < 2 || len(vrulesMatch) < 2 {
			continue
		}

		trClass := classMatch[1]
		destCode := strings.ToUpper(codeMatch[1])

		// Determine visa status from the class
		var status string
		if strings.Contains(trClass, "vf") {
			status = "visa free"
		} else if strings.Contains(trClass, "voa") {
			status = "visa on arrival"
		} else if strings.Contains(trClass, "eta") {
			status = "eta"
		} else if strings.Contains(trClass, "vr") {
			status = "visa required"
		} else {
			status = "visa required"
		}

		days := 0
		vdaysMatch := reVdays.FindStringSubmatch(tr)
		if len(vdaysMatch) >= 1 {
			// Extract number from match: e.g. class='vdays'>90</span>
			// Let's parse the digits inside the tag
			reDigits := regexp.MustCompile(`\d+`)
			digits := reDigits.FindString(vdaysMatch[0])
			if digits != "" {
				fmt.Sscanf(digits, "%d", &days)
			}
		}

		result[destCode] = visaInfo{
			Status: status,
			Days:   days,
		}
	}
	return result, nil
}

func scrapeCountry(client *http.Client, from string) (map[string]visaInfo, error) {
	slug := getURLSlug(from)
	url := fmt.Sprintf("https://www.passportindex.org/passport/%s/", slug)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("doing request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}

	return parseHTML(string(body))
}

// --- Sorting ----------------------------------------------------------------

func rowSortKey(iso2 string) string {
	if k, ok := sortKeyBase[iso2]; ok {
		return k
	}
	return iso2ToName[iso2]
}

func colSortKey(iso2 string) string {
	if k, ok := sortKeyCol[iso2]; ok {
		return k
	}
	return rowSortKey(iso2)
}

func sortCountries(keyFunc func(string) string) []string {
	sorted := make([]string, len(countryCodes))
	copy(sorted, countryCodes)
	sort.Slice(sorted, func(i, j int) bool {
		return keyFunc(sorted[i]) < keyFunc(sorted[j])
	})
	return sorted
}

// --- CSV output -------------------------------------------------------------

func visaValue(vi visaInfo) string {
	if strings.HasPrefix(vi.Status, "visa free") && vi.Days > 0 {
		return strconv.Itoa(vi.Days)
	}
	return vi.Status
}

func writeMatrixCSV(filename string, data map[string]map[string]visaInfo, codeFunc func(string) string) error {
	rows := sortCountries(rowSortKey)
	cols := append(sortCountries(colSortKey)[1:], sortCountries(colSortKey)[0])

	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	w.UseCRLF = false
	defer w.Flush()

	header := make([]string, len(cols)+1)
	header[0] = "Passport"
	for i, c := range cols {
		header[i+1] = codeFunc(c)
	}
	if err := w.Write(header); err != nil {
		return err
	}

	for _, from := range rows {
		row := make([]string, len(cols)+1)
		row[0] = codeFunc(from)
		for i, to := range cols {
			if from == to {
				row[i+1] = "-1"
			} else if vi, ok := data[strings.ToUpper(from)][strings.ToUpper(to)]; ok {
				row[i+1] = visaValue(vi)
			}
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	return nil
}

func writeTidyCSV(filename string, data map[string]map[string]visaInfo, codeFunc func(string) string) error {
	rows := sortCountries(rowSortKey)
	cols := append(sortCountries(colSortKey)[1:], sortCountries(colSortKey)[0])

	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	w.UseCRLF = false
	defer w.Flush()

	if err := w.Write([]string{"Passport", "Destination", "Requirement"}); err != nil {
		return err
	}

	for _, from := range rows {
		fromUpper := strings.ToUpper(from)
		for _, to := range cols {
			val := ""
			if from == to {
				val = "-1"
			} else if vi, ok := data[fromUpper][strings.ToUpper(to)]; ok {
				val = visaValue(vi)
			}
			if err := w.Write([]string{codeFunc(from), codeFunc(to), val}); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- Main -------------------------------------------------------------------

func main() {
	// Test mode: single request.
	if len(os.Args) >= 3 {
		client := &http.Client{Timeout: 10 * time.Second}
		from := os.Args[1]
		to := strings.ToUpper(os.Args[2])
		res, err := scrapeCountry(client, from)
		if err != nil {
			log.Fatalf("Error: %v", err)
		}
		vi, ok := res[to]
		if !ok {
			log.Fatalf("Destination %s not found in results for %s", to, from)
		}
		fmt.Printf("From=%s To=%s Status=%q Days=%d\n", from, to, vi.Status, vi.Days)
		return
	}

	// Full scrape mode.
	total := int64(len(countryCodes))
	log.Printf("Starting full HTML scrape: %d passports, %d workers", total, workers)

	bar := progressbar.NewOptions64(total,
		progressbar.OptionSetDescription("Scraping Passports"),
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionShowCount(),
		progressbar.OptionShowIts(),
		progressbar.OptionSetItsString("passports"),
		progressbar.OptionThrottle(200*time.Millisecond),
		progressbar.OptionShowElapsedTimeOnFinish(),
		progressbar.OptionSetPredictTime(true),
		progressbar.OptionFullWidth(),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "█",
			SaucerHead:    "█",
			SaucerPadding: "░",
			BarStart:      "",
			BarEnd:        "",
		}),
	)

	countriesChan := make(chan string, 100)
	go func() {
		for _, from := range countryCodes {
			countriesChan <- from
		}
		close(countriesChan)
	}()

	type scrapeResult struct {
		from  string
		rules map[string]visaInfo
	}
	results := make(chan scrapeResult, 100)
	var errCount atomic.Int64

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := &http.Client{Timeout: 15 * time.Second}
			for from := range countriesChan {
				res, err := scrapeCountry(client, from)
				bar.Add(1)
				if err != nil {
					errCount.Add(1)
					bar.Describe(fmt.Sprintf("Scraping (errors: %d)", errCount.Load()))
					log.Printf("Error scraping passport %s: %v", from, err)
					time.Sleep(1 * time.Second)
					continue
				}
				results <- scrapeResult{
					from:  strings.ToUpper(from),
					rules: res,
				}
				time.Sleep(delay)
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	data := make(map[string]map[string]visaInfo)
	var written int
	for r := range results {
		data[r.from] = r.rules
		written++
	}
	bar.Finish()
	fmt.Fprintln(os.Stderr)

	// Archive existing output files to ../history/<date> if history dir exists.
	outputFiles := []string{
		"passport-index.json",
		"passport-index-matrix.csv",
		"passport-index-matrix-iso2.csv",
		"passport-index-matrix-iso3.csv",
		"passport-index-tidy.csv",
		"passport-index-tidy-iso2.csv",
		"passport-index-tidy-iso3.csv",
	}
	historyBase := "history"
	if info, err := os.Stat(historyBase); err == nil && info.IsDir() {
		dateDir := fmt.Sprintf("%s/%s", historyBase, time.Now().Format("2006-01-02"))
		if err := os.MkdirAll(dateDir, 0o755); err != nil {
			log.Fatalf("Creating history dir: %v", err)
		}
		for _, src := range outputFiles {
			if _, err := os.Stat(src); err != nil {
				continue // file doesn't exist, skip
			}
			dst := fmt.Sprintf("%s/%s", dateDir, src)
			if err := os.Rename(src, dst); err != nil {
				log.Fatalf("Moving %s to %s: %v", src, dst, err)
			}
			log.Printf("Moved %s → %s", src, dst)
		}
	}

	// Write JSON.
	jf, err := os.Create("passport-index.json")
	if err != nil {
		log.Fatalf("Creating JSON file: %v", err)
	}
	enc := json.NewEncoder(jf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(data); err != nil {
		jf.Close()
		log.Fatalf("Writing JSON: %v", err)
	}
	jf.Close()
	log.Printf("Done! %d passports written, %d errors", written, errCount.Load())

	// Write CSVs.
	toName := func(iso2 string) string { return iso2ToName[iso2] }
	toISO2 := func(iso2 string) string { return strings.ToUpper(iso2) }
	toISO3 := func(iso2 string) string { return iso2ToISO3[iso2] }

	csvFiles := []struct {
		file    string
		fn      func(string) string
		writeFn func(string, map[string]map[string]visaInfo, func(string) string) error
	}{
		{"passport-index-matrix.csv", toName, writeMatrixCSV},
		{"passport-index-matrix-iso2.csv", toISO2, writeMatrixCSV},
		{"passport-index-matrix-iso3.csv", toISO3, writeMatrixCSV},
		{"passport-index-tidy.csv", toName, writeTidyCSV},
		{"passport-index-tidy-iso2.csv", toISO2, writeTidyCSV},
		{"passport-index-tidy-iso3.csv", toISO3, writeTidyCSV},
	}
	for _, spec := range csvFiles {
		if err := spec.writeFn(spec.file, data, spec.fn); err != nil {
			log.Fatalf("Writing %s: %v", spec.file, err)
		}
		log.Printf("Written %s", spec.file)
	}
}
