package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	numPollers     = 1
	pollInterval   = 60 * time.Second
	statusInterval = 1 * time.Minute
	errTimeout     = 10 * time.Second
)

var (
	threshold = flag.Uint64("threshold", uint64(1.074*10E11), "Threshold to start serving 500's over HTTP")
	addr      = flag.String("addr", ":8080", "Listen address for HTTP")
	path      = flag.String("path", "/mnt/storage", "The path to query for disk usage")
	override  = flag.Bool("override", false, "Boolean to override check response for HTTP handler")
)

// State represents the last-known state of a path
// This is sent around between the Poller & StateMonitor's channels.
type State struct {
	path  string
	bytes uint64
}

// Disk status storage, with an RWMutex for safe read/write access
// across multiple goroutines
type diskStatus struct {
	state map[string]uint64
	sync.RWMutex
}

// StateMonitor maintains a map that stores the disk usage for paths
// being
// polled, and prints the current state every updateInterval
// nanoseconds.
// It returns a chan State to which resource state should be sent.
// It also serves a HTTP Handler Func for threshold checking. It's
// probably doing too many things! :D
func StateMonitor(updateInterval time.Duration) chan<- State {
	updates := make(chan State)
	diskStatus := &diskStatus{state: make(map[string]uint64)}
	ticker := time.NewTicker(updateInterval)
	go func() {
		for {
			select {
			case <-ticker.C:
				logState(diskStatus)
			case s := <-updates:
				// Write lock
				diskStatus.Lock()
				diskStatus.state[s.path] = s.bytes
				diskStatus.Unlock()
			}
		}
	}()
	go func() {
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			// Read lock
			diskStatus.RLock()
			defer diskStatus.RUnlock()
			bytes := diskStatus.state[*path]
			switch {
			case bytes == 0:
				http.Error(w, "Disk status not cached yet", http.StatusServiceUnavailable)
			case bytes > *threshold && *override == false:
				err := fmt.Sprintf("ERROR: Bytes exceed threshold (%v/%v)", bytes, *threshold)
				http.Error(w, err, http.StatusInternalServerError)
			default:
				fmt.Fprintf(w, "OK: %v is %v bytes; override set to %v\n", *path, bytes, *override)
			}
		})
		if err := http.ListenAndServe(*addr, nil); err != nil {
			log.Fatal("ListenAndServe failed: ", err)
		}
	}()
	return updates
}

// logState prints a state map.
func logState(ds *diskStatus) {
	log.Println("Current state:")
	// Read Lock
	ds.RLock()
	defer ds.RUnlock()
	for k, v := range ds.state {
		log.Printf(" %s %v", k, v)
	}
}

// Path represents a filesystem directory to be polled with du and a
// count of errors when interacting with said path
type Path struct {
	path     string
	errCount int
}

// Poll executes du for a path
// and returns the disk usage in bytes or an error string
func (r *Path) Poll() (bytes uint64) {
	out, err := exec.Command("du", "-sbx", r.path).Output()
	if err != nil {
		log.Fatal(err)
		r.errCount++
	}
	// Tidy up the line
	s := string(out)
	s = strings.TrimSpace(s)

	// Parse tabulation
	bytesStr := strings.Split(s, "\t")[0]

	// Parse uint64 from string
	bytes, err = strconv.ParseUint(bytesStr, 0, 64)
	if err != nil {
		log.Fatal(err)
		r.errCount++
	}
	r.errCount = 0
	return bytes
}

// Sleep sleeps for an appropriate interval (dependent on error state)
// before sending the Path to done.
func (r *Path) Sleep(done chan<- *Path) {
	time.Sleep(pollInterval + errTimeout*time.Duration(r.errCount))
	done <- r
}

// Poller pulls paths off the input queue, runs Poll on the paths,
// sends the output along the status channel then sends the path to
// the complete channel
func Poller(in <-chan *Path, out chan<- *Path, status chan<- State) {
	for r := range in {
		bytes := r.Poll()
		status <- State{r.path, bytes}
		out <- r
	}
}

func main() {
	// parse CLI flags
	flag.Parse()

	// Create our input and output channels.
	pending := make(chan *Path)
	complete := make(chan *Path)

	// Launch the StateMonitor.
	status := StateMonitor(statusInterval)
	log.Println("State Monitor started")

	// Launch some Poller goroutines.
	for i := 0; i < numPollers; i++ {
		go Poller(pending, complete, status)
	}

	// Send the path flag to the pending queue
	go func() {
		pending <- &Path{path: *path}
	}()

	for r := range complete {
		go r.Sleep(pending)
	}

}
