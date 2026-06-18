package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type DiskStats struct {
	Name            string `json:"name"`
	ReadsCompleted  uint64 `json:"reads_completed"`
	ReadsMerged     uint64 `json:"reads_merged"`
	SectorsRead     uint64 `json:"sectors_read"`
	ReadTimeMs      uint64 `json:"read_time_ms"`
	WritesCompleted uint64 `json:"writes_completed"`
	WritesMerged    uint64 `json:"writes_merged"`
	SectorsWritten  uint64 `json:"sectors_written"`
	WriteTimeMs     uint64 `json:"write_time_ms"`
	IOPending       uint64 `json:"io_pending"`
	IOTimeMs        uint64 `json:"io_time_ms"`
	WeightedIOTime  uint64 `json:"weighted_io_time_ms"`
}

type DiskMetrics struct {
	Name              string  `json:"name"`
	ReadBytesPerSec   float64 `json:"read_bytes_per_sec"`
	WriteBytesPerSec  float64 `json:"write_bytes_per_sec"`
	ReadIOPS          float64 `json:"read_iops"`
	WriteIOPS         float64 `json:"write_iops"`
	AvgReadWaitMs     float64 `json:"avg_read_wait_ms"`
	AvgWriteWaitMs    float64 `json:"avg_write_wait_ms"`
	AvgIOWaitMs       float64 `json:"avg_io_wait_ms"`
	UtilizationPct    float64 `json:"utilization_pct"`
	IOPending         uint64  `json:"io_pending"`
}

type MetricsResponse struct {
	Timestamp time.Time     `json:"timestamp"`
	Disks     []DiskMetrics `json:"disks"`
}

const (
	sectorSize     = 512
	diskStatsPath  = "/proc/diskstats"
	monitorPort    = ":8080"
)

var (
	prevStats   map[string]DiskStats
	prevTime    time.Time
	statsMutex  sync.RWMutex

	sdaPattern    = regexp.MustCompile(`^[a-z]+d[a-z]+$`)
	nvmePattern   = regexp.MustCompile(`^nvme\d+n\d+$`)
	mmcPattern    = regexp.MustCompile(`^mmcblk\d+$`)
	mdPattern     = regexp.MustCompile(`^md\d+$`)
	vdaPattern    = regexp.MustCompile(`^vd[a-z]+$`)
	xvdPattern    = regexp.MustCompile(`^xvd[a-z]+$`)
)

func readDiskStats() (map[string]DiskStats, error) {
	file, err := os.Open(diskStatsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", diskStatsPath, err)
	}
	defer file.Close()

	stats := make(map[string]DiskStats)
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 14 {
			continue
		}

		name := fields[2]
		if !isPhysicalDisk(name) {
			continue
		}

		ds := DiskStats{Name: name}
		ds.ReadsCompleted, _ = strconv.ParseUint(fields[3], 10, 64)
		ds.ReadsMerged, _ = strconv.ParseUint(fields[4], 10, 64)
		ds.SectorsRead, _ = strconv.ParseUint(fields[5], 10, 64)
		ds.ReadTimeMs, _ = strconv.ParseUint(fields[6], 10, 64)
		ds.WritesCompleted, _ = strconv.ParseUint(fields[7], 10, 64)
		ds.WritesMerged, _ = strconv.ParseUint(fields[8], 10, 64)
		ds.SectorsWritten, _ = strconv.ParseUint(fields[9], 10, 64)
		ds.WriteTimeMs, _ = strconv.ParseUint(fields[10], 10, 64)
		ds.IOPending, _ = strconv.ParseUint(fields[11], 10, 64)
		ds.IOTimeMs, _ = strconv.ParseUint(fields[12], 10, 64)
		ds.WeightedIOTime, _ = strconv.ParseUint(fields[13], 10, 64)

		stats[name] = ds
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading %s: %w", diskStatsPath, err)
	}

	return stats, nil
}

func isPhysicalDisk(name string) bool {
	if len(name) == 0 {
		return false
	}
	if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "sr") {
		return false
	}
	if sdaPattern.MatchString(name) {
		return true
	}
	if nvmePattern.MatchString(name) {
		return true
	}
	if mmcPattern.MatchString(name) {
		return true
	}
	if mdPattern.MatchString(name) {
		return true
	}
	if vdaPattern.MatchString(name) {
		return true
	}
	if xvdPattern.MatchString(name) {
		return true
	}
	return false
}

func calculateMetrics(curr, prev map[string]DiskStats, deltaTime time.Duration) []DiskMetrics {
	metrics := make([]DiskMetrics, 0)
	deltaSec := deltaTime.Seconds()

	names := make([]string, 0, len(curr))
	for name := range curr {
		if _, ok := prev[name]; ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	for _, name := range names {
		currDs := curr[name]
		prevDs := prev[name]

		dm := DiskMetrics{Name: name}

		sectorsRead := currDs.SectorsRead - prevDs.SectorsRead
		sectorsWritten := currDs.SectorsWritten - prevDs.SectorsWritten
		dm.ReadBytesPerSec = float64(sectorsRead*sectorSize) / deltaSec
		dm.WriteBytesPerSec = float64(sectorsWritten*sectorSize) / deltaSec

		readsCompleted := currDs.ReadsCompleted - prevDs.ReadsCompleted
		writesCompleted := currDs.WritesCompleted - prevDs.WritesCompleted
		dm.ReadIOPS = float64(readsCompleted) / deltaSec
		dm.WriteIOPS = float64(writesCompleted) / deltaSec

		readTimeDelta := currDs.ReadTimeMs - prevDs.ReadTimeMs
		writeTimeDelta := currDs.WriteTimeMs - prevDs.WriteTimeMs

		if readsCompleted > 0 {
			dm.AvgReadWaitMs = float64(readTimeDelta) / float64(readsCompleted)
		}
		if writesCompleted > 0 {
			dm.AvgWriteWaitMs = float64(writeTimeDelta) / float64(writesCompleted)
		}
		totalIO := readsCompleted + writesCompleted
		totalTime := readTimeDelta + writeTimeDelta
		if totalIO > 0 {
			dm.AvgIOWaitMs = float64(totalTime) / float64(totalIO)
		}

		ioTimeDelta := currDs.IOTimeMs - prevDs.IOTimeMs
		dm.UtilizationPct = (float64(ioTimeDelta) / (deltaSec * 1000.0)) * 100.0
		if dm.UtilizationPct > 100.0 {
			dm.UtilizationPct = 100.0
		}

		dm.IOPending = currDs.IOPending

		metrics = append(metrics, dm)
	}

	return metrics
}

func collectLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		currStats, err := readDiskStats()
		if err != nil {
			log.Printf("failed to read disk stats: %v", err)
			continue
		}
		currTime := time.Now()

		statsMutex.Lock()
		prevStats = currStats
		prevTime = currTime
		statsMutex.Unlock()

		<-ticker.C
		currStats2, err := readDiskStats()
		if err != nil {
			log.Printf("failed to read disk stats (second sample): %v", err)
			continue
		}
		currTime2 := time.Now()
		deltaTime := currTime2.Sub(currTime)

		statsMutex.Lock()
		prevStats = currStats2
		prevTime = currTime2
		statsMutex.Unlock()

		_ = calculateMetrics(currStats2, currStats, deltaTime)
	}
}

func metricsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	currStats, err := readDiskStats()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to read disk stats: %v", err), http.StatusInternalServerError)
		return
	}
	currTime := time.Now()

	statsMutex.RLock()
	pStats := prevStats
	pTime := prevTime
	statsMutex.RUnlock()

	if pStats == nil || len(pStats) == 0 {
		time.Sleep(1 * time.Second)
		currStats2, err := readDiskStats()
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to read disk stats: %v", err), http.StatusInternalServerError)
			return
		}
		currTime2 := time.Now()
		deltaTime := currTime2.Sub(currTime)

		metrics := calculateMetrics(currStats2, currStats, deltaTime)
		response := MetricsResponse{
			Timestamp: currTime2,
			Disks:     metrics,
		}
		json.NewEncoder(w).Encode(response)
		return
	}

	deltaTime := currTime.Sub(pTime)
	metrics := calculateMetrics(currStats, pStats, deltaTime)

	response := MetricsResponse{
		Timestamp: currTime,
		Disks:     metrics,
	}
	json.NewEncoder(w).Encode(response)
}

func main() {
	initialStats, err := readDiskStats()
	if err != nil {
		log.Printf("warning: failed to read initial disk stats: %v", err)
	} else {
		diskNames := make([]string, 0, len(initialStats))
		for name := range initialStats {
			diskNames = append(diskNames, name)
		}
		sort.Strings(diskNames)
		log.Printf("detected %d physical disk(s): %v", len(diskNames), diskNames)

		statsMutex.Lock()
		prevStats = initialStats
		prevTime = time.Now()
		statsMutex.Unlock()
	}

	http.HandleFunc("/api/diskio", metricsHandler)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	log.Printf("disk IO monitor starting on %s", monitorPort)
	log.Printf("metrics endpoint: http://localhost%s/api/diskio", monitorPort)

	if err := http.ListenAndServe(monitorPort, nil); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
