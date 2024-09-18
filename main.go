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
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/storage"
)

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
)

var db *leveldb.DB
var dbMutex sync.Mutex

func init() {
	prometheus.MustRegister(cidRequests)
	prometheus.MustRegister(uniqueCIDsCount)
	prometheus.MustRegister(beaconRequests)
}

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
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("Error: %v", err)
		}
	}
}

type LogEntry struct {
	Request struct {
		URI string `json:"uri"`
	} `json:"request"`
}

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

		// check for /ipfs/{cid} path
		if parsedURL, parseErr := url.Parse(uri); parseErr == nil {
			r := regexp.MustCompile(`/ipfs/([^/]+)`)
			if matches := r.FindStringSubmatch(parsedURL.Path); len(matches) > 1 {
				cid := matches[1]
				incrementCIDCount(cid)
			}
		}
	}
}

func incrementCIDCount(cid string) {
	dbMutex.Lock()
	defer dbMutex.Unlock()

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

func updateMetricsPeriodically() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		updateMetrics()
	}
}

func updateMetrics() {
	log.Println("Starting updateMetrics")
	dbMutex.Lock()
	defer dbMutex.Unlock()

	var cidCounts []struct {
		CID   string
		Count uint64
	}

	iter := db.NewIterator(nil, nil)
	for iter.Next() {
		cid := string(iter.Key())
		count := binary.BigEndian.Uint64(iter.Value())
		cidCounts = append(cidCounts, struct {
			CID   string
			Count uint64
		}{cid, count})
	}
	iter.Release()
	if len(cidCounts) > 0 {
		log.Printf("Finished iterating, found %d CIDs", len(cidCounts))
	}

	uniqueCIDsCount.Set(float64(len(cidCounts)))

	sort.Slice(cidCounts, func(i, j int) bool {
		return cidCounts[i].Count > cidCounts[j].Count
	})

	cidRequests.Reset()
	for i, cc := range cidCounts {
		if i >= 10 {
			break
		}
		cidRequests.WithLabelValues(cc.CID).Add(float64(cc.Count))
	}
	log.Println("Finished updateMetrics")
}

func main() {
	log.Println("Starting Caddy Log Monitor")

	var (
		logFilePath string
		port        int
		bind        string
		dbPath      string
		help        bool
	)

	flag.StringVar(&logFilePath, "log", "", "Path to the log file to monitor")
	flag.IntVar(&port, "port", 8080, "Port to listen on")
	flag.StringVar(&bind, "bind", "127.0.0.1", "Address to bind to")
	flag.StringVar(&dbPath, "db", "", "Path to LevelDB database (default: in-memory)")
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
	updateMetrics()
	log.Println("Metrics updated")

	log.Println("Starting file monitor")
	go monitorFile(logFilePath)

	log.Println("Starting metrics updater")
	go updateMetricsPeriodically()

	http.Handle("/metrics", promhttp.Handler())
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

func healthCheck(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
	log.Println("Health check request received")
}

func notFoundHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte("404 - Not Found"))
	log.Printf("404 Not Found: %s", r.URL.Path)
}
