package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/storage"
)

type LogEntry struct {
	Request struct {
		URI string `json:"uri"`
	} `json:"request"`
}

var (
	cidRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wapo_ipfs_cid_requests_total",
			Help: "Total number of requests for top 10 CIDs",
		},
		[]string{"cid"},
	)
	uniqueCIDsCount = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "wapo_ipfs_unique_cids_total",
			Help: "Total number of unique CIDs",
		},
	)
	beaconRequests = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "wapo_beacon_requests_total",
			Help: "Total number of requests to the /_/beacon path",
		},
	)
	quoteRequests = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "wapo_quote_requests_total",
			Help: "Total number of requests to the /_/quote path",
		},
	)
	dailyRequestCount = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "wapo_ipfs_daily_requests_total",
			Help: "Total number of requests in the last 24 hours",
		},
	)
)

var db *leveldb.DB
var dbMutex sync.Mutex
var top10CIDs []string
var top10CIDsMutex sync.Mutex

func init() {
	// Initialize and register Prometheus metrics
	prometheus.MustRegister(cidRequests)
	prometheus.MustRegister(uniqueCIDsCount)
	prometheus.MustRegister(beaconRequests)
  prometheus.MustRegister(quoteRequests)
	prometheus.MustRegister(dailyRequestCount)
}

// loadDataFromDB loads CID request counts from the database into Prometheus metrics
func loadDataFromDB() {
	dbMutex.Lock()
	defer dbMutex.Unlock()

	iter := db.NewIterator(nil, nil)
	defer iter.Release()

	count := 0
	log.Println("Starting to iterate over database")
	for iter.Next() {
		cid := string(iter.Key())
		value := binary.BigEndian.Uint64(iter.Value())
		cidRequests.WithLabelValues(cid).Add(float64(value))
		count++
		if count%1000 == 0 {
			log.Printf("Processed %d entries", count)
			dbMutex.Unlock()
			time.Sleep(time.Millisecond)
			dbMutex.Lock()
		}
	}

	if err := iter.Error(); err != nil {
		log.Printf("Error iterating over database: %v", err)
	}
}

func monitorFile(filePath string) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalf("Error creating watcher: %v", err)
	}
	defer watcher.Close()

	err = watcher.Add(filePath)
	if err != nil {
		log.Fatalf("Error adding file to watcher: %v", err)
	}

	file, err := os.Open(filePath)
	if err != nil {
		log.Fatalf("Error opening file: %v", err)
	}
	defer file.Close()

	_, err = file.Seek(0, io.SeekEnd)
	if err != nil {
		log.Fatalf("Error seeking to end of file: %v", err)
	}

	reader := bufio.NewReader(file)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&fsnotify.Write == fsnotify.Write {
				processNewLines(reader)
			} else if event.Op&fsnotify.Remove == fsnotify.Remove || event.Op&fsnotify.Rename == fsnotify.Rename {
				file, reader = reopenFile(filePath)
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("Error: %v", err)
		}
	}
}

func processFile(filePath string) {
	file, err := os.Open(filePath)
	if err != nil {
		log.Fatalf("Error opening file: %v", err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	processNewLines(reader)
}

func reopenFile(filePath string) (*os.File, *bufio.Reader) {
	for {
		if _, err := os.Stat(filePath); err == nil {
			file, err := os.Open(filePath)
			if err != nil {
				log.Printf("Error reopening file: %v", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return file, bufio.NewReader(file)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// processNewLines reads and processes new lines from the given reader
func processNewLines(reader *bufio.Reader) {
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				log.Printf("Error reading line: %v", err)
			}
			break
		}

		var logEntry LogEntry
		err = json.Unmarshal([]byte(line), &logEntry)
		if err != nil {
			log.Printf("Error parsing JSON: %v", err)
			continue
		}

		uri := logEntry.Request.URI

		// Check for /_/beacon path
		if uri == "/_/beacon" {
			beaconRequests.Inc()
		}
		if uri == "/_/quote" {
			quoteRequests.Inc()
		}

		// check for /ipfs/{cid} path
		if parsedURL, parseErr := url.Parse(uri); parseErr == nil {
			r := regexp.MustCompile(`/ipfs/([^/]+)`)
			if matches := r.FindStringSubmatch(parsedURL.Path); len(matches) > 1 {
				cid := "ipfs://" + matches[1]

				dailyRequestCount.Inc()

				top10CIDsMutex.Lock()
				if len(top10CIDs) < 10 {
					top10CIDs = append(top10CIDs, cid)

				}
				top10CIDsMutex.Unlock()

				if index := sort.SearchStrings(top10CIDs, cid); index < len(top10CIDs) && top10CIDs[index] == cid {
					cidRequests.WithLabelValues(cid).Inc()
				}

				incrementCIDCount(cid)
			}
		}
	}
}

// incrementCIDCount increments the count for a given CID in the database
func incrementCIDCount(cid string) {
	dbMutex.Lock()
	defer dbMutex.Unlock()

	if exists, err := db.Has([]byte(cid), nil); err == nil && !exists {
		uniqueCIDsCount.Inc()
	}

	var count uint64
	data, err := db.Get([]byte(cid), nil)
	if err == nil {
		count = binary.BigEndian.Uint64(data)
	}

	count++
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, count)

	err = db.Put([]byte(cid), buf, nil)
	if err != nil {
		log.Printf("Error writing to database: %v", err)
	}
}

// updateMetricsPeriodically updates metrics at regular intervals
func updateMetricsPeriodically() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		refreshMetrics()

		now := time.Now().UTC()
		if now.Hour() == 0 && now.Minute() == 0 && now.Second() < 5 {
			dailyRequestCount.Set(0)
			log.Println("Daily request counter reset at UTC 00:00:00")
		}
	}
}

func refreshMetrics() {
	dbMutex.Lock()
	top10CIDsMutex.Lock()
	defer dbMutex.Unlock()
	defer top10CIDsMutex.Unlock()

	var cidCounts []struct {
		CID   string
		Count uint64
	}
	var totalCid float64

	iter := db.NewIterator(nil, nil)
	for iter.Next() {
		cid := string(iter.Key())
		count := binary.BigEndian.Uint64(iter.Value())

		if !strings.HasPrefix(cid, "ipfs://") {
			continue
		}

		totalCid++

		cidCounts = append(cidCounts, struct {
			CID   string
			Count uint64
		}{cid, count})

		sort.Slice(cidCounts, func(i, j int) bool {
			return cidCounts[i].Count > cidCounts[j].Count
		})

		if len(cidCounts) > 10 {
			cidCounts = cidCounts[:10]
		}
	}
	iter.Release()

	uniqueCIDsCount.Set(totalCid)
	cidRequests.Reset()
	top10CIDs = top10CIDs[:0]
	for _, cc := range cidCounts {
		cidRequests.WithLabelValues(cc.CID).Add(float64(cc.Count))
		top10CIDs = append(top10CIDs, cc.CID)
	}
}

// handleMetrics handles the /api/metrics endpoint and returns Prometheus metrics in JSON format
func handleMetrics(w http.ResponseWriter, r *http.Request) {
	if metrics, err := prometheus.DefaultGatherer.Gather(); err == nil {
		jsonMetrics := make(map[string]interface{})

		for _, mf := range metrics {
			name := *mf.Name
			help := *mf.Help
			metricType := mf.Type.String()

			jsonMetrics[name] = map[string]interface{}{
				"help":   help,
				"type":   metricType,
				"values": make([]map[string]interface{}, 0),
			}

			for _, m := range mf.Metric {
				labels := make(map[string]string)
				for _, l := range m.Label {
					labels[*l.Name] = *l.Value
				}

				var value float64
				switch metricType {
				case "COUNTER":
					value = *m.Counter.Value
				case "GAUGE":
					value = *m.Gauge.Value
				case "HISTOGRAM":
					value = *m.Histogram.SampleSum
				case "SUMMARY":
					value = *m.Summary.SampleSum
				}

				jsonMetrics[name].(map[string]interface{})["values"] = append(
					jsonMetrics[name].(map[string]interface{})["values"].([]map[string]interface{}),
					map[string]interface{}{
						"labels": labels,
						"value":  value,
					},
				)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jsonMetrics)
	} else {
		log.Printf("Error gathering metrics: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
}

// Echo plaintext "OK" to indicate that the server is running
func healthCheck(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
	log.Println("Health check request received")
}

// Handles requests to the root path
func notFoundHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte("404 - Not Found"))
	log.Printf("404 Not Found: %s", r.URL.Path)
}

func main() {
	log.Println("Starting Caddy Log Monitor")

	var (
		logFilePath string
		port        int
		bind        string
		dbPath      string
		help        bool
		reread      bool
	)

	flag.IntVar(&port, "port", 8080, "Port to listen on")
	flag.StringVar(&bind, "bind", "127.0.0.1", "Address to bind to")
	flag.StringVar(&dbPath, "db", "", "Path to LevelDB database (default: in-memory)")
	flag.BoolVar(&reread, "reread", false, "Reread the log file from the beginning")
	flag.BoolVar(&help, "h", false, "Show help")
	flag.BoolVar(&help, "help", false, "Show help")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] <log_file_path>\n", os.Args[0])
		flag.PrintDefaults()
	}

	flag.Parse()

	if help {
		flag.Usage()
		os.Exit(0)
	}

	if logFilePath == "" && flag.NArg() > 0 {
		logFilePath = flag.Arg(0)
	}

	if logFilePath == "" {
		log.Println("Error: Log file path is required")
		flag.Usage()
		os.Exit(1)
	}

	log.Printf("Log file path: %s", logFilePath)
	log.Printf("Port: %d", port)
	log.Printf("Bind address: %s", bind)
	log.Printf("Database path: %s", dbPath)

	var err error
	if dbPath == "" {
		log.Println("Using in-memory storage")
		memStorage := storage.NewMemStorage()
		db, err = leveldb.Open(memStorage, nil)
	} else {
		log.Printf("Opening database at %s", dbPath)
		db, err = leveldb.OpenFile(dbPath, nil)
	}
	if err != nil {
		log.Fatalf("Error opening database: %v", err)
	}
	defer db.Close()

	log.Println("Loading data from database")
	loadDataFromDB()

	log.Println("Finished iterating over database")
	log.Println("Updating metrics")
	if reread {
		processFile(logFilePath)
	}
	refreshMetrics()
	log.Println("Metrics updated")

	log.Println("Starting file monitor")
	go monitorFile(logFilePath)

	log.Println("Starting metrics updater")
	go updateMetricsPeriodically()

	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/api/metrics", handleMetrics)
	http.HandleFunc("/health", healthCheck)
	http.HandleFunc("/", notFoundHandler)

	listenAddr := fmt.Sprintf("%s:%d", bind, port)
	log.Printf("Starting server on %s", listenAddr)

	server := &http.Server{
		Addr:    listenAddr,
		Handler: http.DefaultServeMux,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Error starting server: %v", err)
		}
	}()

	log.Println("Server started successfully")

	// Keep the main goroutine running
	select {}
}
